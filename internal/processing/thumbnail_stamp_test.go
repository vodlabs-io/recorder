package processing

import (
	"bytes"
	"image"
	"image/jpeg"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/vodlabs-io/recorder/internal/provenance"
)

func TestStampThumbnailKeepsAValidImage(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "vod1.jpg")

	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, image.NewGray(image.Rect(0, 0, 8, 8)), nil); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	stampThumbnail(slog.New(slog.DiscardHandler), path)

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := jpeg.Decode(bytes.NewReader(got)); err != nil {
		t.Fatalf("stamped thumbnail no longer decodes: %v", err)
	}
	rec := provenance.Record("thumbnail", "", "vod1.jpg")
	if !bytes.Contains(got, []byte(rec)) {
		t.Errorf("record %q not present in stamped image", rec)
	}
}

// A thumbnail that cannot be stamped is still a good thumbnail: the image must
// survive untouched rather than be lost.
func TestStampThumbnailLeavesUnstampableFilesAlone(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "not.jpg")
	want := []byte("this is not a jpeg")
	if err := os.WriteFile(path, want, 0o644); err != nil {
		t.Fatal(err)
	}

	stampThumbnail(slog.New(slog.DiscardHandler), path)

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("file was modified: %q", got)
	}
}
