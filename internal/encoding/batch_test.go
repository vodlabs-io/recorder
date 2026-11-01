package encoding

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vodlabs-io/recorder/internal/ffmpeg"
	"github.com/vodlabs-io/recorder/internal/recording"
	"github.com/vodlabs-io/recorder/internal/vlstate"
)

// isolateCache points the user cache dir, which holds staging, at a temp dir
// so a test never touches the real daemon's.
func isolateCache(t *testing.T) {
	t.Helper()
	// The binaries are extracted first: their paths are resolved once per
	// process, and a temp dir would not outlive this test.
	_, _ = ffmpeg.Path()
	_, _ = ffmpeg.ProbePath()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CACHE_HOME", dir)
	t.Setenv("LocalAppData", dir)
}

func TestBatchParts(t *testing.T) {
	parts := make([]partProbe, 5)
	for _, tc := range []struct {
		n    int
		want []int
	}{{2, []int{2, 2, 1}}, {5, []int{5}}, {40, []int{5}}} {
		var got []int
		for _, b := range batchParts(parts, tc.n) {
			got = append(got, len(b))
		}
		if fmt.Sprint(got) != fmt.Sprint(tc.want) {
			t.Errorf("batchParts(5, %d) sizes = %v, want %v", tc.n, got, tc.want)
		}
	}
}

func TestOutputFrameRate(t *testing.T) {
	for _, tc := range []struct {
		name    string
		plan    mergePlan
		encoder string
		want    float64
	}{
		{"software keeps the parts' timing", mergePlan{frameRate: 29.97}, softwareEncoder, 0},
		{"other hardware keeps the parts' timing", mergePlan{frameRate: 29.97}, "h264_nvenc", 0},
		{"media foundation takes the parts' rate", mergePlan{frameRate: 29.97}, "h264_mf", 29.97},
		{"media foundation falls back without one", mergePlan{}, "h264_mf", defaultFrameRate},
		{"vp8 always runs at its measured rate", mergePlan{vp8FrameRate: 24, frameRate: 30}, softwareEncoder, 24},
	} {
		if got := outputFrameRate(&tc.plan, tc.encoder); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestReencodeArgsConstantFrameRate(t *testing.T) {
	plan := &mergePlan{parts: []partProbe{{path: "/v/vod1.mp4"}}, targetW: 640, targetH: 360, allAudio: true}
	job := &Job{SessionID: "s1", Output: "vod.mp4"}
	if got := strings.Join(reencodeArgs(job, plan, nil, 29.97, "/s/f.txt", "", "/s/vod.mp4"), " "); !strings.Contains(got, "-fps_mode cfr -r 29.97") {
		t.Errorf("args lack the constant rate: %s", got)
	}
	if got := strings.Join(reencodeArgs(job, plan, nil, 0, "/s/f.txt", "", "/s/vod.mp4"), " "); strings.Contains(got, "-fps_mode") {
		t.Errorf("args force a rate: %s", got)
	}
}

// TestMergeJobReencodesInBatches runs a merge of three mismatched parts with
// a batch size of two, end to end with the bundled ffmpeg: the parts are
// re-encoded as two batches and the batches stream-copied into one file.
func TestMergeJobReencodesInBatches(t *testing.T) {
	isolateCache(t)
	ffPath, err := ffmpeg.Path()
	if err != nil {
		t.Skipf("no bundled ffmpeg: %v", err)
	}
	if _, err := ffmpeg.ProbePath(); err != nil {
		t.Skipf("no bundled ffprobe: %v", err)
	}
	old := reencodeBatchParts
	reencodeBatchParts = 2
	t.Cleanup(func() { reencodeBatchParts = old })

	dir := t.TempDir()
	var files []recording.ManifestFile
	for i, size := range []string{"320x240", "160x120", "320x240"} {
		name := fmt.Sprintf("vod%d.mp4", i+1)
		cmd := exec.CommandContext(t.Context(), ffPath,
			"-hide_banner", "-v", "error",
			"-f", "lavfi", "-i", "testsrc=size="+size+":rate=10",
			"-t", "2", "-c:v", "libx264", "-pix_fmt", "yuv420p", "-f", "mp4", "-y", filepath.Join(dir, name))
		if b, err := cmd.CombinedOutput(); err != nil {
			t.Skipf("bundled ffmpeg cannot synthesise test media: %v: %s", err, b)
		}
		files = append(files, recording.ManifestFile{Name: name, DurationSec: 2})
	}
	if err := recording.WriteManifest(dir, recording.Manifest{
		Platform: "tiktok", Username: "amy", SessionID: "s1", Files: files,
	}); err != nil {
		t.Fatal(err)
	}

	s := newTestService(t)
	s.store = vlstate.NewStore(nil)
	ctx := context.Background()
	job, err := s.EnqueueMerge(ctx, dir, SessionMeta{Platform: "tiktok", Username: "amy", SessionID: "s1"}, false, "software")
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	defer s.cleanupStaging(job.ID)

	if _, err := s.runMergeJob(ctx, slog.New(slog.DiscardHandler), &job); err != nil {
		t.Fatalf("merge: %v", err)
	}

	res, err := ffmpeg.Probe(ctx, slog.New(slog.DiscardHandler), filepath.Join(dir, "vod.mp4"))
	if err != nil {
		t.Fatalf("probe output: %v", err)
	}
	if res.Width != 320 || res.Height != 240 {
		t.Errorf("output is %dx%d, want the 320x240 canvas", res.Width, res.Height)
	}
	if res.Duration < 5.5 || res.Duration > 6.5 {
		t.Errorf("output lasts %.2fs, want about 6s", res.Duration)
	}
	staging, err := stagingDir(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if left, _ := filepath.Glob(filepath.Join(staging, "batch*")); len(left) != 0 {
		t.Errorf("batches left in staging: %v", left)
	}
	log, _ := os.ReadFile(filepath.Join(dir, encodeLogName))
	if !strings.Contains(string(log), "Merge job") {
		t.Errorf("ffmpeg.log lacks the job header")
	}
}
