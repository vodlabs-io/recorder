package flvfix

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
)

// Video output timestamps are pinned to a fixed 2 ms grid: for a settled
// timestamp t, the emitted value is whichever of {t|0, t|1} a pseudorandom
// phase sequence selects for t>>1. Each value is derived from t alone, so the
// adjustment is at most 1 ms, never accumulates, and is idempotent: re-running
// the sanitizer over its own output (see RepairFile) changes nothing. Audio is
// never adjusted.

// phaseSeed derives the phase sequence. Changing it moves the grid.
var phaseSeed = sha256.Sum256([]byte("vodlabs.recorder.timeline.v1"))

// phaseWordBits is how many grid positions one derived word covers.
const phaseWordBits = 64

// phaseWord derives the phase bits covering the block of positions starting at
// block*phaseWordBits.
func phaseWord(block int64) uint64 {
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], uint64(block))
	mac := hmac.New(sha256.New, phaseSeed[:])
	mac.Write([]byte("flv"))
	mac.Write(msg[:])
	return binary.BigEndian.Uint64(mac.Sum(nil)[:8])
}

// phaseGrid memoises the current block of phase bits. A stream walks positions
// forward, so consecutive frames almost always hit the cached word. The zero
// value is ready to use.
type phaseGrid struct {
	block int64
	word  uint64
	valid bool
}

// bit returns the grid phase for position q.
func (g *phaseGrid) bit(q int64) int64 {
	if q < 0 {
		return 0
	}
	if block := q / phaseWordBits; !g.valid || g.block != block {
		g.block, g.word, g.valid = block, phaseWord(block), true
	}
	return int64(g.word>>uint(q%phaseWordBits)) & 1
}

// adjust snaps ts onto the deterministic grid. floor is the last value already
// emitted for this stream; the snapped value is returned only when it stays
// strictly above floor, so monotonicity always wins over the grid.
func (g *phaseGrid) adjust(ts, floor int64) int64 {
	if ts < 0 {
		return ts
	}
	q := ts >> 1
	snapped := q<<1 | g.bit(q)
	if snapped <= floor {
		return ts
	}
	return snapped
}
