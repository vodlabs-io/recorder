package encoding

import (
	"context"
	"errors"
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

func TestEnqueueConvertValidation(t *testing.T) {
	s := newTestService(t)
	ctx := context.Background()

	for _, tc := range []struct {
		name  string
		files []string
	}{
		{"empty folder", nil},
		{"mp4 only", []string{"vod.mp4"}},
		{"already converted", []string{"vod.webm", "vod.mp4"}},
		{"numbered parts", []string{"vod1.webm", "vod2.webm"}},
		{"single numbered part", []string{"vod1.webm"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := makeSession(t, tc.files...)
			if _, err := s.EnqueueConvert(ctx, dir, SessionMeta{}, true, "auto"); !errors.Is(err, ErrInvalid) {
				t.Fatalf("want ErrInvalid, got %v", err)
			}
		})
	}

	t.Run("single vod.webm", func(t *testing.T) {
		// Sidecars and hidden backups are not VOD files.
		dir := makeSession(t, "vod.webm", "vod.webm.jpg", "vod1.webm.bak", "session.json")
		job, err := s.EnqueueConvert(ctx, dir, SessionMeta{Platform: "manyvids", Username: "amy"}, false, "software")
		if err != nil {
			t.Fatalf("enqueue: %v", err)
		}
		if job.Type != jobTypeConvert {
			t.Errorf("type = %q, want %q", job.Type, jobTypeConvert)
		}
		if len(job.Inputs) != 1 || job.Inputs[0] != "vod.webm" || job.Output != "vod.mp4" {
			t.Errorf("inputs/output = %v → %q, want [vod.webm] → vod.mp4", job.Inputs, job.Output)
		}
		if job.DeleteParts || job.Encoder != "software" || job.Status != statusQueued {
			t.Errorf("row not persisted as requested: %+v", job)
		}
		if _, err := s.EnqueueConvert(ctx, dir, SessionMeta{}, false, "auto"); !errors.Is(err, ErrConflict) {
			t.Fatalf("want ErrConflict on duplicate, got %v", err)
		}
	})

	t.Run("conflicts with a queued merge", func(t *testing.T) {
		dir := makeSession(t, "vod.webm")
		insertJob(t, s, dir, statusQueued, "")
		if _, err := s.EnqueueConvert(ctx, dir, SessionMeta{}, false, "auto"); !errors.Is(err, ErrConflict) {
			t.Fatalf("want ErrConflict, got %v", err)
		}
	})
}

func TestReencodeArgsNameTheConvertWriter(t *testing.T) {
	plan := &mergePlan{parts: []partProbe{{path: "/v/vod.webm"}}, targetW: 640, targetH: 360, allAudio: true}
	convert := strings.Join(reencodeArgs(&Job{Type: jobTypeConvert, Output: "vod.mp4"}, plan, nil, 0, "/s/filter_complex.txt", "", "/s/vod.mp4"), " ")
	if !strings.Contains(convert, "writer=convert+reencode") {
		t.Errorf("convert args lack the convert writer: %s", convert)
	}
	merge := strings.Join(reencodeArgs(&Job{Type: jobTypeMerge, Output: "vod.mp4"}, plan, nil, 0, "/s/filter_complex.txt", "", "/s/vod.mp4"), " ")
	if !strings.Contains(merge, "writer=merge+reencode") {
		t.Errorf("merge args lack the merge writer: %s", merge)
	}
}

// TestConvertJobReencodesSingleFile runs a convert job end to end with the
// bundled ffmpeg. That build cannot encode VP8 or synthesise audio, so the
// source is video-only yuv444p H.264 under the vod.webm name (ffprobe reads
// the content, not the extension): a stream copy would keep yuv444p, so
// yuv420p in the output proves the re-encode.
func TestConvertJobReencodesSingleFile(t *testing.T) {
	isolateCache(t)
	ffPath, err := ffmpeg.Path()
	if err != nil {
		t.Skipf("no bundled ffmpeg: %v", err)
	}
	if _, err := ffmpeg.ProbePath(); err != nil {
		t.Skipf("no bundled ffprobe: %v", err)
	}

	dir := t.TempDir()
	src := filepath.Join(dir, "vod.webm")
	cmd := exec.CommandContext(t.Context(), ffPath,
		"-hide_banner", "-v", "error",
		"-f", "lavfi", "-i", "testsrc=size=320x240:rate=10",
		"-t", "2", "-c:v", "libx264", "-pix_fmt", "yuv444p", "-f", "mp4", "-y", src)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("bundled ffmpeg cannot synthesise test media: %v: %s", err, b)
	}
	if err := recording.WriteManifest(dir, recording.Manifest{
		Platform: "manyvids", Username: "amy", SessionID: "s1",
		Files: []recording.ManifestFile{{Name: "vod.webm", DurationSec: 2}},
	}); err != nil {
		t.Fatal(err)
	}

	s := newTestService(t)
	s.store = vlstate.NewStore(nil)
	ctx := context.Background()
	job, err := s.EnqueueConvert(ctx, dir, SessionMeta{Platform: "manyvids", Username: "amy", SessionID: "s1"}, false, "software")
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	defer s.cleanupStaging(job.ID)

	if _, err := s.runMergeJob(ctx, slog.New(slog.DiscardHandler), &job); err != nil {
		t.Fatalf("convert: %v", err)
	}

	res, err := ffmpeg.Probe(ctx, slog.New(slog.DiscardHandler), filepath.Join(dir, "vod.mp4"))
	if err != nil {
		t.Fatalf("probe output: %v", err)
	}
	if res.VideoCodec != "h264" || res.PixFmt != "yuv420p" {
		t.Errorf("output video = %s/%s, want h264/yuv420p", res.VideoCodec, res.PixFmt)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Errorf("vod.webm still listed: %v", err)
	}
	if _, err := os.Stat(src + bakSuffix); err != nil {
		t.Errorf("original not kept as .bak: %v", err)
	}
	m, err := recording.ReadManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Files) != 1 || m.Files[0].Name != "vod.mp4" {
		t.Errorf("manifest files = %+v, want only vod.mp4", m.Files)
	}
}
