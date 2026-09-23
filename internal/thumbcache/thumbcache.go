// Package thumbcache keeps VOD screenshots on local disk so the VOD Archive
// never reads them back from an Archive folder, where on a busy NAS even a stat
// can exceed a request budget. A screenshot is immutable once its video file is
// finished, so the first local copy is good forever.
package thumbcache

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// DefaultMaxBytes caps the cache. Screenshots run 10-50 KB, so this holds tens
// of thousands of them — more than an archive is likely to list — while staying
// small enough to be unremarkable in a user's cache folder.
const DefaultMaxBytes = 512 << 20 // 512 MiB

// sweepAfterBytes is how much must be written before eviction runs again.
// Sweeping reads the whole cache directory, so amortise it rather than paying
// on every store.
const sweepAfterBytes = 8 << 20 // 8 MiB

// touchAfter is how stale an entry's timestamp must be before a read refreshes
// it. Eviction is oldest-first by mtime, so reads have to bump it or a
// frequently-viewed old VOD would be evicted ahead of a never-viewed new one —
// but bumping on literally every read would turn a page of cards into a page of
// writes for no benefit.
const touchAfter = time.Hour

// Cache is a flat directory of JPEGs keyed by session and file name. The zero
// value is not usable; call New. A nil *Cache is safe and always misses, so
// callers need no branch when caching is disabled.
type Cache struct {
	dir      string
	maxBytes int64

	mu           sync.Mutex
	sinceSweep   int64
	sweptAtLeast bool
}

// New returns a cache rooted at dir, creating it if needed. maxBytes <= 0 uses
// DefaultMaxBytes.
func New(dir string, maxBytes int64) (*Cache, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create thumbnail cache dir: %w", err)
	}
	if maxBytes <= 0 {
		maxBytes = DefaultMaxBytes
	}
	return &Cache{dir: dir, maxBytes: maxBytes}, nil
}

// path maps a session key and file name to their cache entry. The key is
// hashed rather than sanitised: session keys are absolute paths for sessions
// without a manifest, and hashing sidesteps separators, case folding and name
// length.
func (c *Cache) path(sessionKey, file string) string {
	sum := sha256.Sum256([]byte(sessionKey + "\x00" + file))
	return filepath.Join(c.dir, hex.EncodeToString(sum[:])+".jpg")
}

// Get returns the cached JPEG for one file of a session. A miss (including a
// nil cache) returns ok=false and no error: every caller's fallback is to read
// or regenerate the real thing.
func (c *Cache) Get(sessionKey, file string) ([]byte, bool) {
	if c == nil || sessionKey == "" {
		return nil, false
	}
	p := c.path(sessionKey, file)
	data, err := os.ReadFile(p)
	if err != nil || len(data) == 0 {
		return nil, false
	}
	c.touch(p)
	return data, true
}

// touch refreshes an entry's mtime so eviction sees it as recently used,
// skipping entries already touched within touchAfter. Best-effort: a cache
// whose timestamps stop updating evicts in a worse order, which is not worth
// failing a read over.
func (c *Cache) touch(path string) {
	info, err := os.Stat(path)
	if err != nil || time.Since(info.ModTime()) < touchAfter {
		return
	}
	now := time.Now()
	_ = os.Chtimes(path, now, now)
}

// Put stores one file's JPEG. Storing is best-effort from the caller's point of
// view — a failure here costs a slow re-read later, never correctness — but the
// error is returned so callers can log it.
func (c *Cache) Put(sessionKey, file string, jpeg []byte) error {
	if c == nil || sessionKey == "" || len(jpeg) == 0 {
		return nil
	}
	p := c.path(sessionKey, file)
	// Write-and-rename: a reader must never see a half-written JPEG, and two
	// daemons sharing a cache dir must not interleave into one file.
	tmp, err := os.CreateTemp(c.dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("create thumbnail cache temp: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(jpeg); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("write thumbnail cache entry: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("close thumbnail cache entry: %w", err)
	}
	if err := os.Rename(tmpName, p); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("commit thumbnail cache entry: %w", err)
	}
	c.noteWrite(int64(len(jpeg)))
	return nil
}

// PutFile stores the JPEG at srcPath. Used where the screenshot already exists
// on local disk (processing writes one beside the video before the move), so
// the cache is warm before the archive volume is ever involved.
func (c *Cache) PutFile(sessionKey, file, srcPath string) error {
	if c == nil || sessionKey == "" {
		return nil
	}
	data, err := os.ReadFile(srcPath)
	if err != nil {
		return fmt.Errorf("read thumbnail to cache: %w", err)
	}
	return c.Put(sessionKey, file, data)
}

// Delete drops one entry. Used where a file stops being part of a session — a
// merge replaces its parts with a single output — so the cache does not hold
// screenshots of files the archive will never list again. A missing entry is
// not an error.
func (c *Cache) Delete(sessionKey, file string) error {
	if c == nil || sessionKey == "" {
		return nil
	}
	if err := os.Remove(c.path(sessionKey, file)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove thumbnail cache entry: %w", err)
	}
	return nil
}

// noteWrite accumulates bytes written and runs eviction once enough have piled
// up. The first write of a process always sweeps, so a cache left over the cap
// by an earlier run (a lowered limit, a crash mid-sweep) is corrected promptly
// instead of at some arbitrary later point.
func (c *Cache) noteWrite(n int64) {
	c.mu.Lock()
	c.sinceSweep += n
	due := !c.sweptAtLeast || c.sinceSweep >= sweepAfterBytes
	if due {
		c.sinceSweep = 0
		c.sweptAtLeast = true
	}
	c.mu.Unlock()
	if due {
		c.evict()
	}
}

// evict deletes oldest-first until the directory fits maxBytes. Best-effort
// throughout: an unreadable directory or an entry another process removed
// first just means the cache stays larger than intended for a while.
func (c *Cache) evict() {
	entries, err := os.ReadDir(c.dir)
	if err != nil {
		return
	}
	type item struct {
		path  string
		size  int64
		mtime time.Time
	}
	items := make([]item, 0, len(entries))
	var total int64
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		items = append(items, item{filepath.Join(c.dir, e.Name()), info.Size(), info.ModTime()})
		total += info.Size()
	}
	if total <= c.maxBytes {
		return
	}
	sort.Slice(items, func(i, j int) bool { return items[i].mtime.Before(items[j].mtime) })
	for _, it := range items {
		if total <= c.maxBytes {
			return
		}
		if err := os.Remove(it.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			continue
		}
		total -= it.size
	}
}
