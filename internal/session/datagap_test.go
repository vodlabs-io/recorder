package session

import (
	"testing"
	"time"
)

// TestDataGapMeasuresTheHoleNotThePartBoundary pins what "lagging" means: the
// time since bytes last landed on disk, not since the writer returned. An HLS
// part ends routinely and resumes seconds later, which is no hole in the
// capture and must not read as one.
func TestDataGapMeasuresTheHoleNotThePartBoundary(t *testing.T) {
	sessionStart := time.Now().Add(-2 * time.Hour)

	t.Run("bytes still flowing", func(t *testing.T) {
		w := &worker{}
		w.lastByteAt.Store(time.Now().Add(-3 * time.Second).UnixNano())
		if gap := w.dataGap(sessionStart); gap >= stallTimeout {
			t.Errorf("a part that rotated 3s ago is not lagging, got gap %v", gap)
		}
	})

	t.Run("bytes stopped", func(t *testing.T) {
		w := &worker{}
		w.lastByteAt.Store(time.Now().Add(-2 * stallTimeout).UnixNano())
		if gap := w.dataGap(sessionStart); gap < stallTimeout {
			t.Errorf("a recording with no bytes for %v is lagging, got gap %v",
				2*stallTimeout, gap)
		}
	})

	// Before the first byte the session start is the only clock there is, so a
	// session that never captures anything still reaches the bar.
	t.Run("no bytes ever", func(t *testing.T) {
		w := &worker{}
		if gap := w.dataGap(time.Now().Add(-time.Second)); gap >= stallTimeout {
			t.Errorf("a session one second old is not lagging, got gap %v", gap)
		}
		if gap := w.dataGap(sessionStart); gap < stallTimeout {
			t.Errorf("a session that never captured a byte is lagging, got gap %v", gap)
		}
	})

	// The anchor never wins over a later byte: the stall watchdog passes the
	// part start, and a part that begins after the last byte must not reset the
	// session's gap backwards.
	t.Run("anchor never moves the gap backwards", func(t *testing.T) {
		w := &worker{}
		last := time.Now().Add(-10 * time.Second)
		w.lastByteAt.Store(last.UnixNano())
		partStart := time.Now().Add(-time.Second)
		if gap := w.dataGap(partStart); gap > 2*time.Second {
			t.Errorf("a part started 1s ago anchors its own watchdog, got gap %v", gap)
		}
	})
}

// A session on a platform nothing monitors ends on a long enough hole, since no
// offline status will ever arrive for it; any other platform keeps lagging.
func TestEndsUnmonitored(t *testing.T) {
	for _, tc := range []struct {
		platform string
		gap      time.Duration
		want     bool
	}{
		{"generic", unmonitoredStallLimit, true},
		{"generic", unmonitoredStallLimit - time.Second, false},
		{"twitch", time.Hour, false},
	} {
		if got := endsUnmonitored(tc.platform, tc.gap); got != tc.want {
			t.Errorf("endsUnmonitored(%s, %v) = %v, want %v", tc.platform, tc.gap, got, tc.want)
		}
	}
}
