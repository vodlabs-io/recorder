package dither

import (
	"fmt"
	"strings"
)

// Moving-image parameters. One offset applies to every whole frame within an
// interval, so the sequence is indexed by time rather than by frame number and
// is unaffected by a frame-rate conversion.
const (
	// IntervalMs is how long one offset holds.
	IntervalMs = 100
	// VideoLabel names the moving-image pattern.
	VideoLabel = "moving"
	// videoAmplitude is the luma offset in eq's brightness units, where 1.0 is
	// full range. About 1.5 levels of 255.
	videoAmplitude = 0.006
)

// VideoScript renders the timed command script that drives the offsets over a
// clip of the given duration. The format is one command per line:
//
//	<seconds> eq brightness <value>;
//
// A zero or negative duration yields an empty script. sendcmd rejects an empty
// script, so callers must leave VideoFilter out of the graph in that case.
func VideoScript(durationSec float64) string {
	return VideoScriptAt(0, durationSec)
}

// VideoScriptAt is VideoScript for a clip that starts startSec into a longer
// timeline, such as one batch of a merge: its times are relative to the
// clip's start, and its offsets are the ones the full timeline has there, so
// the clips joined back together carry the full timeline's sequence.
func VideoScriptAt(startSec, durationSec float64) string {
	if durationSec <= 0 {
		return ""
	}
	p := New(VideoLabel)
	first := int64(startSec * 1000 / IntervalMs)
	last := int64((startSec + durationSec) * 1000 / IntervalMs)
	var b strings.Builder
	for i := first; i <= last; i++ {
		// The interval in force at the clip's start begins before it.
		at := max(float64(i)*IntervalMs/1000-startSec, 0)
		fmt.Fprintf(&b, "%.4f eq brightness %.5f;\n",
			at, float64(p.Sign(i))*videoAmplitude)
	}
	return b.String()
}

// VideoFilter is the filter chain that applies a script, for splicing into a
// filtergraph. The eq starts neutral; every value comes from the script.
func VideoFilter(scriptPath string) string {
	return fmt.Sprintf("sendcmd=f=%s,eq=brightness=0", escapeFilterPath(scriptPath))
}

// escapeFilterPath escapes p for both levels ffmpeg unescapes a filter
// option value at: first the option list (`:` separates options), then the
// filtergraph around it (`,;[]` separate filters).
func escapeFilterPath(p string) string {
	opt := strings.NewReplacer(`\`, `\\`, `:`, `\:`, `'`, `\'`).Replace(p)
	return strings.NewReplacer(`\`, `\\`, `'`, `\'`, `,`, `\,`,
		`[`, `\[`, `]`, `\]`, `;`, `\;`).Replace(opt)
}
