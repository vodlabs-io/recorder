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

func (w *worker) logFinalStats(sessionStart time.Time) {
	w.logger.Info("Session stats",
		"total_bytes", w.output.Stats.TotalBytes,
		"duration_ms", time.Since(sessionStart).Milliseconds(),
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
	w.requestFreshStreams()
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case <-w.notifyCh:
	case <-time.After(streamRefreshWait):
		return "", errors.New("no fresh stream URLs arrived in time")
	}
	str := w.str.Load()
	if str == nil || str.Status != streamer.StatusLive {
		return "", errors.New("streamer is no longer live")
	}
	selected := str.Streams.Select(w.config.Variant)
	if selected == nil {
		return "", errors.New("no stream variant to record")
	}
	w.selectedStream = selected
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
// .ts→.mp4 conversion; HLS parts feed the group directly. Closing a group that
// reached a split limit is the run loop's job, since it happens whether or not
// this part was kept.
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
// creation order.
func (w *worker) emitGroup(g *partGroup, sessionEnd bool) {
	if len(g.stems) == 0 {
		return
	}
	w.groupsMu.Lock()
	sole := w.groupSeq == 1
	w.groupsMu.Unlock()

	outName := fmt.Sprintf("vod%d.mp4", g.index)
	if sessionEnd && sole {
		outName = "vod.mp4"
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
	w.output.SessionState = recording.SessionFinished

	// One last reading, so whatever arrived as the session was winding down is
	// in the sidecar the folder ships with.
	w.observeMetadata()

	if w.output.Stats.TotalBytes >= recording.MinPartBytes {
		stats := w.Tracker.GetStats(w.SessionID)
		stats.Speed = w.Tracker.GetOverallAverageSpeed()
		w.output.Stats = stats

		// Make sure speed in KV is overall average speed, not 0.0
		vlstate.PutSessionStats(context.Background(), w.kv, w.strKey, stats)
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

	if w.sessionLogFile != nil {
		_ = w.sessionLogFile.Close()
		w.sessionLogFile = nil
	}
}
