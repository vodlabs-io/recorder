package dither

import (
	"fmt"
	"strings"
	"testing"
)

func TestPatternIsDeterministicAndUnbiased(t *testing.T) {
	a, b := New(VideoLabel), New(VideoLabel)
	ones := 0
	const n = 100000
	for i := range int64(n) {
		if a.Bit(i) != b.Bit(i) {
			t.Fatalf("pattern not deterministic at %d", i)
		}
		ones += int(a.Bit(i))
	}
	// A biased pattern would shift overall brightness.
	if ones < n*47/100 || ones > n*53/100 {
		t.Errorf("pattern biased: %d ones in %d", ones, n)
	}
}

func TestPatternsAreIndependentPerLabel(t *testing.T) {
	a, b := New(VideoLabel), New("other")
	same := 0
	for i := range int64(1000) {
		if a.Bit(i) == b.Bit(i) {
			same++
		}
	}
	if same > 600 || same < 400 {
		t.Errorf("labels agree on %d/1000 positions; they should be independent", same)
	}
}

func TestVideoScriptCoversTheClock(t *testing.T) {
	s := VideoScript(2.0)
	lines := strings.Split(strings.TrimSpace(s), "\n")
	// One command per interval, plus the one closing the final interval.
	if want := 2000/IntervalMs + 1; len(lines) != want {
		t.Fatalf("got %d commands for 2s, want %d", len(lines), want)
	}
	if !strings.HasPrefix(lines[0], "0.0000 eq brightness ") {
		t.Errorf("first command is %q", lines[0])
	}
	for _, l := range lines {
		if !strings.HasSuffix(l, ";") {
			t.Fatalf("command not terminated: %q", l)
		}
	}
	if VideoScript(0) != "" {
		t.Error("a zero-length clip should produce no commands")
	}
}

// valueAt is the brightness a script holds at t seconds.
func valueAt(t *testing.T, script string, at float64) string {
	t.Helper()
	val := ""
	for _, l := range strings.Split(strings.TrimSpace(script), "\n") {
		var when float64
		var v string
		if _, err := fmt.Sscanf(l, "%f eq brightness %s", &when, &v); err != nil {
			t.Fatalf("bad command %q: %v", l, err)
		}
		if when > at+1e-9 {
			break
		}
		val = v
	}
	return val
}

// Clips cut from a timeline at any point, even mid-interval, must hold the
// same offsets the whole timeline holds there.
func TestVideoScriptAtContinuesTheTimeline(t *testing.T) {
	full := VideoScript(10)
	for _, clip := range []struct{ start, dur float64 }{{0, 3.27}, {3.27, 4.05}, {7.32, 2.68}} {
		part := VideoScriptAt(clip.start, clip.dur)
		if !strings.HasPrefix(part, "0.0000 ") {
			t.Errorf("clip at %.2fs does not start at 0: %q", clip.start, part[:20])
		}
		for at := 0.0; at < clip.dur; at += 0.013 {
			if got, want := valueAt(t, part, at), valueAt(t, full, clip.start+at); got != want {
				t.Fatalf("clip at %.2fs, %.3fs in: %s, timeline has %s", clip.start, at, got, want)
			}
		}
	}
}

// A Windows drive colon must survive both unescaping passes, or the option
// list splits at it.
func TestVideoFilterEscapesWindowsPath(t *testing.T) {
	got := VideoFilter(`C:\enc\dither.txt`)
	want := `sendcmd=f=C\\:\\\\enc\\\\dither.txt,eq=brightness=0`
	if got != want {
		t.Errorf("VideoFilter = %s, want %s", got, want)
	}
}

func TestVideoFilterEscapesPaths(t *testing.T) {
	// A filtergraph treats these as structure; an unescaped path would change
	// the graph rather than name a file.
	got := VideoFilter(`/tmp/a:b,c[d]/dither.txt`)
	path := strings.TrimSuffix(strings.TrimPrefix(got, "sendcmd=f="), ",eq=brightness=0")
	for i, c := range path {
		if !strings.ContainsRune(`:,[];'\`, c) {
			continue
		}
		if c == '\\' {
			continue
		}
		if i == 0 || path[i-1] != '\\' {
			t.Errorf("unescaped %q at %d in %q", c, i, path)
		}
	}
	if !strings.HasPrefix(got, "sendcmd=f=") || !strings.HasSuffix(got, ",eq=brightness=0") {
		t.Errorf("unexpected filter shape: %q", got)
	}
}
