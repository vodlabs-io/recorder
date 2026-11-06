package hlsingest

import (
	"crypto/sha256"
	"encoding/hex"
	"sync"
)

// tailSize is how many recent segments a Tail remembers: enough to cover the
// live-edge window a new ingest starts from, at the shortest segments in use.
const tailSize = 128

// Tail remembers the last segments a Session recorded, by content, so that an
// ingest starting at the live edge after a reconnect or a restart does not
// record again what the part before it already holds. The Session owns it and
// hands the same Tail to every part.
//
// A segment is named by a hash of the bytes handed to the Sink, which an
// archiving Sink writes to disk verbatim, so the Tail of a part cut off by a
// crash can be rebuilt from its files.
type Tail struct {
	mu     sync.Mutex
	hashes []string
	set    map[string]bool
}

// NewTail returns a Tail holding hashes, oldest first.
func NewTail(hashes []string) *Tail {
	t := &Tail{set: make(map[string]bool)}
	for _, h := range hashes {
		t.Add(h)
	}
	return t
}

// SegmentHash names a segment by its content.
func SegmentHash(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:16])
}

// Has reports whether a segment with hash h was recorded recently.
func (t *Tail) Has(h string) bool {
	if t == nil || h == "" {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.set[h]
}

// Add records a segment, forgetting the oldest beyond tailSize.
func (t *Tail) Add(h string) {
	if t == nil || h == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.set[h] {
		return
	}
	t.hashes = append(t.hashes, h)
	t.set[h] = true
	if over := len(t.hashes) - tailSize; over > 0 {
		for _, old := range t.hashes[:over] {
			delete(t.set, old)
		}
		t.hashes = append([]string(nil), t.hashes[over:]...)
	}
}

// Hashes returns the remembered hashes, oldest first.
func (t *Tail) Hashes() []string {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]string(nil), t.hashes...)
}
