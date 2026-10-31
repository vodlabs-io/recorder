package processing

import (
	"bytes"
	"context"
	"errors"
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
	return previewFixtureAt(t, t.TempDir(), "vod.mp4", w, h, dur, gop)
}

func previewFixtureAt(t *testing.T, dir, name string, w, h int, dur time.Duration, gop int) string {
	t.Helper()
	return previewFixtureBrightness(t, dir, name, w, h, dur, gop, 0)
}

// previewFixtureBrightness shifts the test pattern's brightness by b, in
// [-1, 1].
func previewFixtureBrightness(t *testing.T, dir, name string, w, h int, dur time.Duration, gop int, b float64) string {
	t.Helper()
	out := filepath.Join(dir, name)
	cmd, err := ffmpeg.Command(context.Background(), slog.Default(),
		"-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc=size="+strconv.Itoa(w)+"x"+strconv.Itoa(h)+":rate=30:duration="+strconv.Itoa(int(dur.Seconds())),
		"-vf", "eq=brightness="+strconv.FormatFloat(b, 'f', 2, 64),
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
	vtt := string(files[StoryboardName("vod.mp4")])
	if !strings.HasPrefix(vtt, "WEBVTT\n\n00:00:00.000 --> 00:00:01.000\nthumbs.000.jpg#xywh=0,0,256,144\n") {
		t.Fatalf("vtt starts %q", vtt[:min(len(vtt), 120)])
	}
	if !strings.Contains(vtt, "\nthumbs.001.jpg#xywh=0,0,256,144") || !strings.HasSuffix(vtt, "\n") {
		t.Fatal("second sheet missing from vtt")
	}
	sheet := decodeSize(t, files[StoryboardSheetName("vod.mp4", 0)])
	if sheet != [2]int{2048, 1152} {
		t.Fatalf("sheet 0 is %v", sheet)
	}
	if _, ok := files[ThumbnailName("vod.mp4")]; ok {
		t.Fatal("landscape video got a triptych thumbnail")
	}
	if len(m.Scrub.TimesMs) != scrubFrames {
		t.Fatalf("scrub has %d frames", len(m.Scrub.TimesMs))
	}
	if _, ok := files[PreviewsManifestName("vod.mp4")]; !ok {
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
	img, err := jpeg.Decode(bytes.NewReader(files[ThumbnailName("vod.mp4")]))
	if err != nil {
		t.Fatal(err)
	}
	if b := img.Bounds(); b.Dx() != 480 || b.Dy() != 270 {
		t.Fatalf("triptych is %v", b)
	}
	// Panels meet edge to edge: no all-black bar at or beside a seam.
	for _, x := range []int{0, 159, 160, 319, 320, 479} {
		black := true
		for y := range 270 {
			if r, g, b, _ := img.At(x, y).RGBA(); r+g+b > 3*0x1000 {
				black = false
				break
			}
		}
		if black {
			t.Fatalf("column %d is a black bar", x)
		}
	}
}

func TestSessionSheetSpansFiles(t *testing.T) {
	dir := t.TempDir()
	var files []SheetFile
	for i, d := range []time.Duration{20 * time.Second, 5 * time.Second, 40 * time.Second} {
		name := "vod" + strconv.Itoa(i+1) + ".mp4"
		video := previewFixtureAt(t, dir, name, 1280, 720, d, 1)
		if i != 1 { // a file without previews still counts
			if _, err := WritePreviews(context.Background(), slog.Default(), dir, dir, name, PreviewHeader{}); err != nil {
				t.Fatal(err)
			}
		}
		st, err := os.Stat(video)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, SheetFile{Name: name, Bytes: st.Size()})
	}
	hdr := PreviewHeader{Platform: "tiktok", Username: "someone", Start: time.Date(2026, 10, 28, 22, 32, 0, 0, time.UTC)}
	sheet, err := WriteSessionSheet(context.Background(), slog.Default(), dir, dir, files, hdr)
	if err != nil {
		t.Fatal(err)
	}
	if cs := decodeSize(t, sheet); cs[0] != 1495 || cs[1] < 900 || len(sheet) > 700<<10 {
		t.Fatalf("contact sheet %v, %d bytes", cs, len(sheet))
	}
	if !SessionSheetCurrent(dir, files) {
		t.Fatal("sheet not current for the files it was built from")
	}
	if SessionSheetCurrent(dir, files[:2]) {
		t.Fatal("sheet still current after a file went away")
	}
}

func TestSessionThumbnailSpansFiles(t *testing.T) {
	dir := t.TempDir()
	// Dark, plain and bright parts: the thirds of the 60 s session land in
	// one each.
	var files []SheetFile
	for i, p := range []struct {
		dur        time.Duration
		brightness float64
	}{{30 * time.Second, -0.6}, {20 * time.Second, 0}, {10 * time.Second, 0.6}} {
		name := "vod" + strconv.Itoa(i+1) + ".mp4"
		video := previewFixtureBrightness(t, dir, name, 360, 640, p.dur, 1, p.brightness)
		if _, err := WritePreviews(context.Background(), slog.Default(), dir, dir, name, PreviewHeader{}); err != nil {
			t.Fatal(err)
		}
		st, err := os.Stat(video)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, SheetFile{Name: name, Bytes: st.Size()})
	}
	thumb, err := WriteSessionThumbnail(context.Background(), slog.Default(), dir, dir, files)
	if err != nil {
		t.Fatal(err)
	}
	img, err := jpeg.Decode(bytes.NewReader(thumb))
	if err != nil {
		t.Fatal(err)
	}
	if b := img.Bounds(); b.Dx() != 480 || b.Dy() != 270 {
		t.Fatalf("triptych is %v", b)
	}
	luma := func(panel int) float64 {
		var sum float64
		for y := range 270 {
			for x := panel * 160; x < (panel+1)*160; x++ {
				r, g, b, _ := img.At(x, y).RGBA()
				sum += float64(r+g+b) / 3
			}
		}
		return sum / (160 * 270)
	}
	if l0, l1, l2 := luma(0), luma(1), luma(2); !(l0 < l1 && l1 < l2) {
		t.Fatalf("panels are not from the dark, plain and bright parts in turn: %.0f %.0f %.0f", l0, l1, l2)
	}
	if !SessionThumbnailCurrent(dir, files) || SessionThumbnailCurrent(dir, files[:2]) {
		t.Fatal("thumbnail key does not follow its files")
	}
}

func TestSessionThumbnailNeedsPortrait(t *testing.T) {
	dir := t.TempDir()
	video := previewFixtureAt(t, dir, "vod1.mp4", 640, 360, 5*time.Second, 1)
	st, err := os.Stat(video)
	if err != nil {
		t.Fatal(err)
	}
	files := []SheetFile{{Name: "vod1.mp4", Bytes: st.Size()}}
	if _, err := WriteSessionThumbnail(context.Background(), slog.Default(), dir, dir, files); !errors.Is(err, ErrNotPortrait) {
		t.Fatalf("err = %v", err)
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
	m, err := GeneratePreviews(context.Background(), slog.Default(), video, ThumbnailName("vod.mp4"), PreviewHeader{Platform: "tiktok", Username: "sample"},
		func(name string, data []byte) error { return WriteFileAtomic(filepath.Join(out, name), data) })
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
