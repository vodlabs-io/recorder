package hlsingest

// An Origin may publish a variant's video and its audio as two playlists: the
// master pairs each variant with an EXT-X-MEDIA audio rendition of its own
// (Chaturbate does, for every variant). A Sink takes one stream, so both
// playlists are read side by side and joined here: one init declaring both
// tracks, then each video segment followed by the audio fragments that play
// during it. Sync rides on the fragments' own decode times, which both
// renditions take from the same clock.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sync"
	"time"
)

// maxPendingVideo is how many video segments may wait for the audio covering
// them. Past it the oldest goes out with whatever audio has arrived, so a
// stalled audio playlist delays the video by seconds rather than stopping it.
const maxPendingVideo = 3

// maxPendingAudio bounds the audio fragments held for video that has not
// arrived, so a stalled video playlist cannot grow the queue without limit.
const maxPendingAudio = 32

// audioSlack is how far short of a video segment's end the audio received so
// far may stop and still count as covering it, in seconds; fragment boundaries
// of the two renditions need not line up.
const audioSlack = 0.05

// audioGrace is how long the audio rendition is still read once the video has
// ended, for the segments it publishes last.
const audioGrace = 5 * time.Second

// runDemuxed reads a video media playlist and the audio rendition its master
// pairs it with, handing the Sink one joined stream. It returns when the video
// ends, or when the audio fails; the audio is not re-resolved by RefreshURL.
func (o *ingest) runDemuxed(ctx context.Context, videoURL, audioURL string) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	audioCtx, stopAudio := context.WithCancel(ctx)
	defer stopAudio()

	m := &demuxMerger{sink: o.sink, logger: o.logger, stopAudio: stopAudio}
	video := &ingest{cfg: o.cfg, client: o.client, logger: o.logger, sink: videoTrack{m}}
	acfg := o.cfg
	acfg.RefreshURL, acfg.URLTTL = nil, 0
	audio := &ingest{cfg: acfg, client: o.client, logger: o.logger.With("rendition", "audio"), sink: audioTrack{m}}

	audioDone := make(chan error, 1)
	go func() {
		err := audio.run(audioCtx, audioURL)
		if err != nil {
			// The video alone is not the Broadcast; end the attempt so the
			// caller resolves both afresh.
			cancel()
		}
		audioDone <- err
	}()
	videoErr := video.run(ctx, videoURL)
	var audioErr error
	if videoErr == nil && ctx.Err() == nil {
		// The video ended with the Broadcast; the audio's last segments may
		// still be on their way.
		select {
		case audioErr = <-audioDone:
		case <-time.After(audioGrace):
			stopAudio()
			audioErr = <-audioDone
		}
	} else {
		stopAudio()
		audioErr = <-audioDone
	}

	flushErr := m.flush()
	switch {
	case videoErr != nil:
		return videoErr
	case audioErr != nil && !m.droppedAudio():
		var se *SinkError
		if errors.As(audioErr, &se) {
			return audioErr
		}
		return fmt.Errorf("audio rendition: %w", audioErr)
	case flushErr != nil:
		return &SinkError{err: flushErr}
	}
	return nil
}

// demuxMerger joins what the two renditions' ingests hand it. Each runs on its
// own goroutine; the mutex makes their calls into the Sink one at a time.
type demuxMerger struct {
	mu        sync.Mutex
	sink      Sink
	logger    *slog.Logger
	stopAudio func()

	// passthrough is set once the video turns out not to need the audio
	// joined: it carries a sound track of its own, or is not fragmented MP4,
	// or is encrypted. The video then goes to the Sink as it is.
	passthrough bool

	videoInit, audioInit []byte
	videoMapURI          string
	videoScale           uint32
	audioScale           uint32
	// initDirty is set when either init changed since the joined one was sent.
	initDirty bool
	// audioID is the audio track's id in the joined stream, zero until a joined
	// init has been sent.
	audioID uint32

	video []pendingSegment
	audio []pendingSegment
	// audioEnd is where the latest audio fragment ends, in seconds.
	audioEnd float64
	started  bool

	// err is a Sink failure, returned from every call after it.
	err error
}

// pendingSegment is a fetched segment waiting to be joined, timed in seconds
// on its track's decode clock. start is NaN when the segment carries no
// decode time.
type pendingSegment struct {
	seg        Segment
	body       []byte
	start, end float64
}

func (m *demuxMerger) droppedAudio() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.passthrough
}

// passVideoThrough stops joining: the video goes to the Sink as it is from now
// on, and the audio rendition is no longer read.
func (m *demuxMerger) passVideoThrough(reason string) error {
	m.passthrough = true
	m.stopAudio()
	m.logger.Warn("Recording the video rendition without its separate audio", "reason", reason)
	if m.videoInit != nil {
		if err := m.sink.Init(Map{URI: m.videoMapURI}, m.videoInit); err != nil {
			return err
		}
	}
	for _, v := range m.video {
		if err := m.sink.Segment(v.seg, v.body); err != nil {
			return err
		}
	}
	m.video = nil
	m.audio = nil
	return nil
}

func timed(seg Segment, body []byte, scale uint32) pendingSegment {
	p := pendingSegment{seg: seg, body: body, start: math.NaN(), end: math.NaN()}
	if t, ok := fragmentStart(body); ok && scale > 0 {
		p.start = float64(t) / float64(scale)
		p.end = p.start + seg.Duration
	}
	return p
}

func (m *demuxMerger) videoInitSeg(mp Map, body []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return m.err
	}
	if m.passthrough {
		return m.fail(m.sink.Init(mp, body))
	}
	m.videoInit, m.videoMapURI = body, mp.URI
	if initHasSound(body) {
		return m.fail(m.passVideoThrough("the video rendition carries its own audio"))
	}
	scale, err := initTimescale(body)
	if err != nil {
		return m.fail(m.passVideoThrough(err.Error()))
	}
	m.videoScale = scale
	m.initDirty = true
	return nil
}

func (m *demuxMerger) videoSegment(seg Segment, body []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return m.err
	}
	if m.passthrough {
		return m.fail(m.sink.Segment(seg, body))
	}
	switch {
	case !seg.IsFMP4:
		m.video = append(m.video, pendingSegment{seg: seg, body: body})
		return m.fail(m.passVideoThrough("the video rendition is not fragmented MP4"))
	case seg.Key != nil:
		m.video = append(m.video, pendingSegment{seg: seg, body: body})
		return m.fail(m.passVideoThrough("the video rendition is encrypted"))
	}
	m.video = append(m.video, timed(seg, body, m.videoScale))
	return m.fail(m.drain(false))
}

func (m *demuxMerger) audioInitSeg(_ Map, body []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil || m.passthrough {
		return m.err
	}
	scale, err := initTimescale(body)
	if err != nil {
		return m.fail(m.passVideoThrough("audio init: " + err.Error()))
	}
	m.audioInit, m.audioScale = body, scale
	m.initDirty = true
	return nil
}

func (m *demuxMerger) audioSegment(seg Segment, body []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil || m.passthrough {
		return m.err
	}
	if !seg.IsFMP4 || seg.Key != nil {
		return m.fail(m.passVideoThrough("the audio rendition is not plain fragmented MP4"))
	}
	p := timed(seg, body, m.audioScale)
	m.audio = append(m.audio, p)
	if len(m.audio) > maxPendingAudio {
		m.audio = m.audio[1:]
	}
	if !math.IsNaN(p.end) && p.end > m.audioEnd {
		m.audioEnd = p.end
	}
	return m.fail(m.drain(false))
}

// flush hands over every video segment still waiting, once both renditions
// have stopped.
func (m *demuxMerger) flush() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil || m.passthrough {
		return m.err
	}
	return m.fail(m.drain(true))
}

func (m *demuxMerger) fail(err error) error {
	if err != nil && m.err == nil {
		m.err = err
	}
	return err
}

// drain hands the Sink every waiting video segment the audio has caught up
// with, joined with that audio. final hands over the rest regardless.
func (m *demuxMerger) drain(final bool) error {
	for len(m.video) > 0 && m.videoInit != nil {
		v := m.video[0]
		covered := m.audioInit != nil && !math.IsNaN(v.end) && m.audioEnd >= v.end-audioSlack
		if !final && !covered && !math.IsNaN(v.end) && len(m.video) <= maxPendingVideo {
			return nil
		}

		if m.initDirty {
			init, id := m.videoInit, uint32(0)
			if m.audioInit != nil {
				merged, audioID, err := mergeInits(m.videoInit, m.audioInit)
				if err != nil {
					return m.passVideoThrough("join inits: " + err.Error())
				}
				init, id = merged, audioID
			}
			if err := m.sink.Init(Map{URI: m.videoMapURI}, init); err != nil {
				return err
			}
			m.audioID = id
			m.initDirty = false
		}

		body := v.body
		if m.audioID != 0 {
			body = append([]byte(nil), v.body...)
			for len(m.audio) > 0 {
				a := m.audio[0]
				if !math.IsNaN(a.start) && !math.IsNaN(v.end) && a.start >= v.end-audioSlack {
					break
				}
				m.audio = m.audio[1:]
				if !m.started && !math.IsNaN(a.end) && !math.IsNaN(v.start) && a.end <= v.start {
					// Audio from before the first video segment: nothing to
					// play it against.
					continue
				}
				frag, err := retrackFragments(a.body, m.audioID)
				if err != nil {
					m.logger.Warn("Dropping an unreadable audio segment", "error", err, "segment_url", a.seg.URI)
					continue
				}
				body = append(body, frag...)
			}
		}
		if err := m.sink.Segment(v.seg, body); err != nil {
			return err
		}
		m.started = true
		m.video = m.video[1:]
	}
	return nil
}

// videoTrack and audioTrack are the Sinks the two renditions' ingests write to.
type videoTrack struct{ m *demuxMerger }

func (t videoTrack) Init(mp Map, body []byte) error       { return t.m.videoInitSeg(mp, body) }
func (t videoTrack) Segment(s Segment, body []byte) error { return t.m.videoSegment(s, body) }
func (t videoTrack) Finish(error) error                   { return nil }

type audioTrack struct{ m *demuxMerger }

func (t audioTrack) Init(mp Map, body []byte) error       { return t.m.audioInitSeg(mp, body) }
func (t audioTrack) Segment(s Segment, body []byte) error { return t.m.audioSegment(s, body) }
func (t audioTrack) Finish(error) error                   { return nil }
