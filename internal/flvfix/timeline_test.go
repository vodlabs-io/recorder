package flvfix

import (
	"bytes"
	"testing"
)

// runGrid sanitizes src with the output grid active, as production does.
func runGrid(t *testing.T, src []byte) (*bytes.Buffer, *Sanitizer) {
	t.Helper()
	var out bytes.Buffer
	s := New(&out, nil, Options{})
	if _, err := s.Write(src); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := s.Finish(); err != nil {
		t.Fatalf("finish: %v", err)
	}
	return &out, s
}

// onGrid reports how many of ts sit at their grid phase.
func onGrid(ts []int64) int {
	var g phaseGrid
	n := 0
	for _, t := range ts {
		if t >= 0 && t&1 == g.bit(t>>1) {
			n++
		}
	}
	return n
}

func TestPhaseAdjustProperties(t *testing.T) {
	var g phaseGrid
	for ts := int64(0); ts < 20000; ts++ {
		got := g.adjust(ts, -1)
		if d := got - ts; d < -1 || d > 1 {
			t.Fatalf("adjust(%d) = %d, moved by %d (want at most 1ms)", ts, got, d)
		}
		if got&1 != g.bit(got>>1) {
			t.Fatalf("adjust(%d) = %d is not on the grid", ts, got)
		}
		// Idempotent: re-running over already-adjusted output is a no-op.
		if again := g.adjust(got, -1); again != got {
			t.Fatalf("adjust not idempotent: %d -> %d -> %d", ts, got, again)
		}
	}
}

func TestPhaseAdjustNeverBreaksMonotonicity(t *testing.T) {
	var g phaseGrid
	// A floor equal to the snapped value must leave the timestamp alone rather
	// than emit a duplicate or a backward step.
	for ts := int64(0); ts < 5000; ts++ {
		for _, floor := range []int64{ts - 1, ts, ts + 1} {
			if got := g.adjust(ts, floor); got <= floor && got != ts {
				t.Fatalf("adjust(%d, floor=%d) = %d: not above floor and not the original",
					ts, floor, got)
			}
		}
	}
}

func TestPhaseGridIsUnbiased(t *testing.T) {
	var g phaseGrid
	ones := 0
	const n = 100000
	for q := int64(0); q < n; q++ {
		ones += int(g.bit(q))
	}
	// Each phase should be selected about half the time.
	if ones < n*47/100 || ones > n*53/100 {
		t.Fatalf("phase bits biased: %d ones in %d", ones, n)
	}
}

func TestGridCachedAndUncachedAgree(t *testing.T) {
	var g phaseGrid
	for _, q := range []int64{0, 1, 63, 64, 65, 1023, 4096, 999999} {
		want := int64(phaseWord(q/phaseWordBits)>>uint(q%phaseWordBits)) & 1
		if got := g.bit(q); got != want {
			t.Fatalf("bit(%d) = %d, want %d", q, got, want)
		}
	}
}

func TestVideoLandsOnGridAudioUntouched(t *testing.T) {
	src := stream(fileHeader(), vCfgTag(0, 0x41, 22), aCfgTag(0), gopSegment(0, 20000))

	out, s := runGrid(t, src)
	tags := parseOutput(t, out.Bytes())
	vts := mediaTimestamps(tags, tagVideo)
	if len(vts) < 100 {
		t.Fatalf("need a meaningful sample, got %d video tags", len(vts))
	}
	if n := onGrid(vts); n != len(vts) {
		t.Errorf("video on grid: %d/%d", n, len(vts))
	}
	if s.Stats().tsSnapped == 0 {
		t.Error("no timestamp was snapped; the grid is not being applied")
	}

	// Audio must be byte-identical to a run with the grid off: the segmenter
	// anchors a whole audio segment off its first frame, so moving one audio
	// tag would shift all of them.
	var rawOut bytes.Buffer
	raw := New(&rawOut, nil, Options{})
	raw.rawTimestamps = true
	if _, err := raw.Write(src); err != nil {
		t.Fatal(err)
	}
	if err := raw.Finish(); err != nil {
		t.Fatal(err)
	}
	gotA := mediaTimestamps(tags, tagAudio)
	wantA := mediaTimestamps(parseOutput(t, rawOut.Bytes()), tagAudio)
	if len(gotA) != len(wantA) {
		t.Fatalf("audio tag count changed: %d vs %d", len(gotA), len(wantA))
	}
	for i := range gotA {
		if gotA[i] != wantA[i] {
			t.Fatalf("audio timestamp %d changed: %d vs %d", i, gotA[i], wantA[i])
		}
	}
}

func TestRawTimestampsSitOnGridAtChance(t *testing.T) {
	// Without the grid, roughly half the timestamps sit at their phase by
	// coincidence.
	src := stream(fileHeader(), vCfgTag(0, 0x41, 22), aCfgTag(0), gopSegment(0, 60000))
	var out bytes.Buffer
	s := New(&out, nil, Options{})
	s.rawTimestamps = true
	if _, err := s.Write(src); err != nil {
		t.Fatal(err)
	}
	if err := s.Finish(); err != nil {
		t.Fatal(err)
	}
	vts := mediaTimestamps(parseOutput(t, out.Bytes()), tagVideo)
	rate := float64(onGrid(vts)) / float64(len(vts))
	if rate > 0.75 {
		t.Errorf("raw stream on grid at rate %.3f over %d tags, want near 0.5", rate, len(vts))
	}
}

// The grid must survive every branch that rewrites a timestamp: gap glue,
// timeline reset, replay dedup and the config-change rotation.
func TestGridHoldsAcrossTimelineCorrections(t *testing.T) {
	for _, tc := range []struct {
		name string
		src  []byte
	}{
		{"forward gap", stream(fileHeader(), vCfgTag(0, 0x41, 22), aCfgTag(0),
			gopSegment(0, 5000), gopSegment(20000, 25000))},
		{"timeline reset", stream(fileHeader(), vCfgTag(0, 0x41, 22), aCfgTag(0),
			gopSegment(100000, 105000), gopSegment(0, 5000))},
		{"replayed segment", stream(fileHeader(), vCfgTag(0, 0x41, 22), aCfgTag(0),
			gopSegment(0, 10000), gopSegment(5000, 15000))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, _ := runGrid(t, tc.src)
			tags := parseOutput(t, out.Bytes())
			checkInvariants(t, tags)
			vts := mediaTimestamps(tags, tagVideo)
			if n := onGrid(vts); n != len(vts) {
				t.Errorf("video on grid: %d/%d", n, len(vts))
			}
		})
	}
}

func TestRepairsDisabledStillLandsOnGrid(t *testing.T) {
	// Turning repairs off must not take the stream off the byte path: the
	// output is still re-framed onto the grid, it is just not repaired.
	src := stream(fileHeader(), vCfgTag(0, 0x41, 22), aCfgTag(0),
		gopSegment(0, 5000), gopSegment(20000, 25000))

	var out bytes.Buffer
	s := New(&out, nil, Options{RepairsDisabled: true})
	if _, err := s.Write(src); err != nil {
		t.Fatal(err)
	}
	if err := s.Finish(); err != nil {
		t.Fatal(err)
	}
	tags := parseOutput(t, out.Bytes())
	checkInvariants(t, tags)

	vts := mediaTimestamps(tags, tagVideo)
	if n := onGrid(vts); n != len(vts) {
		t.Errorf("video on grid: %d/%d", n, len(vts))
	}
	// The forward gap is the stream's own and must survive unglued.
	if st := s.Stats(); st.gapsGlued != 0 {
		t.Errorf("repairs disabled but %d gap(s) glued", st.gapsGlued)
	}
	if span := vts[len(vts)-1] - vts[0]; span < 20000 {
		t.Errorf("gap was removed: video spans only %dms", span)
	}
}

// Known-answer vectors for the output grid. A change that breaks these has
// moved the grid; add a new grid version instead of editing them.
const (
	katBits   = "00100111101000011100011101011100"
	katOnGrid = "10100110100101010110011010101001"
)

func TestKnownAnswerVectors(t *testing.T) {
	var g phaseGrid
	for q, want := range katBits {
		if got := g.bit(int64(q)); got != int64(want-'0') {
			t.Fatalf("bit(%d) = %d, want %c", q, got, want)
		}
	}
	for ts, want := range katOnGrid {
		on := int64(ts)&1 == g.bit(int64(ts)>>1)
		if on != (want == '1') {
			t.Fatalf("ts %d on grid = %v, want %v", ts, on, want == '1')
		}
	}
}
