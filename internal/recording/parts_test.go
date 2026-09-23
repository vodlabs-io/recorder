package recording

import (
	"os"
	"path/filepath"
	"testing"
)

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func mustMkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
}

func assertExists(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		t.Errorf("expected %s to exist: %v", path, err)
	}
}

func assertGone(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("expected %s to be removed, stat err = %v", path, err)
	}
}

func TestRemovePartFiles_FLV(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "pt1_1080p.ts"), "a")
	mustWrite(t, filepath.Join(dir, "pt2_1080p.ts"), "b")
	mustWrite(t, filepath.Join(dir, "vod.mp4"), "out")
	mustWrite(t, filepath.Join(dir, "session.log"), "log")

	v := Output{BaseOutDir: dir, WriterName: "flvwriter"}
	n, err := v.RemovePartFiles()
	if err != nil {
		t.Fatalf("RemovePartFiles: %v", err)
	}
	if n != 2 {
		t.Errorf("removed = %d, want 2", n)
	}
	assertGone(t, filepath.Join(dir, "pt1_1080p.ts"))
	assertGone(t, filepath.Join(dir, "pt2_1080p.ts"))
	// The assembled output and logs must survive.
	assertExists(t, filepath.Join(dir, "vod.mp4"))
	assertExists(t, filepath.Join(dir, "session.log"))
}

func TestRemovePartFiles_HLS(t *testing.T) {
	dir := t.TempDir()
	mustMkdir(t, filepath.Join(dir, "pt1_1080p"))
	mustWrite(t, filepath.Join(dir, "pt1_1080p", "index.m3u8"), "m")
	mustMkdir(t, filepath.Join(dir, "pt2_1080p"))
	mustWrite(t, filepath.Join(dir, "vod.mp4"), "out")

	v := Output{BaseOutDir: dir, WriterName: "hlswriter"}
	n, err := v.RemovePartFiles()
	if err != nil {
		t.Fatalf("RemovePartFiles: %v", err)
	}
	if n != 2 {
		t.Errorf("removed = %d, want 2", n)
	}
	assertGone(t, filepath.Join(dir, "pt1_1080p"))
	assertGone(t, filepath.Join(dir, "pt2_1080p"))
	assertExists(t, filepath.Join(dir, "vod.mp4"))
}

func TestRemovePartFiles_UnsupportedWriter(t *testing.T) {
	v := Output{BaseOutDir: t.TempDir(), WriterName: "bogus"}
	if _, err := v.RemovePartFiles(); err == nil {
		t.Errorf("RemovePartFiles with bad writer: err = nil, want error")
	}
}
