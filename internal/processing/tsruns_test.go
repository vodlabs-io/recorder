package processing

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vodlabs-io/recorder/internal/ffmpeg"
)

// tsSegments encodes six seconds of test video as 2s MPEG-TS segments, each
// recording starting its clock afresh as a restarted encoder would.
func tsSegments(t *testing.T, ffPath string) [][]byte {
	t.Helper()
	dir := t.TempDir()
	args := []string{"-hide_banner", "-v", "error",
		"-f", "lavfi", "-i", "testsrc=size=320x240:rate=10", "-t", "6",
		"-c:v", "libx264", "-g", "20", "-pix_fmt", "yuv420p",
		"-f", "hls", "-hls_time", "2", "-hls_list_size", "0",
		"-hls_segment_filename", filepath.Join(dir, "s%03d.ts"),
		"-y", filepath.Join(dir, "index.m3u8")}
	if b, err := exec.CommandContext(t.Context(), ffPath, args...).CombinedOutput(); err != nil {
		t.Skipf("bundled ffmpeg cannot synthesise test media: %v: %s", err, b)
	}
	names, _ := filepath.Glob(filepath.Join(dir, "s*.ts"))
	var segs [][]byte
	for _, n := range names {
		b, err := os.ReadFile(n)
		if err != nil {
			t.Fatal(err)
		}
		segs = append(segs, b)
	}
	if len(segs) < 3 {
		t.Fatalf("want at least 3 segments, got %d", len(segs))
	}
	return segs
}

// writeTSPart lays the segments out as one hls writer part, in order, with no
// discontinuity tag: the origin restarted its encoder without saying so.
func writeTSPart(t *testing.T, partDir string, runs ...[][]byte) {
	t.Helper()
	files := map[string][]byte{}
	var lines []string
	seq := 0
	for _, run := range runs {
		for _, s := range run {
			name := fmt.Sprintf("%06d.ts", seq)
			seq++
			files[name] = s
			lines = append(lines, "#EXTINF:2.000,", name)
		}
	}
	writePart(t, partDir, files, lines...)
}

func TestTSResetRunsSplitsWhereTheClockRestarts(t *testing.T) {
	ffPath, err := ffmpeg.Path()
	if err != nil {
		t.Skipf("no bundled ffmpeg: %v", err)
	}
	segs := tsSegments(t, ffPath)
	partDir := filepath.Join(t.TempDir(), "pt1_720p")
	writeTSPart(t, partDir, segs, segs)

	runs, err := tsResetRuns(filepath.Join(partDir, "index.m3u8"))
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 2 {
		t.Fatalf("got %d stretches, want 2", len(runs))
	}
	for i, r := range runs {
		if n := strings.Count(mustRead(t, r.playlist), "#EXTINF"); n != len(segs) {
			t.Errorf("stretch %d lists %d segments, want %d", i, n, len(segs))
		}
	}

	// A clock that only moves forward stays one playlist.
	steady := filepath.Join(t.TempDir(), "pt1_720p")
	writeTSPart(t, steady, segs)
	runs, err = tsResetRuns(filepath.Join(steady, "index.m3u8"))
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].playlist != filepath.Join(steady, "index.m3u8") {
		t.Errorf("steady part split into %v", runs)
	}
}

// TestConcatGroupKeepsTheLengthAcrossAClockRestart pins the Tango case: a part
// whose timestamps start over mid-way used to come out at half its length and
// be rejected as short, leaving only the raw part folder.
func TestConcatGroupKeepsTheLengthAcrossAClockRestart(t *testing.T) {
	ffPath, err := ffmpeg.Path()
	if err != nil {
		t.Skipf("no bundled ffmpeg: %v", err)
	}
	if _, err := ffmpeg.ProbePath(); err != nil {
		t.Skipf("no bundled ffprobe: %v", err)
	}
	segs := tsSegments(t, ffPath)
	base := t.TempDir()
	partDir := filepath.Join(base, "pt1_720p")
	writeTSPart(t, partDir, segs, segs, segs)

	j := &job{groupInputs: []string{"pt1_720p"}, groupOutput: "vod.mp4"}
	j.vod.WriterName = "hlswriter"
	j.vod.BaseOutDir = base
	j.vod.Variant = "best"
	out := filepath.Join(base, "vod.mp4")
	if err := j.concatGroup(context.Background(), discardLogger(), j.groupInputPaths(), out); err != nil {
		t.Fatalf("concatGroup: %v", err)
	}
	probe, err := ffmpeg.Probe(context.Background(), discardLogger(), out)
	if err != nil {
		t.Fatal(err)
	}
	if probe.Duration < 17 || probe.Duration > 19 {
		t.Errorf("output %.1fs, want ~18s", probe.Duration)
	}
	left, _ := filepath.Glob(filepath.Join(partDir, "*.run*"))
	if len(left) != 0 {
		t.Errorf("intermediates left behind: %v", left)
	}
}

func mustRead(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
