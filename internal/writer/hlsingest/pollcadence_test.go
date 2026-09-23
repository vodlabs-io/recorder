package hlsingest

import (
	"testing"
	"time"
)

// TestAdaptivePoll covers the trade the cadence makes: an Origin that publishes
// seconds of history must be polled at the configured floor or segments are
// lost, while one that publishes hours of it cannot lose anything and is polled
// far more slowly — which is what stops YouTube's 7200-segment playlist from
// being re-fetched thirty times a minute.
func TestAdaptivePoll(t *testing.T) {
	o := &ingest{cfg: Config{PollInterval: 2 * time.Second}}
	cases := []struct {
		name   string
		window time.Duration
		want   time.Duration
	}{
		{"twitch-like 12s window", 12 * time.Second, 2 * time.Second},
		{"a window worth a tenth of the floor", 10 * time.Second, 2 * time.Second},
		{"30s window earns 3s", 30 * time.Second, 3 * time.Second},
		{"youtube-like 2h window", 2 * time.Hour, maxAdaptivePoll},
		{"empty playlist stays at the floor", 0, 2 * time.Second},
	}
	for _, tc := range cases {
		if got := o.adaptivePoll(tc.window); got != tc.want {
			t.Errorf("%s: adaptivePoll(%s) = %s, want %s", tc.name, tc.window, got, tc.want)
		}
	}
}

// TestSegSeenBySequenceSurvivesResignedURLs is why segSeen has two modes: after
// a refresh the same segment arrives under a URL that shares nothing with the
// old one, and only the media sequence still names it.
func TestSegSeenBySequenceSurvivesResignedURLs(t *testing.T) {
	s := newSegSeen(true)
	s.mark(100, "https://a.example/g1/100.ts")
	if !s.has(100, "https://a.example/g2/100.ts") {
		t.Error("a re-signed URL for sequence 100 was not recognised as already taken")
	}
	if s.has(101, "https://a.example/g2/101.ts") {
		t.Error("sequence 101 was reported as already taken")
	}
}

// TestSegSeenByURLIsTheDefault pins that nothing changes for the Origins whose
// URLs are stable, including those that publish no media sequence at all.
func TestSegSeenByURLIsTheDefault(t *testing.T) {
	s := newSegSeen(false)
	s.mark(0, "https://a.example/x.ts")
	if !s.has(0, "https://a.example/x.ts") {
		t.Error("a URL that was marked was not seen")
	}
	if s.has(0, "https://a.example/y.ts") {
		t.Error("an unmarked URL sharing sequence 0 was reported as seen")
	}
}

// TestSegSeenCursorNeverRewinds guards the case a sliding playlist creates: an
// Origin re-publishing an older segment must not make the ingest record from
// there again.
func TestSegSeenCursorNeverRewinds(t *testing.T) {
	s := newSegSeen(true)
	s.mark(10, "")
	s.mark(5, "")
	if !s.has(7, "") {
		t.Error("the cursor rewound to an older sequence")
	}
}

// TestSegSeenPruneDropsWhatTheOriginDropped keeps a long Session from
// remembering every segment it has ever seen.
func TestSegSeenPruneDropsWhatTheOriginDropped(t *testing.T) {
	s := newSegSeen(false)
	s.mark(0, "old.ts")
	s.mark(0, "current.ts")
	s.prune(map[string]bool{"current.ts": true})
	if s.len() != 1 || !s.has(0, "current.ts") {
		t.Errorf("prune left %d entries, want only the one still published", s.len())
	}
}
