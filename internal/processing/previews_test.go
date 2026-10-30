package processing

import (
	"bytes"
	"context"
	"image/jpeg"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/vodlabs-io/recorder/internal/ffmpeg"
)

func previewFixture(t *testing.T, w, h int, dur time.Duration, gop int) string {
	t.Helper()
	dir := t.TempDir()
	out := filepath.Join(dir, "vod.mp4")
	cmd, err := ffmpeg.Command(context.Background(), slog.Default(),
		"-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc=size="+strconv.Itoa(w)+"x"+strconv.Itoa(h)+":rate=30:duration="+strconv.Itoa(int(dur.Seconds())),
		"-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p",
		"-g", strconv.Itoa(gop*30), "-keyint_min", strconv.Itoa(gop*30), "-sc_threshold", "0", out)
	if err != nil {
		t.Skip(err)
	}
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("no usable ffmpeg: %v %s", err, b)
	}
	return out
}

func runPreviews(t *testing.T, video string) (PreviewsManifest, map[string][]byte) {
	t.Helper()
	files := map[string][]byte{}
	m, err := GeneratePreviews(context.Background(), slog.Default(), video, ThumbnailName(filepath.Base(video)),
		PreviewHeader{Platform: "tiktok", Username: "someone", Start: time.Date(2026, 10, 28, 22, 32, 0, 0, time.UTC)},
		func(name string, data []byte) error { files[name] = data; return nil })
	if err != nil {
		t.Fatal(err)
	}
	return m, files
}

func TestGeneratePreviewsLandscape(t *testing.T) {
	video := previewFixture(t, 1280, 720, 3*time.Minute, 1)
	m, files := runPreviews(t, video)
	if m.Portrait || m.IntervalMs != 1000 || m.Tile != [2]int{256, 144} {
		t.Fatalf("manifest = %+v", m)
	}
	// 3 min of 1 s keyframes previews every keyframe, read from the index.
	if m.Frames != 180 || m.Sheets != 3 {
		t.Fatalf("frames=%d sheets=%d", m.Frames, m.Sheets)
	}
	vtt := string(files["vod.thumbs.vtt"])
	if !strings.HasPrefix(vtt, "WEBVTT\n\n00:00:00.000 --> 00:00:01.000\nvod.thumbs.000.jpg#xywh=0,0,256,144\n") {
		t.Fatalf("vtt starts %q", vtt[:min(len(vtt), 120)])
	}
	if !strings.Contains(vtt, "vod.thumbs.001.jpg#xywh=0,0,256,144") || !strings.HasSuffix(vtt, "\n") {
		t.Fatal("second sheet missing from vtt")
	}
	sheet := decodeSize(t, files["vod.thumbs.000.jpg"])
	if sheet != [2]int{2048, 1152} {
		t.Fatalf("sheet 0 is %v", sheet)
	}
	if cs := decodeSize(t, files["vod.sheet.jpg"]); cs[0] != 1495 || cs[1] < 900 || len(files["vod.sheet.jpg"]) > 700<<10 {
		t.Fatalf("contact sheet %v, %d bytes", cs, len(files["vod.sheet.jpg"]))
	}
	if _, ok := files["vod.jpg"]; ok {
		t.Fatal("landscape video got a triptych thumbnail")
	}
	if len(m.Scrub.TimesMs) != scrubFrames {
		t.Fatalf("scrub has %d frames", len(m.Scrub.TimesMs))
	}
	if _, ok := files["vod.previews.json"]; !ok {
		t.Fatal("no manifest")
	}
}

func TestGeneratePreviewsPortraitSparseKeys(t *testing.T) {
	// Keyframes every 9 s on a short video: decoded in full for 1 s previews.
	video := previewFixture(t, 360, 640, 60*time.Second, 9)
	m, files := runPreviews(t, video)
	if !m.Portrait || m.Tile != [2]int{144, 256} || m.Frames != 60 {
		t.Fatalf("manifest = %+v", m)
	}
	if got := decodeSize(t, files["vod.jpg"]); got != [2]int{480, 270} {
		t.Fatalf("triptych is %v", got)
	}
	if cs := decodeSize(t, files["vod.sheet.jpg"]); cs[0] < cs[1] {
		t.Fatalf("portrait contact sheet is taller than wide: %v", cs)
	}
}

// TestGeneratePreviewsSample runs on a real recording when
// PREVIEW_SAMPLE names one, and writes its previews next to it in a temp
// folder for a look.
func TestGeneratePreviewsSample(t *testing.T) {
	video := os.Getenv("PREVIEW_SAMPLE")
	if video == "" {
		t.Skip("PREVIEW_SAMPLE not set")
	}
	out := os.Getenv("PREVIEW_OUT")
	if out == "" {
		out = t.TempDir()
	}
	start := time.Now()
	m, err := GeneratePreviews(context.Background(), slog.Default(), video, "vod.jpg", PreviewHeader{Platform: "tiktok", Username: "sample"},
		func(name string, data []byte) error { return os.WriteFile(filepath.Join(out, name), data, 0o644) })
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%+v in %v -> %s", m, time.Since(start), out)
}

func decodeSize(t *testing.T, b []byte) [2]int {
	t.Helper()
	cfg, err := jpeg.DecodeConfig(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	return [2]int{cfg.Width, cfg.Height}
}
