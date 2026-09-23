package hlsingest

import (
	"time"

	m3u8 "github.com/Eyevinn/hls-m3u8/m3u8"
)

// defaultLiveEdgeWindow bounds how much already-published content an ingest
// adopts from the FIRST playlist it fetches, when Config leaves it unset.
// Segments older than this are marked as seen without being downloaded; every
// later poll behaves normally.
//
// Without it a DVR Origin (e.g. YouTube, which publishes hours of history)
// would be backfilled from the start of its window. 30 seconds sits above the
// short windows most Platforms publish, so only DVR histories are trimmed.
const defaultLiveEdgeWindow = 30 * time.Second

// liveEdgeCutoff returns the index of the last segment that falls outside
// window, or -1 when the whole playlist is inside it.
//
// Walking from the end and accumulating EXTINF durations makes the cut on
// playback time rather than a segment count, which stays correct whether an
// Origin publishes 1-second or 10-second segments.
func liveEdgeCutoff(segments []*m3u8.MediaSegment, window time.Duration) int {
	var acc time.Duration
	for i := len(segments) - 1; i >= 0; i-- {
		seg := segments[i]
		if seg == nil || seg.URI == "" {
			continue
		}
		if acc >= window {
			return i
		}
		acc += time.Duration(seg.Duration * float64(time.Second))
	}
	return -1
}

// seedLiveEdge marks the segments older than window as already downloaded, so
// an ingest begins at the live edge. It returns how many segments it skipped.
func seedLiveEdge(
	segments []*m3u8.MediaSegment,
	window time.Duration,
	resolve func(string) (string, bool),
	seen map[string]bool,
) int {
	cutoff := liveEdgeCutoff(segments, window)
	if cutoff < 0 {
		return 0
	}

	skipped := 0
	for i := 0; i <= cutoff; i++ {
		seg := segments[i]
		if seg == nil || seg.URI == "" {
			continue
		}
		abs, ok := resolve(seg.URI)
		if !ok {
			continue
		}
		if !seen[abs] {
			seen[abs] = true
			skipped++
		}
	}
	return skipped
}
