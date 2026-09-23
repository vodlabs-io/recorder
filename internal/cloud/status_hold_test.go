package cloud

import (
	"testing"
	"time"
)

// A network outage is answered by holding off, not by retrying: the cloud runs
// every check back out through this daemon, so retrying every streamer would
// only add load to the outage.
func TestPlatformLimiterHold(t *testing.T) {
	newLimiter := func() *platformLimiter {
		return &platformLimiter{platform: "twitch", resumed: make(chan struct{})}
	}
	// Fail a fresh limiter into its first hold, as one whole failed cycle does.
	trip := func(t *testing.T, l *platformLimiter, now time.Time) time.Duration {
		t.Helper()
		var started time.Duration
		for range laneFailureThreshold {
			d, _ := l.noteFailure(now)
			started = d
		}
		return started
	}

	t.Run("ridesOutAFailedRequestBeforeHolding", func(t *testing.T) {
		l := newLimiter()
		now := time.Now()
		for i := range laneFailureThreshold - 1 {
			if started, holding := l.noteFailure(now); holding || started != 0 {
				t.Fatalf("failure %d put the platform on hold: started=%v, holding=%v; "+
					"a whole cycle of retries has to fail first", i, started, holding)
			}
		}
		if started, holding := l.noteFailure(now); !holding || started != laneHoldBase {
			t.Fatalf("a whole failed cycle did not start a hold: started=%v, holding=%v", started, holding)
		}
		if wait, _ := l.hold(now); wait != laneHoldBase {
			t.Fatalf("hold left %v, want %v", wait, laneHoldBase)
		}
	})

	t.Run("theOtherLaneJoinsTheHoldWithoutExtendingIt", func(t *testing.T) {
		l := newLimiter()
		now := time.Now()
		trip(t, l, now)

		// The stream lane fails against the same broken path a moment later.
		started, holding := l.noteFailure(now.Add(time.Second))
		if !holding {
			t.Fatal("a lane failing during a hold was told to retry")
		}
		if started != 0 {
			t.Fatalf("a lane failing during a hold extended it by %v; the hold is per "+
				"platform, and one report of it is the point", started)
		}
	})

	t.Run("holdDoublesUpToTheCap", func(t *testing.T) {
		l := newLimiter()
		now := time.Now()
		want := laneHoldBase
		got := trip(t, l, now)
		for range 8 {
			if got != want {
				t.Fatalf("hold was %v, want %v", got, want)
			}
			now = now.Add(got) // the hold expires and the lane's probe fails again
			got, _ = l.noteFailure(now)
			want = min(2*want, laneHoldMax)
		}
		if got != laneHoldMax {
			t.Fatalf("hold grew to %v, want it capped at %v", got, laneHoldMax)
		}
	})

	t.Run("oneSuccessEndsTheHoldAndReportsItOnce", func(t *testing.T) {
		l := newLimiter()
		now := time.Now()
		trip(t, l, now)

		if !l.noteSuccess() {
			t.Fatal("the success that ended a hold did not report the recovery")
		}
		if wait, _ := l.hold(now); wait != 0 {
			t.Fatalf("still holding for %v after a successful fetch", wait)
		}
		if l.noteSuccess() {
			t.Fatal("a later success reported the recovery again")
		}
		// The next outage starts from the short hold, not where the last ended.
		if started := trip(t, l, now); started != laneHoldBase {
			t.Fatalf("hold after a recovery started at %v, want %v", started, laneHoldBase)
		}
	})

	t.Run("aReconnectReleasesTheLanesWaitingItOut", func(t *testing.T) {
		l := newLimiter()
		trip(t, l, time.Now())
		c := &Client{limiters: map[string]*platformLimiter{l.platform: l}}

		released := make(chan bool, 1)
		go func() { released <- c.waitHold(t.Context(), l) }()

		select {
		case <-released:
			t.Fatal("a lane went ahead while its platform was holding off")
		case <-time.After(50 * time.Millisecond):
		}

		c.resumeLanes()
		select {
		case ok := <-released:
			if !ok {
				t.Fatal("the released lane reported the daemon had stopped")
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("a lane sat out its %v hold after the daemon reconnected", laneHoldBase)
		}
	})
}

// The scrape handler serves the return leg of this daemon's own status checks,
// so a budget below what the lanes emit would refuse the daemon's own work, and
// each refusal would come back as a failed check to retry.
func TestScrapeBudgetCoversWhatTheLanesEmit(t *testing.T) {
	emitted := int(time.Minute / ceilingLimit.Interval)
	if scrapeBudgetPerMinute < emitted {
		t.Fatalf("scrape budget is %d/min against a ceiling that emits %d/min",
			scrapeBudgetPerMinute, emitted)
	}
	h := newScrapeHandler(nil, nil, nil, nil, nil)
	if h.maxPerMinute != scrapeBudgetPerMinute {
		t.Fatalf("handler admits %d/min, want the ceiling-sized %d/min",
			h.maxPerMinute, scrapeBudgetPerMinute)
	}
}
