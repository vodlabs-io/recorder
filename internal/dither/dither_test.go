package dither

import (
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
