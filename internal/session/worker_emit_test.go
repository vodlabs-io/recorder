package session

import (
	"fmt"
	"io"
	"log/slog"
	"testing"

	"github.com/vodlabs-io/recorder/internal/recording"
	"github.com/vodlabs-io/recorder/internal/writer"
)

// emitWorker builds a worker wired only for exercising the split-group emission
// path: a buffered pendingRecordingsCh, a discard logger, and a real (I/O-free) writer
// selected by name so recordGroupPart can branch on the writer.
func emitWorker(t *testing.T, writerName string) (*worker, chan recording.Job) {
	t.Helper()
	wr, ok := writer.Get(writerName)
	if !ok {
		t.Fatalf("writer %q not registered", writerName)
	}
	ch := make(chan recording.Job, 64)
	w := &worker{
		logger:              slog.New(slog.NewTextHandler(io.Discard, nil)),
		writer:              wr,
		pendingRecordingsCh: ch,
	}
	w.output.WriterName = writerName
	w.output.BaseOutDir = "/rec/tiktok/alice/sess"
	w.output.Platform = "tiktok"
	w.output.Username = "alice"
	w.output.SessionID = "sess"
	return w, ch
}

// completePart simulates a finished part of the given size and folds it into the
// current group; splitRotate=true marks the part that reached the split limit,
// attributed (as the live ticker would) to the group current at that moment.
func (w *worker) completePart(stem string, bytes int64, splitRotate bool) {
	w.completePartSplitting(stem, bytes, splitRotate, w.curPartKey)
}

// completePartSplitting is completePart with an explicit split-attribution key,
// for the case where the config changed mid-part and the group the split fired
// against is not the group the part joins. It follows the run loop's order:
// fold the part in, then close whatever group the split named.
func (w *worker) completePartSplitting(stem string, bytes int64, splitRotate bool, splitOn string) {
	w.curStem = stem
	w.output.CurrentPart.Index++
	w.output.CurrentPart.Bytes = bytes
	w.recordGroupPart()
	if splitRotate {
		w.closeGroup(splitOn, false)
	}
}

// rejectPart simulates a part dropped by the MinPartBytes guard while a split
// was pending: it joins no group, and the run loop closes the named group
// regardless.
func (w *worker) rejectPart(splitOn string) {
	w.closeGroup(splitOn, false)
}

type emitted struct {
	kind   string // "convert" | "group" | "finalize"
	output string
	inputs []string
}

func drain(ch chan recording.Job) []emitted {
	close(ch)
	var got []emitted
	for m := range ch {
		switch m.Kind {
		case recording.JobFinalize:
			got = append(got, emitted{kind: "finalize"})
		case recording.JobGroup:
			got = append(got, emitted{kind: "group", output: m.GroupOutput, inputs: m.GroupInputs})
		default:
			got = append(got, emitted{kind: "convert"})
		}
	}
	return got
}

func eqStrs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestEmitFLVMultiGroup(t *testing.T) {
	w, ch := emitWorker(t, "flvwriter")
	const gb = 1 << 30

	// Group 1 fills to the limit across three parts; group 2 is the trailing part.
	w.completePart("pt1_best", gb, false)
	w.completePart("pt2_best", gb, false)
	w.completePart("pt3_best", 2*gb, true) // cumulative 4GB closes the group
	w.completePart("pt4_best", gb/2, false)
	w.finishSession()

	got := drain(ch)
	want := []emitted{
		{kind: "convert"},
		{kind: "convert"},
		{kind: "convert"},
		{kind: "group", output: "vod1.mp4", inputs: []string{"pt1_best", "pt2_best", "pt3_best"}},
		{kind: "convert"},
		{kind: "group", output: "vod2.mp4", inputs: []string{"pt4_best"}},
		{kind: "finalize"},
	}
	assertEmitted(t, got, want)
}

func TestEmitFLVConfigChangeStartsNewGroup(t *testing.T) {
	w, ch := emitWorker(t, "flvwriter")
	// Codec config flip-flop A→B→A. A group spans one contiguous run of
	// parts, so the returning config opens a third group rather than
	// extending the first: vod1..vod3 replay in recording order, and a merge
	// that concatenates them in name order reproduces the broadcast.
	w.curPartKey = "cfgA"
	w.completePart("pt1_best", 1<<30, false)
	w.curPartKey = "cfgB"
	w.completePart("pt2_best", 1<<30, false)
	w.curPartKey = "cfgA"
	w.completePart("pt3_best", 1<<30, false)
	w.finishSession()

	got := drain(ch)
	want := []emitted{
		{kind: "convert"},
		{kind: "group", output: "vod1.mp4", inputs: []string{"pt1_best"}},
		{kind: "convert"},
		{kind: "group", output: "vod2.mp4", inputs: []string{"pt2_best"}},
		{kind: "convert"},
		{kind: "group", output: "vod3.mp4", inputs: []string{"pt3_best"}},
		{kind: "finalize"},
	}
	assertEmitted(t, got, want)
}

func TestEmitFLVGroupsPartitionPartsInOrder(t *testing.T) {
	w, ch := emitWorker(t, "flvwriter")
	// The property the merge depends on: concatenating the emitted groups in
	// vodN order replays every part exactly once, in recording order. Driven
	// by a config sequence that revisits earlier configs repeatedly, the shape
	// a platform switching renditions produces.
	stems := []string{}
	for i, key := range []string{"A", "B", "A", "A", "C", "B", "A", "C", "C", "B"} {
		stem := fmt.Sprintf("pt%d_best", i+1)
		stems = append(stems, stem)
		w.curPartKey = key
		w.completePart(stem, 1<<20, false)
	}
	w.finishSession()

	var got []string
	for _, m := range drain(ch) {
		if m.kind == "group" {
			got = append(got, m.inputs...)
		}
	}
	if !eqStrs(got, stems) {
		t.Errorf("concatenated group inputs = %v, want %v", got, stems)
	}
}

func TestEmitFLVSplitAttributedAcrossConfigChange(t *testing.T) {
	w, ch := emitWorker(t, "flvwriter")
	// The mid-part split check measures the open group, because a part's
	// config key arrives only as it finishes. Here cfgA's group reaches its
	// limit while a part that turns out to be cfgB is recording: cfgA's group
	// is already closed by the time the split is applied, so the guard leaves
	// cfgB's new group open for its own next part rather than cutting it to
	// one part.
	w.curPartKey = "cfgA"
	w.completePart("pt1_best", 1<<30, false)
	w.completePart("pt2_best", 1<<30, false)
	w.curPartKey = "cfgB"
	w.completePartSplitting("pt3_best", 1<<20, true, "cfgA")
	w.finishSession()

	got := drain(ch)
	want := []emitted{
		{kind: "convert"},
		{kind: "convert"},
		{kind: "group", output: "vod1.mp4", inputs: []string{"pt1_best", "pt2_best"}},
		{kind: "convert"},
		{kind: "group", output: "vod2.mp4", inputs: []string{"pt3_best"}},
		{kind: "finalize"},
	}
	assertEmitted(t, got, want)
}

func TestEmitFLVSplitOnRejectedPart(t *testing.T) {
	w, ch := emitWorker(t, "flvwriter")
	// A split fires during a part that then falls under MinPartBytes. The
	// dropped part joins no group, but the group that reached its limit is
	// still emitted rather than held open to the end of the session.
	w.completePart("pt1_best", 1<<30, false)
	w.rejectPart(w.curPartKey)
	w.completePart("pt3_best", 1<<30, false)
	w.finishSession()

	got := drain(ch)
	want := []emitted{
		{kind: "convert"},
		{kind: "group", output: "vod1.mp4", inputs: []string{"pt1_best"}},
		{kind: "convert"},
		{kind: "group", output: "vod2.mp4", inputs: []string{"pt3_best"}},
		{kind: "finalize"},
	}
	assertEmitted(t, got, want)
}

func TestEmitFLVSingleGroupNoSplit(t *testing.T) {
	w, ch := emitWorker(t, "flvwriter")
	// No split ever fires: the whole session is one undecorated vod.mp4.
	w.completePart("pt1_best", 1<<30, false)
	w.completePart("pt2_best", 1<<30, false)
	w.finishSession()

	got := drain(ch)
	want := []emitted{
		{kind: "convert"},
		{kind: "convert"},
		{kind: "group", output: "vod.mp4", inputs: []string{"pt1_best", "pt2_best"}},
		{kind: "finalize"},
	}
	assertEmitted(t, got, want)
}

func TestEmitHLSNoPerPartConvert(t *testing.T) {
	w, ch := emitWorker(t, "hlswriter")
	// HLS parts feed the group directly — no per-part convert messages.
	w.completePart("pt1_best", 1<<30, false)
	w.completePart("pt2_best", 2<<30, true) // closes group 1
	w.completePart("pt3_best", 1<<29, false)
	w.finishSession()

	got := drain(ch)
	want := []emitted{
		{kind: "group", output: "vod1.mp4", inputs: []string{"pt1_best", "pt2_best"}},
		{kind: "group", output: "vod2.mp4", inputs: []string{"pt3_best"}},
		{kind: "finalize"},
	}
	assertEmitted(t, got, want)
}

func TestEmitWebMNoPerPartConvert(t *testing.T) {
	w, ch := emitWorker(t, "webmwriter")
	// WebM parts are joined as recorded: no per-part convert, .webm groups.
	w.completePart("pt1_best", 1<<30, false)
	w.completePart("pt2_best", 2<<30, true) // closes group 1
	w.completePart("pt3_best", 1<<29, false)
	w.finishSession()

	got := drain(ch)
	want := []emitted{
		{kind: "group", output: "vod1.webm", inputs: []string{"pt1_best", "pt2_best"}},
		{kind: "group", output: "vod2.webm", inputs: []string{"pt3_best"}},
		{kind: "finalize"},
	}
	assertEmitted(t, got, want)
}

func TestEmitWebMSoleGroup(t *testing.T) {
	w, ch := emitWorker(t, "webmwriter")
	w.completePart("pt1_best", 1<<29, false)
	w.finishSession()

	got := drain(ch)
	want := []emitted{
		{kind: "group", output: "vod.webm", inputs: []string{"pt1_best"}},
		{kind: "finalize"},
	}
	assertEmitted(t, got, want)
}

func assertEmitted(t *testing.T, got, want []emitted) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("message count = %d, want %d\n got: %+v\nwant: %+v", len(got), len(want), got, want)
	}
	for i := range want {
		if got[i].kind != want[i].kind || got[i].output != want[i].output || !eqStrs(got[i].inputs, want[i].inputs) {
			t.Errorf("message %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}
