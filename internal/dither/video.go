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
// An empty duration yields an empty script, which the filter accepts.
func VideoScript(durationSec float64) string {
	if durationSec <= 0 {
		return ""
	}
	p := New(VideoLabel)
	n := int(durationSec*1000/IntervalMs) + 1
	var b strings.Builder
	for i := range n {
		at := float64(i) * IntervalMs / 1000
		fmt.Fprintf(&b, "%.4f eq brightness %.5f;\n",
			at, float64(p.Sign(int64(i)))*videoAmplitude)
	}
	return b.String()
}

// VideoFilter is the filter chain that applies a script, for splicing into a
// filtergraph. The eq starts neutral; every value comes from the script.
func VideoFilter(scriptPath string) string {
	return fmt.Sprintf("sendcmd=f=%s,eq=brightness=0", escapeFilterPath(scriptPath))
}

// escapeFilterPath escapes the characters a filtergraph treats specially in an
// option value.
func escapeFilterPath(p string) string {
	r := strings.NewReplacer(`\`, `\\`, `:`, `\:`, `'`, `\'`, `,`, `\,`,
		`[`, `\[`, `]`, `\]`, `;`, `\;`)
	return r.Replace(p)
}
