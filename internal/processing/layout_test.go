package processing

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestMigrateLayout(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"vod1.mp4":            "v",
		"vod1.jpg":            "thumb1",
		"vod1.thumbs.vtt":     "WEBVTT\n\n00:00:00.000 --> 00:00:01.000\nvod1.thumbs.000.jpg#xywh=0,0,128,256\n",
		"vod1.thumbs.000.jpg": "sheet",
		"vod1.scrub.jpg":      "scrub",
		"vod1.previews.json":  `{"version":1}`,
		"vod10.mp4":           "v",
		"vod10.jpg":           "thumb10",
		"contact-sheet.jpg":   "sheet",
		"contact-sheet.json":  "[]",
		"session.log":         "log",
		"vod7.jpg":            "orphan",
	})
	n, err := MigrateLayout(dir, []string{"vod1.mp4", "vod10.mp4"})
	if err != nil {
		t.Fatal(err)
	}
	if n != 7 {
		t.Fatalf("moved %d sidecars", n)
	}
	for name, body := range map[string]string{
		ThumbnailName("vod1.mp4"):          "thumb1",
		StoryboardSheetName("vod1.mp4", 0): "sheet",
		ScrubStripName("vod1.mp4"):         "scrub",
		PreviewsManifestName("vod1.mp4"):   `{"version":1}`,
		ThumbnailName("vod10.mp4"):         "thumb10",
		sessionSheetKeyName:                "[]",
		"contact-sheet.jpg":                "sheet",
		"session.log":                      "log",
		"vod7.jpg":                         "orphan",
		"vod1.mp4":                         "v",
	} {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || string(b) != body {
			t.Fatalf("%s = %q, %v", name, b, err)
		}
	}
	vtt, err := os.ReadFile(filepath.Join(dir, StoryboardName("vod1.mp4")))
	if err != nil || !strings.Contains(string(vtt), "\nthumbs.000.jpg#xywh=0,0,128,256\n") {
		t.Fatalf("track = %q, %v", vtt, err)
	}
	for _, old := range []string{"vod1.jpg", "vod1.thumbs.vtt", "vod1.thumbs.000.jpg", "vod1.scrub.jpg", "vod1.previews.json", "vod10.jpg", "contact-sheet.json"} {
		if _, err := os.Stat(filepath.Join(dir, old)); !os.IsNotExist(err) {
			t.Fatalf("%s left behind: %v", old, err)
		}
	}
	if n, err := MigrateLayout(dir, []string{"vod1.mp4", "vod10.mp4"}); n != 0 || err != nil {
		t.Fatalf("second pass moved %d, %v", n, err)
	}
}

func TestMigrateLayoutKeepsNewerCopy(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"vod1.mp4": "v", "vod1.jpg": "old"})
	if err := WriteFileAtomic(filepath.Join(dir, ThumbnailName("vod1.mp4")), []byte("new")); err != nil {
		t.Fatal(err)
	}
	if _, err := MigrateLayout(dir, []string{"vod1.mp4"}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, ThumbnailName("vod1.mp4"))); string(b) != "new" {
		t.Fatalf("thumbnail = %q", b)
	}
	if _, err := os.Stat(filepath.Join(dir, "vod1.jpg")); !os.IsNotExist(err) {
		t.Fatal("flat copy left behind")
	}
}

func TestSidecarsReadFromFlatLayout(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"vod2.previews.json":  `{"version":1,"duration_ms":5000}`,
		"vod2.thumbs.003.jpg": "sheet",
		"vod2.jpg":            "thumb",
	})
	if !HasPreviews(dir, "vod2.mp4") {
		t.Fatal("flat manifest not found")
	}
	f, err := OpenSidecar(dir, StoryboardSheetName("vod2.mp4", 3))
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	if got := SidecarPath(dir, ThumbnailName("vod2.mp4")); got != filepath.Join(dir, "vod2.jpg") {
		t.Fatalf("thumbnail at %s", got)
	}
	if got := SidecarPath(dir, ThumbnailName("vod3.mp4")); got != filepath.Join(dir, ThumbnailName("vod3.mp4")) {
		t.Fatalf("missing thumbnail resolves to %s", got)
	}
	if _, err := OpenSidecar(dir, "../"+StoryboardSheetName("vod2.mp4", 3)); err == nil {
		t.Fatal("opened a sidecar outside the folder")
	}
}
