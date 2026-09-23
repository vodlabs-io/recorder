package hlsingest

import (
	"fmt"
	"testing"
	"time"

	m3u8 "github.com/Eyevinn/hls-m3u8/m3u8"
)

// playlist builds n segments of the given duration, named seg0..segN-1.
func playlist(n int, durSec float64) []*m3u8.MediaSegment {
	segs := make([]*m3u8.MediaSegment, n)
	for i := range segs {
		segs[i] = &m3u8.MediaSegment{URI: fmt.Sprintf("seg%d.ts", i), Duration: durSec}
	}
	return segs
}

func resolver() func(string) (string, bool) {
	return func(uri string) (string, bool) { return "https://cdn.example/" + uri, true }
}

// TestSeedLiveEdge_ShortWindowsUnchanged covers the Platforms whose Origins
// publish only a few seconds of history: nothing may be skipped, so an ingest
// still begins with the very first segment it sees.
func TestSeedLiveEdge_ShortWindowsUnchanged(t *testing.T) {
	cases := []struct {
		name  string
		count int
		dur   float64
	}{
		{"twitch-like 6x2s", 6, 2},
		{"kick/ivs-like 5x2s", 5, 2},
		{"pandalive-like 3x5s", 3, 5},
		{"bigo-like 4x4s", 4, 4},
		{"exactly at the window 10x3s", 10, 3},
	}
	for _, tc := range cases {
		seen := map[string]bool{}
		skipped := seedLiveEdge(playlist(tc.count, tc.dur), defaultLiveEdgeWindow, resolver(), seen)
		if skipped != 0 {
			t.Errorf("%s: skipped %d segments, want 0 (behaviour must be unchanged)", tc.name, skipped)
		}
		if len(seen) != 0 {
			t.Errorf("%s: seeded %d entries, want none", tc.name, len(seen))
		}
	}
}

// TestSeedLiveEdge_DVRTrimmed covers the case this exists for: YouTube's live
// playlist carries a fixed 4-hour, 2880-segment DVR window.
func TestSeedLiveEdge_DVRTrimmed(t *testing.T) {
	const total = 2880
	seen := map[string]bool{}
	skipped := seedLiveEdge(playlist(total, 5), defaultLiveEdgeWindow, resolver(), seen)

	// 30s of window at 5s per segment leaves the last handful playable.
	kept := total - skipped
	if kept < 6 || kept > 8 {
		t.Errorf("kept %d segments of %d, want the live edge only", kept, total)
	}
	if len(seen) != skipped {
		t.Errorf("seeded %d entries for %d skipped segments", len(seen), skipped)
	}
}

// TestSeedLiveEdge_CutsOnPlaybackTime pins that the boundary is playback time
// rather than a segment count, so it stays correct whether an Origin publishes
// 1-second or 10-second segments.
func TestSeedLiveEdge_CutsOnPlaybackTime(t *testing.T) {
	oneSec := map[string]bool{}
	seedLiveEdge(playlist(300, 1), defaultLiveEdgeWindow, resolver(), oneSec)
	tenSec := map[string]bool{}
	seedLiveEdge(playlist(300, 10), defaultLiveEdgeWindow, resolver(), tenSec)

	// 30s of 1s segments keeps ~30; 30s of 10s segments keeps ~3.
	if kept := 300 - len(oneSec); kept < 29 || kept > 32 {
		t.Errorf("1s segments: kept %d, want ~30", kept)
	}
	if kept := 300 - len(tenSec); kept < 2 || kept > 5 {
		t.Errorf("10s segments: kept %d, want ~3", kept)
	}
}

// TestSeedLiveEdge_WindowIsConfigurable covers the Config knob: a wider window
// keeps more of the Origin's history, a narrower one keeps less.
func TestSeedLiveEdge_WindowIsConfigurable(t *testing.T) {
	narrow := map[string]bool{}
	seedLiveEdge(playlist(100, 1), 5*time.Second, resolver(), narrow)
	wide := map[string]bool{}
	seedLiveEdge(playlist(100, 1), 60*time.Second, resolver(), wide)

	if len(narrow) <= len(wide) {
		t.Errorf("a 5s window skipped %d and a 60s window skipped %d; the narrow one must skip more",
			len(narrow), len(wide))
	}
}

func TestSeedLiveEdge_EdgeCases(t *testing.T) {
	if got := seedLiveEdge(nil, defaultLiveEdgeWindow, resolver(), map[string]bool{}); got != 0 {
		t.Errorf("nil playlist skipped %d, want 0", got)
	}
	if got := seedLiveEdge(playlist(0, 2), defaultLiveEdgeWindow, resolver(), map[string]bool{}); got != 0 {
		t.Errorf("empty playlist skipped %d, want 0", got)
	}
	// An unresolvable URI cannot be marked seen, so it is left to the loop.
	segs := playlist(100, 5)
	seen := map[string]bool{}
	skipped := seedLiveEdge(segs, defaultLiveEdgeWindow, func(string) (string, bool) { return "", false }, seen)
	if skipped != 0 || len(seen) != 0 {
		t.Errorf("unresolvable URIs skipped %d and seeded %d, want 0 and 0", skipped, len(seen))
	}
}
