package rtcflv

import (
	"sync"
	"time"
)

// srWait is how long a track's samples wait for the sender's first RTCP
// Sender Report before the track falls back to arrival-time stamping.
const srWait = 3 * time.Second

// reanchorAfter is how far a Sender Report may disagree with a track's anchor
// before the track is re-anchored on it.
const reanchorAfter = 500 * time.Millisecond

// mediaClock maps each track's RTP timestamps onto one wall clock, so audio
// and video line up the way the broadcaster captured them.
//
// A Sender Report pairs an RTP timestamp with the sender's NTP time. A track
// is anchored on its first one and stamped from its RTP clock after that: the
// NTP times in later reports jitter by tens of milliseconds, and following
// them would step the timeline back and forth. A report that disagrees with
// the anchor by more than reanchorAfter means the RTP clock itself jumped, and
// re-anchors the track. A track whose sender never reports within srWait is
// stamped from when its first packet arrived instead, and stays that way.
type mediaClock struct {
	mu     sync.Mutex
	tracks map[uint32]*trackClock
}

type trackClock struct {
	rate     float64
	srNTP    time.Time
	srRTP    uint32
	haveSR   bool
	arrival  bool // stamped from arrival time, for want of a Sender Report
	firstRTP uint32
	firstAt  time.Time
	seen     bool
}

func newMediaClock() *mediaClock {
	return &mediaClock{tracks: map[uint32]*trackClock{}}
}

func (c *mediaClock) track(ssrc uint32, rate uint32) *trackClock {
	t := c.tracks[ssrc]
	if t == nil {
		t = &trackClock{rate: float64(rate)}
		c.tracks[ssrc] = t
	}
	return t
}

// senderReport records a Sender Report for ssrc.
func (c *mediaClock) senderReport(ssrc uint32, rate uint32, ntp uint64, rtpTS uint32) {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := c.track(ssrc, rate)
	at := ntpTime(ntp)
	if t.haveSR {
		drift := t.srNTP.Add(ticks(rtpTS-t.srRTP, t.rate)).Sub(at)
		if drift.Abs() <= reanchorAfter {
			return
		}
	}
	t.srNTP = at
	t.srRTP = rtpTS
	t.haveSR = true
}

// wall returns the wall-clock time of a sample with RTP timestamp rtpTS, and
// false while the track is still waiting for its first Sender Report.
func (c *mediaClock) wall(ssrc uint32, rate uint32, rtpTS uint32, now time.Time) (time.Time, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := c.track(ssrc, rate)
	if !t.seen {
		t.firstRTP, t.firstAt, t.seen = rtpTS, now, true
	}
	switch {
	case t.arrival:
	case t.haveSR:
		return t.srNTP.Add(ticks(rtpTS-t.srRTP, t.rate)), true
	case now.Sub(t.firstAt) < srWait:
		return time.Time{}, false
	default:
		t.arrival = true
	}
	return t.firstAt.Add(ticks(rtpTS-t.firstRTP, t.rate)), true
}

// ticks converts an RTP timestamp difference, taken modulo 2^32 and read as
// signed so a sample just before the reference lands just before it.
func ticks(d uint32, rate float64) time.Duration {
	return time.Duration(float64(int32(d)) / rate * float64(time.Second))
}

// ntpTime converts a 64-bit NTP timestamp.
func ntpTime(ntp uint64) time.Time {
	const ntpEpochOffset = 2208988800
	secs := int64(ntp>>32) - ntpEpochOffset
	frac := float64(ntp&0xffffffff) / (1 << 32)
	return time.Unix(secs, int64(frac*1e9))
}
