package thumbcache

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func newTestCache(t *testing.T, maxBytes int64) *Cache {
	t.Helper()
	c, err := New(t.TempDir(), maxBytes)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func TestPutGetRoundTrip(t *testing.T) {
	c := newTestCache(t, 0)
	want := []byte("jpeg-bytes")
	if err := c.Put("session-1", "vod1.mp4", want); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, ok := c.Get("session-1", "vod1.mp4")
	if !ok {
		t.Fatal("Get: miss after Put")
	}
	if string(got) != string(want) {
		t.Fatalf("Get returned %q, want %q", got, want)
	}
}

func TestGetMissesAreNotErrors(t *testing.T) {
	c := newTestCache(t, 0)
	if _, ok := c.Get("session-1", "vod1.mp4"); ok {
		t.Error("Get: hit on an empty cache")
	}
	// A different file of a cached session is still a miss.
	if err := c.Put("session-1", "vod1.mp4", []byte("x")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if _, ok := c.Get("session-1", "vod2.mp4"); ok {
		t.Error("Get: vod2 hit an entry stored for vod1")
	}
	if _, ok := c.Get("session-2", "vod1.mp4"); ok {
		t.Error("Get: a different session hit another session's entry")
	}
}

// The cache is addressed by session key, not folder, so an entry survives the
// move from the download folder to the archive — the whole point of seeding it
// during processing.
func TestEntryFoundAfterSessionMoves(t *testing.T) {
	c := newTestCache(t, 0)
	if err := c.Put("abc123", "vod1.mp4", []byte("frame")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	// Same session, looked up after its folder changed: the key did not.
	if _, ok := c.Get("abc123", "vod1.mp4"); !ok {
		t.Error("Get: miss for a session whose folder moved")
	}
}

func TestNilCacheIsUsable(t *testing.T) {
	var c *Cache
	if _, ok := c.Get("session-1", "vod1.mp4"); ok {
		t.Error("Get: nil cache reported a hit")
	}
	if err := c.Put("session-1", "vod1.mp4", []byte("x")); err != nil {
		t.Errorf("Put on nil cache: %v", err)
	}
	if err := c.PutFile("session-1", "vod1.mp4", "/nonexistent"); err != nil {
		t.Errorf("PutFile on nil cache: %v", err)
	}
}

func TestEmptyKeyAndPayloadAreIgnored(t *testing.T) {
	c := newTestCache(t, 0)
	if err := c.Put("", "vod1.mp4", []byte("x")); err != nil {
		t.Errorf("Put with empty key: %v", err)
	}
	if err := c.Put("session-1", "vod1.mp4", nil); err != nil {
		t.Errorf("Put with empty payload: %v", err)
	}
	if _, ok := c.Get("session-1", "vod1.mp4"); ok {
		t.Error("Get: an empty payload was stored")
	}
}

func TestPutFileCopiesFromDisk(t *testing.T) {
	c := newTestCache(t, 0)
	src := filepath.Join(t.TempDir(), "vod1.jpg")
	if err := os.WriteFile(src, []byte("from-disk"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := c.PutFile("session-1", "vod1.mp4", src); err != nil {
		t.Fatalf("PutFile: %v", err)
	}
	got, ok := c.Get("session-1", "vod1.mp4")
	if !ok || string(got) != "from-disk" {
		t.Fatalf("Get after PutFile = %q, %v", got, ok)
	}
}

func TestPutFileReportsAMissingSource(t *testing.T) {
	c := newTestCache(t, 0)
	if err := c.PutFile("session-1", "vod1.mp4", filepath.Join(t.TempDir(), "gone.jpg")); err == nil {
		t.Error("PutFile: no error for a missing source file")
	}
}

func TestEvictionDropsOldestFirst(t *testing.T) {
	// Small cap so a handful of entries crosses it. Eviction runs on the first
	// write of the process and every sweepAfterBytes thereafter.
	c := newTestCache(t, 100)
	payload := make([]byte, 40)

	if err := c.Put("old", "vod1.mp4", payload); err != nil {
		t.Fatalf("Put old: %v", err)
	}
	// Age it well past the newer entries so oldest-first is unambiguous.
	oldPath := c.path("old", "vod1.mp4")
	past := time.Now().Add(-24 * time.Hour)
	if err := os.Chtimes(oldPath, past, past); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}
	for _, key := range []string{"new1", "new2"} {
		if err := c.Put(key, "vod1.mp4", payload); err != nil {
			t.Fatalf("Put %s: %v", key, err)
		}
	}
	c.evict()

	if _, ok := c.Get("old", "vod1.mp4"); ok {
		t.Error("the oldest entry survived eviction")
	}
	if _, ok := c.Get("new2", "vod1.mp4"); !ok {
		t.Error("the newest entry was evicted")
	}
}

func TestEvictionKeepsEverythingUnderTheCap(t *testing.T) {
	c := newTestCache(t, 1<<20)
	for _, key := range []string{"a", "b", "c"} {
		if err := c.Put(key, "vod1.mp4", make([]byte, 1024)); err != nil {
			t.Fatalf("Put %s: %v", key, err)
		}
	}
	c.evict()
	for _, key := range []string{"a", "b", "c"} {
		if _, ok := c.Get(key, "vod1.mp4"); !ok {
			t.Errorf("entry %q was evicted while under the cap", key)
		}
	}
}

// A Put must never leave a partial file behind for a reader to pick up, and it
// must not leave its temp file in the directory either.
func TestPutLeavesNoTempFiles(t *testing.T) {
	c := newTestCache(t, 0)
	if err := c.Put("session-1", "vod1.mp4", []byte("x")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	entries, err := os.ReadDir(c.dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) != ".jpg" {
			t.Errorf("leftover file in cache dir: %s", e.Name())
		}
	}
}

func TestPutOverwritesAnExistingEntry(t *testing.T) {
	c := newTestCache(t, 0)
	if err := c.Put("session-1", "vod1.mp4", []byte("first")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := c.Put("session-1", "vod1.mp4", []byte("second")); err != nil {
		t.Fatalf("Put again: %v", err)
	}
	got, ok := c.Get("session-1", "vod1.mp4")
	if !ok || string(got) != "second" {
		t.Fatalf("Get = %q, %v; want the second write", got, ok)
	}
}

func TestDeleteRemovesAnEntry(t *testing.T) {
	c := newTestCache(t, 0)
	if err := c.Put("session-1", "vod1.mp4", []byte("x")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := c.Delete("session-1", "vod1.mp4"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, ok := c.Get("session-1", "vod1.mp4"); ok {
		t.Error("Get: hit after Delete")
	}
}

func TestDeleteIsIdempotent(t *testing.T) {
	c := newTestCache(t, 0)
	if err := c.Delete("session-1", "never-stored.mp4"); err != nil {
		t.Errorf("Delete of a missing entry: %v", err)
	}
	var nilCache *Cache
	if err := nilCache.Delete("session-1", "vod1.mp4"); err != nil {
		t.Errorf("Delete on nil cache: %v", err)
	}
}
