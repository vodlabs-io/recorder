// Package flvfix sanitizes live FLV byte streams so the recorded file remuxes
// to a clean, seekable MP4 with -c copy. It is a pure streaming filter: feed it
// arbitrarily chunked bytes, it emits only whole, structurally valid tags on a
// continuous monotonic timeline.
//
// It corrects the pathologies observed in TikTok live pulls:
//   - in-band stream restarts (a full FLV file header injected mid-stream)
//   - replayed/overlapping segments (backward timestamp jumps)
//   - timeline gaps (forward jumps when the uplink stalls)
//   - implausible composition times (frames presented far from their decode time)
//   - partial/garbage tags (connection cuts, Origin junk)
//   - re-sent metadata and sequence headers
//
// A genuine codec-configuration change (e.g. the encoder switches resolution)
// cannot be represented in a single stream-copied MP4 track: Write returns
// ErrConfigChange and the caller calls RotateOutput with a fresh sink to
// continue losslessly. The tag carrying the new configuration stays buffered
// until then, so a caller that drops the sanitizer instead loses it — and with
// it the only announcement of the new configuration the stream will make.
package flvfix

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"log/slog"
)

// ErrConfigChange is returned by Write when the stream's codec configuration
// materially changed. The current output is complete and valid; continuing
// requires a new output (see RotateOutput).
var ErrConfigChange = errors.New("flv codec configuration changed")

// errSinkWrite wraps errors from the underlying output writer so callers can
// distinguish disk failures from stream problems.
var errSinkWrite = errors.New("flv sanitizer sink write failed")

// Options tunes sanitizer thresholds. Zero values select the defaults.
type Options struct {
	// GapToleranceMs: a media tag this far ahead of both streams' last output
	// is a timeline gap; the segments are glued together (gap removed).
	GapToleranceMs int64
	// ResetThresholdMs: a backward jump larger than this is a timeline reset
	// (rebase and continue) rather than a replay to deduplicate.
	ResetThresholdMs int64
	// DedupEscalateMs: if a dedup window spans more than this much replayed
	// media time without catching up, escalate to a timeline reset.
	DedupEscalateMs int64
	// JitterClampMs: video tags at most this far backward are clamped
	// monotonic instead of dropped (dropping mid-GOP video forces a keyframe
	// wait; sub-frame jitter after a glue is not worth that).
	JitterClampMs int64
	// MaxTagSize: tags claiming a larger payload are treated as desync.
	MaxTagSize int
	// NoResume makes every RotateOutput start a fresh output, even for a
	// configuration seen before. A caller whose outputs are write-once (the
	// live recorder: each part is converted and concatenated on its own) must
	// set it; resuming would splice into a file it can no longer append to.
	NoResume bool
	// RepairsDisabled keeps the stream's own timeline: gaps are not glued,
	// replays are not deduplicated and backward jumps are not rebased. Tags are
	// still parsed and re-framed, so the output is structurally valid FLV on a
	// monotonic timeline; it just is not repaired.
	RepairsDisabled bool
}

func (o *Options) applyDefaults() {
	if o.GapToleranceMs == 0 {
		o.GapToleranceMs = 3000
	}
	if o.ResetThresholdMs == 0 {
		o.ResetThresholdMs = 60000
	}
	if o.DedupEscalateMs == 0 {
		o.DedupEscalateMs = 45000
	}
	if o.JitterClampMs == 0 {
		o.JitterClampMs = 500
	}
	if o.MaxTagSize == 0 {
		o.MaxTagSize = 8 << 20
	}
}

const (
	streamAudio = 0
	streamVideo = 1

	defaultFrameGapMs = 33

	// maxCompositionMs bounds a plausible composition time: the B-frame reorder
	// delay, a few frames on a live encoder. Around an uplink stall TikTok has
	// sent frames over a minute beyond it; presented that late, a frame makes a
	// re-encode drop every frame behind it until the timeline catches up.
	maxCompositionMs = 1000
)

// Sanitizer filters one logical recording (one output file at a time). It is
// not safe for concurrent use.
type Sanitizer struct {
	out    io.Writer
	logger *slog.Logger
	opts   Options

	buf   []byte
	start int

	sawFileHeader bool
	resync        bool

	// Timeline state. out = in + offset; lastOut is per stream (-1 = none).
	offsetSet bool
	offset    int64
	lastOut   [2]int64
	frameGap  int64 // EMA of accepted video frame spacing (input ms)
	lastVidIn int64 // previous accepted video input ts (-1 = none)
	lastCTS   int32 // composition time of the last plausible video frame
	grid      phaseGrid
	// rawTimestamps disables the output-timestamp grid. Tests that assert exact
	// timestamps set it; there is no way to reach it from a configuration.
	rawTimestamps bool

	// Replay-deduplication window.
	dedupActive  bool
	dedupLowOut  int64 // output-ts of the first dropped tag in the window
	dedupHighOut int64 // output-ts of the latest dropped tag in the window

	videoNeedKeyframe bool

	// Active codec configs: identity form for comparison, full tag bytes
	// (header+payload, no PreviousTagSize) for deferred/rotated emission.
	videoCfgID, videoCfgTag []byte
	audioCfgID, audioCfgTag []byte
	videoCfgEmitted         bool
	audioCfgEmitted         bool

	scriptSeen   bool
	mediaEmitted bool

	// Rotation handshake: the tag that carries the new config stays in the
	// buffer; RotateOutput clears the matching active config and reprocesses.
	pendingRotate  bool
	pendingIsVideo bool
	pendingKey     string

	// Per-config timeline snapshots let RotateOutput resume a previously seen
	// config's output file: the offline repairer appends recurring configs to
	// one file per config instead of one file per configuration *run*
	// (encoders flip-flop between configs constantly on bad uplinks).
	snapshots map[string]timelineSnapshot
	baseOut   int64 // output ts the next part's first media tag anchors to

	eof     bool // Finish called: no more input will arrive
	written int64
	stats   Stats
	err     error // sticky sink error
}

// timelineSnapshot preserves a config's output-timeline position across
// rotations so a recurring config can continue its file seamlessly.
type timelineSnapshot struct {
	lastOut  [2]int64
	frameGap int64
}

// New returns a Sanitizer writing sanitized FLV to out.
func New(out io.Writer, logger *slog.Logger, opts Options) *Sanitizer {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	opts.applyDefaults()
	return &Sanitizer{
		out:       out,
		logger:    logger,
		opts:      opts,
		lastOut:   [2]int64{-1, -1},
		frameGap:  defaultFrameGapMs,
		lastVidIn: -1,
		snapshots: map[string]timelineSnapshot{},
	}
}

// ConfigKey returns a stable opaque key identifying the codec configuration
// of the content emitted to the current output (level-insensitive for AVC).
// Empty until a configuration is known. Outputs with equal keys are
// stream-copy compatible and may be concatenated.
func (s *Sanitizer) ConfigKey() string {
	return configKey(s.videoCfgID, s.audioCfgID)
}

// pendingConfigKey returns the config key the stream switches to after the
// pending ErrConfigChange rotation. Empty when no rotation is pending.
func (s *Sanitizer) pendingConfigKey() string {
	if !s.pendingRotate {
		return ""
	}
	return s.pendingKey
}

// hasResumeState reports whether RotateOutput will resume (append to) an
// earlier output for the pending config rather than starting a fresh one.
// Callers use it to reopen that config's existing file instead of creating a
// new one.
func (s *Sanitizer) hasResumeState() bool {
	if !s.pendingRotate || s.opts.NoResume {
		return false
	}
	_, ok := s.snapshots[s.pendingKey]
	return ok
}

func configKey(videoID, audioID []byte) string {
	if videoID == nil && audioID == nil {
		return ""
	}
	h := fnv.New64a()
	var lenBuf [8]byte
	binary.BigEndian.PutUint64(lenBuf[:], uint64(len(videoID)))
	h.Write(lenBuf[:])
	h.Write(videoID)
	h.Write(audioID)
	return fmt.Sprintf("%016x", h.Sum64())
}

// Written returns the total bytes emitted to the output(s) so far.
func (s *Sanitizer) Written() int64 { return s.written }

// Stats returns a snapshot of the sanitizer's counters.
func (s *Sanitizer) Stats() Stats { return s.stats }

// Write feeds raw stream bytes. It always consumes all of p (buffering any
// incomplete trailing unit) and may return ErrConfigChange, errSinkWrite, or a
// previously sticky error.
func (s *Sanitizer) Write(p []byte) (int, error) {
	if s.err != nil {
		return 0, s.err
	}
	if s.pendingRotate {
		return 0, ErrConfigChange
	}
	s.stats.bytesIn += int64(len(p))
	s.compact()
	s.buf = append(s.buf, p...)
	if err := s.process(); err != nil {
		return len(p), err
	}
	return len(p), nil
}

// Finish signals end of input. Buffered data that only a future byte could
// validate is resolved definitively: resync candidates needing more data are
// rejected, and a trailing partial unit is dropped (nothing partial is ever
// emitted). Returns ErrConfigChange if a config change is still pending; the
// caller may RotateOutput and call Finish again.
func (s *Sanitizer) Finish() error {
	if s.err != nil {
		return s.err
	}
	s.eof = true
	if s.pendingRotate {
		return ErrConfigChange
	}
	err := s.process()
	if n := len(s.buf) - s.start; n > 0 && err == nil && !s.pendingRotate {
		s.stats.truncatedTailBytes += int64(n)
		s.consume(n)
	}
	return err
}

// RotateOutput finishes the config-change handshake begun by an
// ErrConfigChange result: subsequent output goes to next. Content is not
// lost; processing resumes from the tag that carried the new config.
//
// When the new config was seen before (hasResumeState), next should be that
// config's earlier output opened for append: the timeline continues where
// that file left off. Otherwise next is treated as a fresh file and receives
// an FLV header and the new configuration, with the timeline restarting at 0.
func (s *Sanitizer) RotateOutput(next io.Writer) error {
	if s.err != nil {
		return s.err
	}
	if !s.pendingRotate {
		return errors.New("flvfix: no pending config change to rotate")
	}
	// Preserve the outgoing config's timeline so a later flip back can
	// continue its file.
	if key := s.ConfigKey(); key != "" && s.mediaEmitted && !s.opts.NoResume {
		s.snapshots[key] = timelineSnapshot{lastOut: s.lastOut, frameGap: s.frameGap}
	}
	snap, resume := s.snapshots[s.pendingKey]
	if s.opts.NoResume {
		resume = false
	}

	s.out = next
	s.pendingRotate = false
	s.stats.configChanges++

	// The config of the stream that did NOT change carries over; the changed
	// stream's config is re-adopted from the pending tag still in the buffer.
	if s.pendingIsVideo {
		s.videoCfgID, s.videoCfgTag = nil, nil
	} else {
		s.audioCfgID, s.audioCfgTag = nil, nil
	}
	s.offsetSet = false
	s.lastVidIn = -1
	s.endDedup()
	s.videoNeedKeyframe = true
	s.scriptSeen = true // a rotated part has no source script tag; never wait
	s.sawFileHeader = true

	if resume {
		// Appending to the config's earlier output: its header and config
		// tags are already in the file; splice after its last timestamps.
		s.lastOut = snap.lastOut
		s.frameGap = snap.frameGap
		s.baseOut = max(snap.lastOut[streamAudio], snap.lastOut[streamVideo]) + s.frameGap
		s.videoCfgEmitted, s.audioCfgEmitted = true, true
		s.mediaEmitted = true
		return s.process()
	}

	s.lastOut = [2]int64{-1, -1}
	s.baseOut = 0
	s.videoCfgEmitted, s.audioCfgEmitted = false, false
	s.mediaEmitted = false
	if err := s.emitFileHeader(); err != nil {
		return err
	}
	return s.process()
}

// compact reclaims consumed buffer space.
func (s *Sanitizer) compact() {
	if s.start == 0 {
		return
	}
	if s.start == len(s.buf) {
		s.buf = s.buf[:0]
		s.start = 0
		return
	}
	if s.start > 64<<10 {
		n := copy(s.buf, s.buf[s.start:])
		s.buf = s.buf[:n]
		s.start = 0
	}
}

func (s *Sanitizer) consume(n int) {
	s.start += n
}

// process drains as many complete units from the buffer as possible.
func (s *Sanitizer) process() error {
	for {
		if s.err != nil {
			return s.err
		}
		if s.resync && !s.scanResync() {
			return nil // still desynced; need more data
		}
		b := s.buf[s.start:]
		if len(b) < 4 {
			return nil
		}
		if isFileHeader(b) {
			n, ok, valid := s.parseFileHeader(b)
			if !ok {
				return nil // need more data
			}
			if !valid {
				s.enterResync("implausible FLV header")
				continue
			}
			if err := s.acceptFileHeader(); err != nil {
				return err
			}
			s.consume(n)
			continue
		}
		t := b[0]
		if t != tagAudio && t != tagVideo && t != tagScript {
			s.enterResync(fmt.Sprintf("unexpected tag type 0x%02x", t))
			continue
		}
		if len(b) < tagHeaderLen {
			return nil
		}
		size := be24(b[1:4])
		if size == 0 || size > s.opts.MaxTagSize {
			s.enterResync("implausible tag size")
			continue
		}
		need := tagHeaderLen + size + prevSizeLen
		if len(b) < need {
			return nil
		}
		if be32(b[tagHeaderLen+size:need]) != uint32(tagHeaderLen+size) {
			s.enterResync("PreviousTagSize mismatch")
			continue
		}
		s.stats.tagsIn++
		consumed, err := s.handleTag(b[:need])
		if err != nil {
			return err
		}
		if !consumed { // config change: tag stays for RotateOutput
			return ErrConfigChange
		}
		s.consume(need)
	}
}

// parseFileHeader validates an FLV file header at the buffer head. Returns the
// total bytes to consume (header + PreviousTagSize0), whether enough data is
// buffered to decide, and whether the header is plausible.
func (s *Sanitizer) parseFileHeader(b []byte) (n int, ok, valid bool) {
	if len(b) < fileHeaderLen {
		return 0, false, false
	}
	dataOffset := int(be32(b[5:9]))
	if dataOffset < fileHeaderLen || dataOffset > 4096 {
		return 0, true, false
	}
	n = dataOffset + prevSizeLen
	if len(b) < n {
		return 0, false, false
	}
	// PreviousTagSize0 must be zero per spec; anything else means we matched
	// bytes that merely look like a header.
	if be32(b[dataOffset:n]) != 0 {
		return 0, true, false
	}
	return n, true, true
}

func (s *Sanitizer) acceptFileHeader() error {
	if !s.sawFileHeader {
		s.sawFileHeader = true
		return s.emitFileHeader()
	}
	// In-band restart: swallow the header; the encoder restarted, so video
	// resumes only at the next keyframe.
	s.stats.restartsConsumed++
	s.videoNeedKeyframe = true
	s.logger.Debug("Consumed in-band FLV stream restart",
		"restarts", s.stats.restartsConsumed)
	return nil
}

// emitFileHeader writes a canonical 13-byte FLV header (audio+video flags).
func (s *Sanitizer) emitFileHeader() error {
	hdr := [fileHeaderLen + prevSizeLen]byte{'F', 'L', 'V', 0x01, 0x05, 0, 0, 0, 9, 0, 0, 0, 0}
	return s.emit(hdr[:])
}

// enterResync drops the current byte and begins scanning for the next valid
// boundary. Any video emitted after recovery waits for a keyframe.
func (s *Sanitizer) enterResync(reason string) {
	if !s.resync {
		s.resync = true
		s.stats.desyncs++
		s.videoNeedKeyframe = true
		s.logger.Info("FLV stream desync, scanning for next boundary", "reason", reason)
	}
	s.consume(1)
	s.stats.desyncBytesDrop++
}

// scanResync searches the buffer for the next plausible boundary: an FLV file
// header, or a tag whose framing checks out (trailing PreviousTagSize matches
// and, when buffered, the following unit also starts plausibly). Definitively
// bad prefixes are consumed. A candidate that needs bytes not yet buffered
// pauses the scan until more data arrives — unless Finish was called, in which
// case no more data can arrive and the candidate is rejected. Returns true
// once resynchronized.
func (s *Sanitizer) scanResync() bool {
	b := s.buf[s.start:]
	i := 0
	found := false
	for i < len(b) {
		c := b[i:]
		if len(c) < 4 {
			if !s.eof {
				break // could be the start of a header/tag; wait for more
			}
			i++
			continue
		}
		if isFileHeader(c) {
			found = true // header path validates the rest
			break
		}
		t := c[0]
		if t == tagAudio || t == tagVideo || t == tagScript {
			if len(c) < tagHeaderLen {
				if !s.eof {
					break // wait for a full tag header before judging
				}
				i++
				continue
			}
			size := be24(c[1:4])
			if size != 0 && size <= s.opts.MaxTagSize {
				end := tagHeaderLen + size
				if len(c) < end+prevSizeLen {
					if !s.eof {
						break // plausible candidate; need more data to verify
					}
					i++
					continue
				}
				if be32(c[end:end+prevSizeLen]) == uint32(end) && plausibleNext(c[end+prevSizeLen:], s.opts.MaxTagSize) {
					found = true
					break
				}
			}
		}
		i++
	}
	s.consume(i)
	s.stats.desyncBytesDrop += int64(i)
	if found {
		s.resync = false
		s.logger.Info("FLV stream resynchronized",
			"bytes_dropped", s.stats.desyncBytesDrop)
	}
	return found
}

// plausibleNext sanity-checks the unit following a resync candidate to reject
// coincidental PreviousTagSize matches. An empty or short remainder is
// accepted optimistically; a wrong guess just re-enters resync.
func plausibleNext(nxt []byte, maxTagSize int) bool {
	if len(nxt) < 4 {
		return true
	}
	if isFileHeader(nxt) {
		return true
	}
	t := nxt[0]
	if t != tagAudio && t != tagVideo && t != tagScript {
		return false
	}
	size := be24(nxt[1:4])
	return size != 0 && size <= maxTagSize
}

// handleTag routes one complete tag (header+payload+prevsize in full). Returns
// consumed=false when the tag triggers a config-change rotation and must be
// reprocessed after RotateOutput.
func (s *Sanitizer) handleTag(full []byte) (consumed bool, err error) {
	// A stream that opens mid-flow (no leading FLV header) must still produce
	// a recognizable file.
	if !s.sawFileHeader {
		s.sawFileHeader = true
		if err := s.emitFileHeader(); err != nil {
			return true, err
		}
	}
	tag := full[:len(full)-prevSizeLen]
	payload := tag[tagHeaderLen:]
	switch tag[0] {
	case tagScript:
		if s.scriptSeen {
			s.stats.scriptsDropped++
			return true, nil
		}
		s.scriptSeen = true
		return true, s.emitTag(full, 0)
	case tagAudio:
		isCfg, needsCfg := audioConfigInfo(payload)
		if isCfg {
			return s.onConfig(tag, payload, false)
		}
		if needsCfg && s.audioCfgID == nil {
			s.stats.preConfigTags++
			return true, nil
		}
		return true, s.onMedia(full, streamAudio, false)
	case tagVideo:
		vi := parseVideoInfo(payload)
		if vi.config {
			return s.onConfig(tag, payload, true)
		}
		if vi.endOfSeq {
			return true, nil
		}
		if s.videoCfgID == nil {
			s.stats.preConfigTags++
			return true, nil
		}
		return true, s.onMedia(full, streamVideo, vi.keyframe)
	}
	return true, nil
}

// onConfig handles a sequence-header tag: adopt it (first of its stream),
// drop it (identical re-send), or trigger an output rotation (real change).
// Emission is deferred to the stream's first media tag so an output never
// carries a config that no media used.
func (s *Sanitizer) onConfig(tag, payload []byte, video bool) (consumed bool, err error) {
	var id []byte
	if video {
		id = videoConfigIdentity(payload)
	} else {
		id = bytes.Clone(payload)
	}
	activeID := s.audioCfgID
	if video {
		activeID = s.videoCfgID
	}
	if activeID != nil {
		if bytes.Equal(id, activeID) {
			s.stats.dupConfigsDropped++
			return true, nil
		}
		if s.mediaEmitted {
			// Real config change under emitted media: rotate the output.
			s.pendingRotate = true
			s.pendingIsVideo = video
			if video {
				s.pendingKey = configKey(id, s.audioCfgID)
			} else {
				s.pendingKey = configKey(s.videoCfgID, id)
			}
			s.logger.Info("Codec configuration changed",
				"stream", streamName(video), "config_changes", s.stats.configChanges+1)
			return false, nil
		}
		// Config superseded before any media used it: replace silently.
	}
	stored := bytes.Clone(tag)
	if video {
		s.videoCfgID, s.videoCfgTag = id, stored
		s.videoCfgEmitted = false
		s.videoNeedKeyframe = true
	} else {
		s.audioCfgID, s.audioCfgTag = id, stored
		s.audioCfgEmitted = false
	}
	return true, nil
}

// onMedia applies the timeline rules to an audio/video media tag and emits it
// (preceded by its stream's pending config tag) if it survives.
func (s *Sanitizer) onMedia(full []byte, stream int, keyframe bool) error {
	tag := full[:len(full)-prevSizeLen]
	inTs := tagTimestamp(tag)
	if !s.offsetSet {
		s.offset = s.baseOut - inTs
		s.offsetSet = true
	}
	out := inTs + s.offset
	// Interleave lag can put a stream's first tags before the part's anchor
	// point (e.g. audio trailing the keyframe that defined the offset). They
	// have nowhere valid to land; drop them rather than clamping into a pile
	// of equal timestamps.
	if out < 0 && s.lastOut[stream] < 0 {
		s.stats.negativeTsTags++
		if stream == streamVideo {
			s.videoNeedKeyframe = true
		}
		return nil
	}
	maxOut := max(s.lastOut[streamAudio], s.lastOut[streamVideo])

	if maxOut >= 0 {
		switch {
		case out > maxOut+s.opts.GapToleranceMs && !s.opts.RepairsDisabled:
			// Forward gap: glue segments (content is continuous; the encoder
			// simply produced nothing for a while).
			//
			// Whatever the encoder resumes with cannot lean on what came
			// before a gap this long, so video restarts at a keyframe. A
			// source that resumes with one — the common case — pays nothing.
			gap := out - maxOut
			s.endDedup()
			s.rebase(maxOut, inTs)
			out = inTs + s.offset
			s.stats.gapsGlued++
			s.stats.gapMsRemoved += gap - s.frameGap
			s.videoNeedKeyframe = true
			s.logger.Info("Glued timeline gap", "gap_ms", gap, "at_out_ms", out)

		case s.lastOut[stream] >= 0 && out <= s.lastOut[stream]:
			back := s.lastOut[stream] - out
			switch {
			case back > s.opts.ResetThresholdMs || s.opts.RepairsDisabled:
				// Too far back to be a replay: timeline reset, glue forward.
				s.endDedup()
				s.rebase(maxOut, inTs)
				out = inTs + s.offset
				s.stats.resets++
				s.videoNeedKeyframe = true
				s.logger.Info("Timeline reset", "back_ms", back, "at_out_ms", out)
			case stream == streamVideo && !s.dedupActive && back <= s.opts.JitterClampMs:
				// Sub-frame jitter (e.g. right after a glue): clamp rather
				// than break the GOP by dropping.
				out = s.lastOut[stream] + 1
				s.stats.jitterClamps++
			default:
				// Replayed content: drop until input passes the high-water
				// mark; escalate if the replay never catches up.
				if !s.dedupActive {
					s.dedupActive = true
					s.dedupLowOut = out
					s.dedupHighOut = out
					s.stats.dedupWindows++
					s.logger.Info("Replayed segment detected, deduplicating",
						"back_ms", back)
				}
				s.dedupHighOut = max(s.dedupHighOut, out)
				if out-s.dedupLowOut > s.opts.DedupEscalateMs {
					span := out - s.dedupLowOut
					s.endDedup()
					s.rebase(maxOut, inTs)
					out = inTs + s.offset
					s.stats.resets++
					s.videoNeedKeyframe = true
					s.logger.Info("Dedup window escalated to timeline reset",
						"span_ms", span)
					break
				}
				s.stats.dedupTags++
				if stream == streamVideo {
					s.videoNeedKeyframe = true
				}
				return nil
			}

		default:
			if s.dedupActive {
				s.endDedup()
				s.logger.Info("Replay deduplicated, resuming",
					"dropped_ms", s.stats.dedupMsDropped)
			}
		}
	}

	if stream == streamVideo {
		if s.videoNeedKeyframe && !keyframe {
			s.stats.keyframeWaitTags++
			return nil
		}
		s.videoNeedKeyframe = false
		if s.lastVidIn >= 0 {
			if d := inTs - s.lastVidIn; d > 0 && d <= 200 {
				s.frameGap = (s.frameGap*7 + d) / 8
			}
		}
		s.lastVidIn = inTs
	}

	if stream == streamVideo && !s.rawTimestamps {
		if adj := s.grid.adjust(out, s.lastOut[streamVideo]); adj != out {
			s.stats.tsSnapped++
			out = adj
		}
	}
	if stream == streamVideo && !s.opts.RepairsDisabled {
		s.repairCompositionTime(tag[tagHeaderLen:], out)
	}

	// Deferred config emission: the stream's active config precedes its first
	// media tag, at the same output timestamp.
	if stream == streamVideo && !s.videoCfgEmitted {
		if err := s.emitStoredTag(s.videoCfgTag, out); err != nil {
			return err
		}
		s.videoCfgEmitted = true
	}
	if stream == streamAudio && !s.audioCfgEmitted && s.audioCfgTag != nil {
		if err := s.emitStoredTag(s.audioCfgTag, out); err != nil {
			return err
		}
		s.audioCfgEmitted = true
	}

	if err := s.emitTag(full, out); err != nil {
		return err
	}
	s.lastOut[stream] = out
	s.mediaEmitted = true
	return nil
}

// repairCompositionTime replaces an implausible composition time in a video
// payload, in place, with the last plausible one: the stream's reorder delay is
// the best estimate of where the frame belongs.
func (s *Sanitizer) repairCompositionTime(payload []byte, out int64) {
	off := compositionTimeOffset(payload)
	if off < 0 {
		return
	}
	cts := si24(payload[off:])
	if cts >= -maxCompositionMs && cts <= maxCompositionMs {
		s.lastCTS = cts
		return
	}
	putSI24(payload[off:], s.lastCTS)
	s.stats.ctsRepaired++
	s.logger.Info("Replaced implausible composition time",
		"cts_ms", cts, "replaced_with_ms", s.lastCTS, "at_out_ms", out)
}

// endDedup closes an open dedup window, folding its span into the stats.
func (s *Sanitizer) endDedup() {
	if !s.dedupActive {
		return
	}
	s.dedupActive = false
	s.stats.dedupMsDropped += s.dedupHighOut - s.dedupLowOut
}

// rebase adjusts the global input→output offset so the tag with input
// timestamp inTs lands one nominal frame after lastGoodOut. A single shared
// offset keeps A/V sync intact across the discontinuity.
func (s *Sanitizer) rebase(lastGoodOut, inTs int64) {
	s.offset = lastGoodOut + s.frameGap - inTs
	s.lastVidIn = -1
}

// emitTag rewrites the tag's timestamp to out and writes the full unit
// (header+payload+PreviousTagSize) in one call. full aliases the internal
// buffer; mutating the timestamp in place is safe because the bytes are
// consumed immediately after emission.
func (s *Sanitizer) emitTag(full []byte, out int64) error {
	putTagTimestamp(full, out)
	if err := s.emit(full); err != nil {
		return err
	}
	s.stats.tagsOut++
	return nil
}

// emitStoredTag emits a retained config tag (header+payload) at timestamp out,
// appending its PreviousTagSize.
func (s *Sanitizer) emitStoredTag(tag []byte, out int64) error {
	unit := make([]byte, len(tag)+prevSizeLen)
	copy(unit, tag)
	putTagTimestamp(unit, out)
	sz := uint32(len(tag))
	unit[len(tag)] = byte(sz >> 24)
	unit[len(tag)+1] = byte(sz >> 16)
	unit[len(tag)+2] = byte(sz >> 8)
	unit[len(tag)+3] = byte(sz)
	if err := s.emit(unit); err != nil {
		return err
	}
	s.stats.tagsOut++
	return nil
}

func (s *Sanitizer) emit(b []byte) error {
	n, err := s.out.Write(b)
	s.written += int64(n)
	s.stats.bytesOut += int64(n)
	if err != nil {
		s.err = fmt.Errorf("%w: %v", errSinkWrite, err)
		return s.err
	}
	return nil
}

func streamName(video bool) string {
	if video {
		return "video"
	}
	return "audio"
}
