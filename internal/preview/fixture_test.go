package preview

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"
)

// testFFmpeg finds an ffmpeg with libx264 for building fixtures: $PREVIEW_FFMPEG,
// the recorder's embedded build, then PATH. Tests skip without one.
func testFFmpeg(t *testing.T) string {
	t.Helper()
	candidates := []string{
		os.Getenv("PREVIEW_FFMPEG"),
		filepath.Join("..", "ffmpeg", "bin", runtime.GOOS+"_"+runtime.GOARCH, "ffmpeg"),
	}
	if p, err := exec.LookPath("ffmpeg"); err == nil {
		candidates = append(candidates, p)
	}
	for _, c := range candidates {
		if c == "" {
			continue
		}
		if info, err := os.Stat(c); err == nil && info.Size() > 1<<20 {
			abs, _ := filepath.Abs(c)
			return abs
		}
	}
	t.Skip("no ffmpeg for fixtures")
	return ""
}

// fixture encodes a testsrc clip of w x h at 30 fps with a keyframe every
// gop seconds.
func fixture(t *testing.T, ff string, w, h int, dur time.Duration, gop int, ext string) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "clip"+ext)
	args := []string{
		"-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc=size=" + strconv.Itoa(w) + "x" + strconv.Itoa(h) + ":rate=30:duration=" + strconv.FormatFloat(dur.Seconds(), 'f', -1, 64),
		"-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p",
		"-g", strconv.Itoa(gop * 30), "-keyint_min", strconv.Itoa(gop * 30), "-sc_threshold", "0", "-bf", "2",
		out,
	}
	if b, err := exec.Command(ff, args...).CombinedOutput(); err != nil {
		t.Fatalf("make fixture: %v: %s", err, b)
	}
	return out
}

func testDecoder(ff string) *Decoder {
	return NewDecoder(func() (string, error) { return ff, nil }, 1)
}

func TestDecodeKeyframesFromMP4(t *testing.T) {
	ff := testFFmpeg(t)
	path := fixture(t, ff, 640, 360, 20*time.Second, 2, ".mp4")
	m, err := OpenMP4(path)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if info := m.Info(); info.Width != 640 || info.Height != 360 || info.Codec != "H.264" {
		t.Fatalf("info = %+v", info)
	}
	keys := m.KeyTimes()
	if len(keys) != 10 {
		t.Fatalf("got %d keyframes, want 10: %v", len(keys), keys)
	}
	for i, k := range keys {
		if want := time.Duration(i) * 2 * time.Second; k < want-50*time.Millisecond || k > want+50*time.Millisecond {
			t.Fatalf("keyframe %d at %v, want %v", i, k, want)
		}
	}
	var frames []Keyframe
	for _, i := range []int{0, 3, 7} {
		kf, err := m.Keyframe(i)
		if err != nil {
			t.Fatal(err)
		}
		frames = append(frames, kf)
	}
	jpegs, err := testDecoder(ff).DecodeKeyframes(context.Background(), m.Config(), frames, Box{256, 256}, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(jpegs) != 3 {
		t.Fatalf("got %d jpegs", len(jpegs))
	}
	w, h := jpegSize(t, jpegs[0])
	if w != 256 || h != 144 {
		t.Fatalf("frame is %dx%d, want 256x144", w, h)
	}
}

func TestDecodeFileCFR(t *testing.T) {
	ff := testFFmpeg(t)
	path := fixture(t, ff, 360, 640, 10*time.Second, 5, ".mp4")
	jpegs, err := testDecoder(ff).DecodeFile(context.Background(), path, FileOptions{Interval: time.Second, Box: Box{144, 256}, Duration: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if len(jpegs) != 10 {
		t.Fatalf("got %d frames for 10 s at 1 s", len(jpegs))
	}
	if w, h := jpegSize(t, jpegs[0]); w != 144 || h != 256 {
		t.Fatalf("frame is %dx%d", w, h)
	}
	keyOnly, err := testDecoder(ff).DecodeFile(context.Background(), path, FileOptions{Interval: 2 * time.Second, KeyframesOnly: true, Box: Box{144, 256}, Duration: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if len(keyOnly) != 5 {
		t.Fatalf("got %d keyframe-only frames for 10 s at 2 s", len(keyOnly))
	}
}
