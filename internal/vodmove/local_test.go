package vodmove

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestStoreRenameFastPath(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src", "vod.mp4")
	writeFile(t, src, []byte("hello world"))

	d := &localDir{Root: filepath.Join(dir, "dest")}
	if err := d.Store(context.Background(), src, "tiktok/alice/vod.mp4"); err != nil {
		t.Fatalf("Store: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(dir, "dest", "tiktok", "alice", "vod.mp4"))
	if err != nil {
		t.Fatalf("read dest: %v", err)
	}
	if string(got) != "hello world" {
		t.Fatalf("dest content = %q", got)
	}
	// Same-volume rename consumes the source.
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Fatalf("expected source gone after rename, stat err = %v", err)
	}
}

func TestCopyVerifiedWritesAndVerifies(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.mp4")
	writeFile(t, src, []byte("payload-bytes"))
	dst := filepath.Join(dir, "dest", "out.mp4")
	// Store() creates the destination dir in production; copyVerified assumes it
	// exists, so create it here when testing the copy path directly.
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}

	d := &localDir{Root: dir}
	if err := d.copyVerified(context.Background(), src, dst, int64(len("payload-bytes"))); err != nil {
		t.Fatalf("copyVerified: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read dst: %v", err)
	}
	if string(got) != "payload-bytes" {
		t.Fatalf("dst content = %q", got)
	}
	// The source must survive the copy path (caller removes it afterwards).
	if _, err := os.Stat(src); err != nil {
		t.Fatalf("source should remain after copy: %v", err)
	}
	// No leftover .part file.
	if _, err := os.Stat(dst + ".part"); !os.IsNotExist(err) {
		t.Fatalf("expected no leftover .part, stat err = %v", err)
	}
}

func TestCopyVerifiedSizeMismatchKeepsSource(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.mp4")
	writeFile(t, src, []byte("12345"))
	dst := filepath.Join(dir, "out.mp4")

	d := &localDir{Root: dir}
	// Claim a larger size than the source actually has; the post-copy size
	// check must fail and leave nothing behind but the (untouched) source.
	err := d.copyVerified(context.Background(), src, dst, 999)
	if err == nil {
		t.Fatal("expected size-mismatch error, got nil")
	}
	if _, err := os.Stat(src); err != nil {
		t.Fatalf("source must remain on failure: %v", err)
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Fatalf("dest must not exist on failure, stat err = %v", err)
	}
	if _, err := os.Stat(dst + ".part"); !os.IsNotExist(err) {
		t.Fatalf(".part must be cleaned on failure, stat err = %v", err)
	}
}

func TestCopyVerifiedCleansStalePart(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.mp4")
	writeFile(t, src, []byte("good"))
	dst := filepath.Join(dir, "out.mp4")
	// Simulate a daemon killed mid-copy: a stale .part file from before.
	writeFile(t, dst+".part", []byte("stale-garbage-bytes"))

	d := &localDir{Root: dir}
	if err := d.copyVerified(context.Background(), src, dst, int64(len("good"))); err != nil {
		t.Fatalf("copyVerified: %v", err)
	}
	got, _ := os.ReadFile(dst)
	if string(got) != "good" {
		t.Fatalf("dst content = %q (stale .part not replaced)", got)
	}
}

func TestStoreIdempotentSameSize(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src", "vod.mp4")
	writeFile(t, src, []byte("abcdef"))
	// Destination already has an identical-size copy.
	writeFile(t, filepath.Join(dir, "dest", "vod.mp4"), []byte("ABCDEF"))

	d := &localDir{Root: filepath.Join(dir, "dest")}
	if err := d.Store(context.Background(), src, "vod.mp4"); err != nil {
		t.Fatalf("Store: %v", err)
	}
	// Idempotent short-circuit: destination is left as-is and the source is not
	// consumed (no rename, no copy).
	got, _ := os.ReadFile(filepath.Join(dir, "dest", "vod.mp4"))
	if string(got) != "ABCDEF" {
		t.Fatalf("dest content = %q, expected untouched", got)
	}
	if _, err := os.Stat(src); err != nil {
		t.Fatalf("source should be untouched on idempotent skip: %v", err)
	}
}

func TestStoreOverwritesDifferentSize(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src", "vod.mp4")
	writeFile(t, src, []byte("new-longer-content"))
	// Destination has a different-size stale partial.
	writeFile(t, filepath.Join(dir, "dest", "vod.mp4"), []byte("old"))

	d := &localDir{Root: filepath.Join(dir, "dest")}
	if err := d.Store(context.Background(), src, "vod.mp4"); err != nil {
		t.Fatalf("Store: %v", err)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "dest", "vod.mp4"))
	if string(got) != "new-longer-content" {
		t.Fatalf("dest content = %q, expected overwrite", got)
	}
}

func TestStoreSourceGoneDestPresent(t *testing.T) {
	dir := t.TempDir()
	// No source; destination already has the file (move completed before crash).
	writeFile(t, filepath.Join(dir, "dest", "vod.mp4"), []byte("done"))

	d := &localDir{Root: filepath.Join(dir, "dest")}
	if err := d.Store(context.Background(), filepath.Join(dir, "src", "vod.mp4"), "vod.mp4"); err != nil {
		t.Fatalf("Store should treat source-gone+dest-present as done: %v", err)
	}
}

func TestStoreSourceGoneDestMissing(t *testing.T) {
	dir := t.TempDir()
	d := &localDir{Root: filepath.Join(dir, "dest")}
	err := d.Store(context.Background(), filepath.Join(dir, "src", "vod.mp4"), "vod.mp4")
	if err == nil {
		t.Fatal("expected error when both source and dest are missing")
	}
}

func TestForDestRejectsScheme(t *testing.T) {
	if _, err := ForDest("s3://bucket/prefix"); err == nil {
		t.Fatal("expected ForDest to reject a scheme URI")
	}
	if _, err := ForDest(""); err == nil {
		t.Fatal("expected ForDest to reject an empty destination")
	}
	if _, err := ForDest("/mnt/nas"); err != nil {
		t.Fatalf("ForDest(local path): %v", err)
	}
}
