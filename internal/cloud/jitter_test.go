package cloud

import (
	"testing"
	"time"
)

func TestJitterStaysWithinItsBand(t *testing.T) {
	const d = time.Minute
	low, high := time.Duration(float64(d)*(1-jitterFraction)), time.Duration(float64(d)*(1+jitterFraction))

	var seen = map[time.Duration]bool{}
	for range 500 {
		got := jitter(d)
		if got < low || got > high {
			t.Fatalf("jitter(%v) = %v, want within [%v, %v]", d, got, low, high)
		}
		seen[got] = true
	}
	// The point is that daemons do not all wake at once, so the same wait has
	// to come out differently from one caller to the next.
	if len(seen) < 100 {
		t.Fatalf("only %d distinct waits in 500 calls; retries would stay in lockstep", len(seen))
	}
}

func TestJitterLeavesNonWaitsAlone(t *testing.T) {
	for _, d := range []time.Duration{0, -time.Second} {
		if got := jitter(d); got != d {
			t.Fatalf("jitter(%v) = %v, want it unchanged", d, got)
		}
	}
}
