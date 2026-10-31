package preview

import (
	"sort"
	"time"
)

// tiers maps a length of video to its timeline-preview interval: a short
// video is previewed every second, a long one sparsely enough to stay within
// a few hundred frames, about as many as a seek bar has pixels.
var tiers = []struct {
	upTo     time.Duration
	interval time.Duration
}{
	{5 * time.Minute, time.Second},
	{10 * time.Minute, 2 * time.Second},
	{30 * time.Minute, 5 * time.Second},
	{time.Hour, 10 * time.Second},
	{2 * time.Hour, 20 * time.Second},
	{4 * time.Hour, 30 * time.Second},
	{8 * time.Hour, time.Minute},
}

// Interval is the timeline-preview spacing for a video of length d. A
// finished file uses its whole length; a live recording its length so far,
// so its previews thin out as it grows.
func Interval(d time.Duration) time.Duration {
	for _, t := range tiers {
		if d <= t.upTo {
			return t.interval
		}
	}
	return 2 * time.Minute
}

// FullDecodeLimit is the longest video whose previews may come from decoding
// every frame, when its keyframes are too sparse for the interval.
const FullDecodeLimit = 15 * time.Minute

// MeanKeySpacing is the average distance between keyframes.
func MeanKeySpacing(keys []time.Duration, d time.Duration) time.Duration {
	if len(keys) < 2 {
		return d
	}
	return (keys[len(keys)-1] - keys[0]) / time.Duration(len(keys)-1)
}

// PickKeys chooses, for each target time, the latest keyframe at or before
// it (the frame a player would show there), dropping repeats. keys must be
// sorted. It returns indexes into keys.
func PickKeys(keys []time.Duration, targets []time.Duration) []int {
	var out []int
	for _, t := range targets {
		i := sort.Search(len(keys), func(i int) bool { return keys[i] > t }) - 1
		if i < 0 {
			i = 0
		}
		if len(keys) == 0 {
			break
		}
		if len(out) > 0 && out[len(out)-1] >= i {
			continue
		}
		out = append(out, i)
	}
	return out
}

// NearestKey is the keyframe closest to t.
func NearestKey(keys []time.Duration, t time.Duration) int {
	i := sort.Search(len(keys), func(i int) bool { return keys[i] >= t })
	switch {
	case i == 0:
		return 0
	case i == len(keys):
		return len(keys) - 1
	case keys[i]-t < t-keys[i-1]:
		return i
	}
	return i - 1
}

// Ticks lists 0, step, 2*step, … below d.
func Ticks(d, step time.Duration) []time.Duration {
	if step <= 0 {
		return nil
	}
	var out []time.Duration
	for t := time.Duration(0); t < d; t += step {
		out = append(out, t)
	}
	return out
}

// Spread lists n times evenly spaced across [from, to], ends included.
func Spread(from, to time.Duration, n int) []time.Duration {
	if n <= 0 {
		return nil
	}
	if n == 1 {
		return []time.Duration{(from + to) / 2}
	}
	out := make([]time.Duration, n)
	for i := range out {
		out[i] = from + (to-from)*time.Duration(i)/time.Duration(n-1)
	}
	return out
}
