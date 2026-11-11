package session

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/vodlabs-io/recorder/internal/agora"
	"github.com/vodlabs-io/recorder/internal/cookiemint"
	"github.com/vodlabs-io/recorder/internal/flvfix"
	"github.com/vodlabs-io/recorder/internal/platformauth"
	"github.com/vodlabs-io/recorder/internal/progress"
	"github.com/vodlabs-io/recorder/internal/recording"
	"github.com/vodlabs-io/recorder/internal/stream"
	"github.com/vodlabs-io/recorder/internal/streamer"
	"github.com/vodlabs-io/recorder/internal/vlconfig"
	"github.com/vodlabs-io/recorder/internal/vlstate"
	"github.com/vodlabs-io/recorder/internal/vodmeta"
	"github.com/vodlabs-io/recorder/internal/writer"
)

// worker owns a Session for a specific streamer
type worker struct {
	config              vlconfig.SessionConfig
	logger              *slog.Logger
	sessionLogFile      *os.File
	writer              writer.Writer
	writerCfg           *writer.Config
	carryover           *writer.Carryover // writer state spanning the session's parts
	basicHTTPClient     *http.Client
	flvHTTPClient       *http.Client
	selectedHTTPClient  *http.Client
	cookies             *platformauth.Store // Local platform logins (PRIVACY: for stream-download auth only, never uploaded)
	SessionID           string              // Unique ID for this Session
	strKey              streamer.StreamerKey
	str                 atomic.Pointer[streamer.StreamerState]
	output              recording.Output
	Tracker             *progress.Tracker
	kv                  jetstream.KeyValue
	store               *vlstate.Store
	selectedStream      *stream.Stream
	curPart             writer.PartSpec // Layout of the part currently recording, as the writer laid it out
	recAttempts         int             // Total recording attempts across all parts
	notifyCh            chan struct{}   // Channel to receive notifications of streamer changes
	metaCh              chan struct{}   // Channel to receive notifications for the metadata log
	LagReportCh         chan<- streamer.StreamerKey
	streamReqCh         chan<- streamer.StreamerRef
	pendingRecordingsCh chan<- recording.Job
	cancelMu            sync.RWMutex
	cancel              context.CancelFunc
	cancelOnce          sync.Once
	// Split limits resolved at session start. Zero = disabled.
	splitSize     int64
	splitDuration time.Duration
	// splitPending holds the config key of the group a split limit fired
	// against, or nil when no split is pending.
	splitPending atomic.Pointer[string]
	// Group state. The worker is authoritative for grouping and vodN
	// numbering: a group is a contiguous run of parts sharing one codec
	// configuration (parts with equal keys are stream-copy compatible), so
	// every group covers one unbroken stretch of the broadcast and the
	// numbered outputs replay in recording order. Groups close on a config
	// change, on split limits and at session end, each emitting a
	// self-contained concat+move message. A steady stream keeps one group for
	// the whole session (vod.mp4).
	curStem    string // stem of the part currently recording (e.g. "pt1_best")
	curPartKey string // codec config key reported for the current part
	groupsMu   sync.Mutex
	group      *partGroup // the open run, nil until the first part completes
	groupSeq   int        // groups ever created; drives vodN naming
	// pubOutDir is output.BaseOutDir republished for cross-goroutine readers
	// (Registry.ActiveSessionDirs); output itself is only touched by the
	// worker goroutine.
	pubOutDir atomic.Pointer[string]
	// lastByteAt is when the session last committed recorded bytes, as Unix
	// nanoseconds. Shared across parts, since a gap can straddle a boundary.
	lastByteAt atomic.Int64
	// streamsTTL is how long the selected stream URL stays usable, as the
	// platform reported it. Zero means it outlives the session.
	streamsTTL time.Duration
	// metaLog archives what the platform said about the broadcast, as the
	// session sees it change.
	metaLog *vodmeta.Log
	// started is when the session began: the recording clock's anchor, which a
	// resumed session keeps.
	started time.Time
	// stop holds the stopReason a cancelled session was stopped for.
	stop atomic.Int32
	// resume keeps the session's checkpoint; nil disables resuming.
	resume       *resumeStore
	resumeWindow time.Duration
	// interrupted is set by Run when the session stops while its stream is
	// live and could resume; finishSession then keeps it instead.
	interrupted bool
	// priorLastByte is when a resumed session last recorded before it stopped.
	priorLastByte time.Time
	// afterConfigChange marks the next part as starting on a codec change.
	afterConfigChange bool
	// endedAt is when a session finished from its checkpoint last recorded;
	// zero for a session that runs to its end.
	endedAt time.Time
	// agoraCodec, once agoraCodecSet, is the codec (as agora names it) the
	// session joins an Agora channel in, whatever the stream URL says: the one
	// its broadcaster was found to send, or its writer's.
	agoraCodec    string
	agoraCodecSet bool
	// codecMismatched marks a last part that ended on a codec mismatch.
	codecMismatched bool
	// restart asks for a fresh check once the session has ended, so a new
	// session takes over the broadcast this one could not record.
	restart bool
}

// stopWith cancels the session for reason.
func (w *worker) stopWith(reason stopReason) {
	w.stop.Store(int32(reason))
	w.cancelSession()
}

func (w *worker) stopReason() stopReason {
	return stopReason(w.stop.Load())
}

// resumable reports whether a cancelled session can be interrupted rather
// than finished: it kept something, its stream was live when it stopped, and
// it was not stopped because its streamer is gone.
func (w *worker) resumable() bool {
	if w.resume == nil || w.resumeWindow <= 0 || w.stopReason() == stopRemoved || len(w.output.Parts) == 0 {
		return false
	}
	str := w.str.Load()
	return str != nil && (str.Status == streamer.StatusLive || str.Status == streamer.StatusPaused)
}

// checkpoint captures the session as it stands. Called from the run loop
// between parts, or on a worker that is not running.
func (w *worker) checkpoint(current *currentPart) *checkpoint {
	cp := &checkpoint{
		Platform:      w.strKey.Platform,
		Username:      w.strKey.Username,
		SessionID:     w.SessionID,
		BaseOutDir:    w.output.BaseOutDir,
		Writer:        w.output.WriterName,
		Variant:       w.config.Variant,
		SplitSize:     w.config.SplitSize,
		SplitDuration: w.config.SplitDuration,
		Sanitize:      w.config.SanitizeFLV,
		Started:       w.started,
		LastByteAt:    w.lastByteTime(),
		TotalBytes:    w.output.Stats.TotalBytes,
		Parts:         append([]recording.Part(nil), w.output.Parts...),
		PartIndex:     w.output.PartIndex,
		Current:       current,
		HLSTail:       w.carryover.HLSTail(),
	}
	if cp.LastByteAt.IsZero() {
		cp.LastByteAt = w.priorLastByte
	}
	if str := w.str.Load(); str != nil {
		cp.LiveStart = str.LastLiveStart
	}
	w.groupsMu.Lock()
	cp.GroupSeq = w.groupSeq
	if g := w.group; g != nil {
		cp.Group = &groupState{
			Index: g.index,
			Key:   g.key,
			Stems: append([]string(nil), g.stems...),
			Bytes: g.bytes,
			Start: g.start,
		}
	}
	w.groupsMu.Unlock()
	return cp
}

// saveCheckpoint writes the session's checkpoint, naming current as the part
// in progress (nil between parts).
func (w *worker) saveCheckpoint(current *currentPart) {
	if w.resume == nil || w.SessionID == "" {
		return
	}
	if err := w.resume.save(w.checkpoint(current)); err != nil {
		w.logger.Warn("Failed to save the session checkpoint", "error", err)
	}
}

// restore loads an interrupted session into the worker: its identity, folder,
// settings, parts, groups and clock.
func (w *worker) restore(cp *checkpoint) {
	w.strKey = cp.key()
	w.SessionID = cp.SessionID
	w.output.SessionID = cp.SessionID
	w.output.Platform = cp.Platform
	w.output.Username = cp.Username
	w.output.BaseOutDir = cp.BaseOutDir
	w.output.WriterName = cp.Writer
	if wr, ok := writer.Get(cp.Writer); ok {
		w.writer = wr
	}
	// The parts so far fix the format, and with it the codec to join in.
	switch cp.Writer {
	case "webmwriter":
		w.agoraCodec, w.agoraCodecSet = agora.CodecVP8, true
	case "flvwriter":
		w.agoraCodec, w.agoraCodecSet = "", true
	}
	w.output.Variant = cp.Variant
	w.output.Parts = append([]recording.Part(nil), cp.Parts...)
	w.output.PartIndex = cp.PartIndex
	w.output.CurrentPart = recording.Part{Index: cp.PartIndex}
	w.output.Stats.TotalBytes = cp.TotalBytes
	w.config.Variant = cp.Variant
	w.config.SplitSize = cp.SplitSize
	w.config.SplitDuration = cp.SplitDuration
	w.config.SanitizeFLV = cp.Sanitize
	w.started = cp.Started
	w.Tracker = progress.NewTrackerFrom(cp.Started, cp.TotalBytes)
	w.priorLastByte = cp.lastByte()
	w.carryover = writer.NewCarryover()
	w.carryover.SeedHLSTail(cp.HLSTail)
	w.groupsMu.Lock()
	w.groupSeq = cp.GroupSeq
	if g := cp.Group; g != nil {
		w.group = &partGroup{
			index: g.Index,
			key:   g.Key,
			stems: append([]string(nil), g.Stems...),
			bytes: g.Bytes,
			start: g.Start,
		}
		w.curPartKey = g.Key
	}
	w.groupsMu.Unlock()
}

// foldCrashedPart keeps the part a crashed daemon was recording, measured from
// disk, as the run loop would have kept it had the part ended normally.
func (w *worker) foldCrashedPart(cur *currentPart) {
	if cur == nil {
		return
	}
	size, mod := measurePart(cur.Path)
	if mod.After(w.priorLastByte) {
		w.priorLastByte = mod
	}
	if w.output.WriterName == "hlswriter" {
		// What the resumed part must not record again.
		w.carryover.SeedHLSTail(hlsPartHashes(cur.Path))
	}
	w.output.PartIndex = cur.Index + 1
	defer func() { w.output.CurrentPart = recording.Part{Index: w.output.PartIndex} }()
	if size < recording.MinPartBytes {
		if wr, ok := writer.Get(w.output.WriterName); ok {
			_ = wr.DiscardPart(writer.PartSpec{Path: cur.Path})
		}
		return
	}
	part := recording.Part{Index: cur.Index, Bytes: size, StartTime: cur.Start, EndTime: mod, Path: cur.Path}
	w.output.CurrentPart = part
	w.output.Parts = append(w.output.Parts, part)
	w.output.Stats.TotalBytes += size
	w.curStem = cur.Stem
	key := w.crashedPartKey(cur)
	w.groupsMu.Lock()
	w.curPartKey = key
	w.groupsMu.Unlock()
	w.recordGroupPart()
	w.logger.Info("Kept the part a crashed recorder was writing",
		"part_index", cur.Index, "part_bytes", size, "part_path", cur.Path)
}

// crashedPartKey works out the codec configuration key of a part whose writer
// never reported one. An FLV part's is read back from its file. Otherwise a
// part that did not start on a codec change continues its group's.
func (w *worker) crashedPartKey(cur *currentPart) string {
	if w.output.WriterName == "flvwriter" && w.config.SanitizeFLV {
		if f, err := os.Open(cur.Path); err == nil {
			key := flvfix.ConfigKeyOf(f, 16<<20)
			f.Close()
			if key != "" {
				return key
			}
		}
	}
	if cur.AfterConfigChange {
		return "crashed:" + cur.Stem
	}
	w.groupsMu.Lock()
	defer w.groupsMu.Unlock()
	if w.group != nil {
		return w.group.key
	}
	return w.curPartKey
}

// closeLog closes the session log.
func (w *worker) closeLog() {
	if w.sessionLogFile != nil {
		_ = w.sessionLogFile.Close()
		w.sessionLogFile = nil
	}
}

// interruptSession keeps a stopped session for a resume: the checkpoint is
// marked interrupted and handed to the store, and nothing is sent for
// processing.
func (w *worker) interruptSession() error {
	w.observeMetadata()
	cp := w.checkpoint(nil)
	cp.InterruptedAt = time.Now()
	cp.Reason = w.stopReason().String()
	if err := w.resume.hold(cp); err != nil {
		return err
	}
	if w.kv != nil && w.output.Stats.TotalBytes > 0 {
		stats := w.Tracker.GetStats(w.SessionID)
		stats.Speed = w.Tracker.GetOverallAverageSpeed()
		vlstate.PutSessionStats(context.Background(), w.kv, w.strKey, stats)
	}
	w.logger.Info("Session interrupted; it resumes into the same VOD if recording restarts in time",
		"reason", cp.Reason,
		"resume_by", cp.resumeBy(w.resumeWindow).Format(time.RFC3339),
		"total_bytes", cp.TotalBytes)
	w.closeLog()
	return nil
}

// runMetadataLog archives what the platform says about the broadcast, for as
// long as the session runs.
func (w *worker) runMetadataLog(done <-chan struct{}) {
	w.observeMetadata()
	for {
		select {
		case <-done:
			return
		case <-w.metaCh:
			w.observeMetadata()
		}
	}
}

// observeMetadata offers the platform's current description of the broadcast to
// the session's metadata log. Repeat readings are dropped there, so calling it
// for every update costs nothing — most updates say nothing new.
func (w *worker) observeMetadata() {
	str := w.str.Load()
	if str == nil {
		return
	}
	recorded, err := w.metaLog.Observe(str.LiveMetadata, time.Now())
	if err != nil {
		w.logger.Warn("Failed to write session metadata", "error", err)
	}
	if recorded {
		w.logger.Info("Recorded live metadata", str.LiveMetadata.Attrs()...)
	}
}

// OutDir returns the session's output folder, or "" before it is created.
// Safe to call from any goroutine.
func (w *worker) OutDir() string {
	if p := w.pubOutDir.Load(); p != nil {
		return *p
	}
	return ""
}

// progressBufSize is the per-part progress channel's buffer. Writers report
// with a non-blocking send and drop on a full buffer, so this only needs to
// absorb a burst between the listener's reads.
const progressBufSize = 100

// partGroup accumulates a contiguous run of completed parts sharing one codec
// configuration.
type partGroup struct {
	index int    // creation order, 1-based ("vod{index}.mp4")
	key   string // codec config key every part in the run shares
	stems []string
	bytes int64
	start time.Time // start of the group's first part (split_duration clock)
}

// newWorker creates and initializes a new session worker
func newWorker(
	cfg vlconfig.SessionConfig,
	kv jetstream.KeyValue,
	store *vlstate.Store,
	lagReportCh chan<- streamer.StreamerKey,
	streamReqCh chan<- streamer.StreamerRef,
	pendingRecordingsCh chan<- recording.Job,
	basicHttpClient, flvHTTPClient *http.Client,
	cookies *platformauth.Store,
) *worker {
	worker := &worker{
		config: cfg,
		output: recording.Output{
			CurrentPart: recording.Part{Index: 1},
			PartIndex:   1,
			Variant:     cfg.Variant,
		},
		basicHTTPClient:     basicHttpClient,
		flvHTTPClient:       flvHTTPClient,
		cookies:             cookies,
		kv:                  kv,
		store:               store,
		Tracker:             progress.NewTracker(),
		notifyCh:            make(chan struct{}, 1),
		metaCh:              make(chan struct{}, 1),
		LagReportCh:         lagReportCh,
		streamReqCh:         streamReqCh,
		pendingRecordingsCh: pendingRecordingsCh,
	}
	return worker
}

func (w *worker) setCancel(cancel context.CancelFunc) {
	w.cancelMu.Lock()
	w.cancel = cancel
	w.cancelMu.Unlock()
}

func (w *worker) cancelSession() {
	w.cancelOnce.Do(func() {
		w.cancelMu.RLock()
		cancel := w.cancel
		w.cancelMu.RUnlock()
		if cancel != nil {
			cancel()
		}
	})
}

func (w *worker) logFinalStats() {
	w.logger.Info("Session stats",
		"total_bytes", w.output.Stats.TotalBytes,
		"duration_ms", time.Since(w.started).Milliseconds(),
		"speed", w.Tracker.GetOverallAverageSpeed())
}

func (w *worker) reportLag() {
	select {
	case w.LagReportCh <- w.strKey:
	default:
		w.logger.Debug("Lag report channel is full, skipping report")
	}
}

// requestFreshStreams asks the cloud to re-resolve this streamer, which
// re-signs the stream URLs from this machine, since some platforms sign a media
// URL for only a few tens of seconds. The reply arrives as an ordinary status
// update through notifyCh. Re-signs are paced apart from status checks.
func (w *worker) requestFreshStreams() {
	str := w.str.Load()
	if str == nil {
		return
	}
	select {
	case w.streamReqCh <- str.StreamerRef:
	default:
		w.logger.Debug("Status request channel is full, skipping stream refresh")
	}
}

// streamRefreshWait is how long a re-resolve is waited for. Some platforms sign
// a URL for only a few tens of seconds, so a reply that arrives later than this
// is worth less than getting on with the attempt.
const streamRefreshWait = 10 * time.Second

// refreshStreamURL re-resolves the streamer and returns the newly signed URL
// for the variant being recorded.
//
// This runs while the writer is mid-part, on the run loop's own goroutine —
// Record is called synchronously, so nothing else is reading notifyCh — which
// is what lets an Origin whose grant expires mid-recording carry on through the
// swap instead of ending the part and leaving a hole in it.
func (w *worker) refreshStreamURL(ctx context.Context) (string, error) {
	before := w.str.Load()
	w.requestFreshStreams()
	timeout := time.After(streamRefreshWait)
	// A notification can predate the request: nothing reads notifyCh while a
	// part records, so an earlier update leaves one behind. Only a new record
	// answers this request.
	for w.str.Load() == before {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-w.notifyCh:
		case <-timeout:
			return "", errors.New("no fresh stream URLs arrived in time")
		}
	}
	str := w.str.Load()
	if str == nil || str.Status != streamer.StatusLive {
		return "", errors.New("streamer is no longer live")
	}
	selected := w.inSessionCodec(str.Streams.Select(w.config.Variant))
	if selected == nil {
		return "", errors.New("no stream variant to record")
	}
	w.selectedStream = selected
	cookiemint.Renew(w.selectedHTTPClient, str.CookieMint)
	return selected.URL, nil
}

func (w *worker) lastRecordingWasEmpty() bool {
	return w.recAttempts > 0 && w.output.CurrentPart.Bytes == 0
}

// lastByteTime returns when the session last committed recorded bytes, or the
// zero time before any have.
func (w *worker) lastByteTime() time.Time {
	ns := w.lastByteAt.Load()
	if ns == 0 {
		return time.Time{}
	}
	return time.Unix(0, ns)
}

// dataGap reports how long the recording has gone without bytes landing on
// disk. since anchors the gap until the first byte arrives — part start for
// the watchdog, session start for the run loop.
func (w *worker) dataGap(since time.Time) time.Duration {
	if t := w.lastByteTime(); t.After(since) {
		since = t
	}
	return time.Since(since)
}

// addJitter adds a random jitter to the given duration, up to maxJitter.
func addJitter(d, maxJitter time.Duration) time.Duration {
	return d + time.Duration(rand.Int63n(int64(maxJitter)))
}

// activeGroupStats returns the config key, accumulated bytes and start time of
// the open group — the run the current part extends unless its config turns out
// to differ, in which case it opens a group of its own and the split check ran
// against a slightly stale base. The returned key identifies which group that
// base came from. Nothing is open until the first part completes, so the
// caller's fallback anchors the split_duration clock until then.
func (w *worker) activeGroupStats(fallbackStart time.Time) (string, int64, time.Time) {
	w.groupsMu.Lock()
	defer w.groupsMu.Unlock()
	if w.group == nil {
		return w.curPartKey, 0, fallbackStart
	}
	return w.group.key, w.group.bytes, w.group.start
}

// stallTimeout is how long a part may go without a recorded byte before it is
// abandoned, and the same gap at which the run loop calls the session lagging.
const stallTimeout = 30 * time.Second

// listenProgress accumulates the writer's byte reports into the current part
// and enforces the stall watchdog and split limits. It owns progressCh for one
// part: closure — not recordCtx — is its exit signal, so it drains every
// increment the writer sent before the run loop reads CurrentPart.Bytes.
//
// The split limit is evaluated over the whole open group, not just the current
// part; partStart anchors the split_duration clock while the part's group
// doesn't exist yet.
func (w *worker) listenProgress(
	recordCtx context.Context,
	cancelRecord context.CancelFunc,
	partStart time.Time,
	progressCh <-chan int64,
	done chan<- struct{},
) {
	defer close(done)

	sampleTicker := time.NewTicker(250 * time.Millisecond)
	defer sampleTicker.Stop()
	dataReceived := false
	// stopping suppresses further sampling once the part is being torn down,
	// while the loop stays alive to drain the writer's remaining increments.
	stopping := false
	for {
		select {
		case <-sampleTicker.C:
			if stopping || recordCtx.Err() != nil {
				continue
			}
			w.Tracker.AddSample()
			stats := w.Tracker.GetStats(w.SessionID)
			vlstate.PutSessionStats(recordCtx, w.kv, w.strKey, stats)
			// The gap since the last byte, not the tracker's speed: speed is a
			// five-second window, and an HLS origin delivers a whole segment at
			// once, so it reads zero between segments longer than that window.
			if gap := w.dataGap(partStart); gap > stallTimeout {
				w.logger.Info("No data received, stopping part",
					"idle", gap.Round(time.Second))
				stopping = true
				cancelRecord()
			}
			// Check split limits on the ticker so we don't need to re-check on
			// every byte increment. CurrentPart.Bytes is owned by this goroutine;
			// the limit is cumulative over the open group so a run of small
			// reconnect-parts still closes the group at the configured size/age.
			groupKey, groupBytesBase, groupStart := w.activeGroupStats(partStart)
			if (w.splitSize > 0 && groupBytesBase+w.output.CurrentPart.Bytes >= w.splitSize) ||
				(w.splitDuration > 0 && time.Since(groupStart) >= w.splitDuration) {
				// Record which group tripped: the finishing part may join a
				// different one if the codec config just changed.
				w.splitPending.Store(&groupKey)
				w.logger.Info("Split limit reached, closing group",
					"part_index", w.output.CurrentPart.Index,
					"group_bytes", groupBytesBase+w.output.CurrentPart.Bytes,
					"group_elapsed", time.Since(groupStart).Round(time.Second))
				stopping = true
				cancelRecord()
			}
		case bytesIncrement, ok := <-progressCh:
			if !ok {
				// Closed by the run loop after Record returned: the part is
				// complete and every reported byte has been counted.
				return
			}
			w.output.CurrentPart.Bytes += bytesIncrement
			w.output.Stats.TotalBytes += bytesIncrement
			w.Tracker.UpdateBytes(bytesIncrement)
			w.lastByteAt.Store(time.Now().UnixNano())
			if !dataReceived {
				dataReceived = true
				w.store.SetSessionState(recordCtx, w.strKey, w.SessionID, recording.SessionRecording)
			}
		}
	}
}

// deleteEmptyPart removes the output of the current part when no data was
// written. What that means is the writer's business: a file for one, a
// directory for another, nothing at all for a writer whose output is shared
// across the parts of a session.
func (w *worker) deleteEmptyPart() error {
	if err := w.writer.DiscardPart(w.curPart); err != nil {
		return err
	}
	w.logger.Info("Deleted empty part", "path", w.curPart.Path)
	return nil
}

// recordGroupPart extends the open run with the just-completed part, or closes
// that run and starts a new one when the part's codec configuration differs.
// Keeping a group contiguous is what lets the numbered outputs be replayed in
// order: a group only ever spans one stretch of the broadcast, so concatenating
// vod1..vodN reproduces the session's timeline. FLV parts are sent for eager
// .ts→.mp4 conversion; WebM and HLS parts feed the group directly. Closing a
// group that reached a split limit is the run loop's job, since it happens
// whether or not this part was kept.
func (w *worker) recordGroupPart() {
	key := w.curPartKey
	w.groupsMu.Lock()
	var rotated *partGroup
	if w.group != nil && w.group.key != key {
		rotated, w.group = w.group, nil
	}
	if w.group == nil {
		w.groupSeq++
		w.group = &partGroup{index: w.groupSeq, key: key, start: w.output.CurrentPart.StartTime}
	}
	w.group.stems = append(w.group.stems, w.curStem)
	w.group.bytes += w.output.CurrentPart.Bytes
	w.groupsMu.Unlock()

	// Emit the run this part ended ahead of its own conversion, so processing
	// receives the session in recording order.
	if rotated != nil {
		w.emitGroup(rotated, false)
	}

	if w.writer.Name() == "flvwriter" {
		w.pendingRecordingsCh <- recording.Job{Output: w.output, Kind: recording.JobPart}
		w.logger.Info("Sent part for conversion",
			"part_index", w.output.CurrentPart.Index,
			"part_bytes", w.output.CurrentPart.Bytes,
			"part_path", w.output.CurrentPart.Path)
	}
}

// closeGroup emits the open group as a self-contained concat+move job when key
// names it; the next part opens a fresh group (with a fresh split clock). A
// split limit is attributed to whichever group was open when it fired, and a
// config change may have closed that group since — the key guard makes the call
// a no-op then, rather than cutting the new run short.
func (w *worker) closeGroup(key string, sessionEnd bool) {
	w.groupsMu.Lock()
	g := w.group
	if g == nil || g.key != key {
		w.groupsMu.Unlock()
		return
	}
	w.group = nil
	w.groupsMu.Unlock()
	w.emitGroup(g, sessionEnd)
}

// emitGroup hands a closed group to processing. The sole group of a whole
// session is the undecorated vod.mp4; every other group is numbered by
// creation order. A webmwriter session's groups are .webm.
func (w *worker) emitGroup(g *partGroup, sessionEnd bool) {
	if len(g.stems) == 0 {
		return
	}
	w.groupsMu.Lock()
	sole := w.groupSeq == 1
	w.groupsMu.Unlock()

	ext := ".mp4"
	if w.output.WriterName == "webmwriter" {
		ext = ".webm"
	}
	outName := fmt.Sprintf("vod%d%s", g.index, ext)
	if sessionEnd && sole {
		outName = "vod" + ext
	}

	w.pendingRecordingsCh <- recording.Job{
		Output:      w.output,
		Kind:        recording.JobGroup,
		GroupInputs: g.stems,
		GroupOutput: outName,
	}

	w.logger.Info("Closed group",
		"output", outName,
		"part_count", len(g.stems),
		"group_bytes", g.bytes)
}

func (w *worker) finishSession() {
	if w.interrupted {
		err := w.interruptSession()
		if err == nil {
			return
		}
		w.interrupted = false
		w.logger.Warn("Could not keep the session for a resume; finishing it", "error", err)
	}
	w.output.SessionState = recording.SessionFinished

	// One last reading, so whatever arrived as the session was winding down is
	// in the sidecar the folder ships with.
	w.observeMetadata()

	if w.output.Stats.TotalBytes >= recording.MinPartBytes {
		stats := w.Tracker.GetStats(w.SessionID)
		stats.Speed = w.Tracker.GetOverallAverageSpeed()
		if !w.endedAt.IsZero() {
			// Finished from its checkpoint: the session ended when it last
			// recorded, not now.
			stats.Duration = w.endedAt.Sub(w.started)
			if s := stats.Duration.Seconds(); s > 0 {
				stats.Speed = float64(stats.TotalBytes) / s
			}
		}
		w.output.Stats = stats

		// Make sure speed in KV is overall average speed, not 0.0
		if w.kv != nil {
			vlstate.PutSessionStats(context.Background(), w.kv, w.strKey, stats)
		}
	}

	// Flush the run still open. A session that only ever had one group
	// produces the undecorated vod.mp4.
	w.groupsMu.Lock()
	g := w.group
	w.group = nil
	w.groupsMu.Unlock()
	if g != nil {
		w.emitGroup(g, true)
	}

	// Session-end bookkeeping: state, raw-part cleanup and the logs sweep.
	w.pendingRecordingsCh <- recording.Job{Output: w.output, Kind: recording.JobFinalize}

	w.logger.Info("Sent final Recording info to processing service",
		"total_bytes", w.output.Stats.TotalBytes,
		"base_out_dir", w.output.BaseOutDir)
	if w.resume != nil {
		w.resume.remove(w.SessionID)
	}

	if w.sessionLogFile != nil {
		_ = w.sessionLogFile.Close()
		w.sessionLogFile = nil
	}
}
