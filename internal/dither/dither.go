// Package dither generates fixed pseudorandom sequences of low-amplitude
// brightness offsets, applied to video before it is compressed. The offsets
// average out to zero, so overall brightness does not shift.
package dither

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
)

// seed fixes the pattern. Changing it changes every sequence.
var seed = sha256.Sum256([]byte("vodlabs.recorder.dither.v1"))

// Pattern is one named offset sequence. Separate names give independent
// sequences.
type Pattern struct {
	label []byte
	block int64
	word  uint64
	valid bool
}

// New returns the pattern for a label. The zero index is the sequence start.
func New(label string) *Pattern { return &Pattern{label: []byte(label)} }

// Bit returns the offset sign at index i: 0 for negative, 1 for positive.
func (p *Pattern) Bit(i int64) int64 {
	if i < 0 {
		return 0
	}
	if b := i / 64; !p.valid || p.block != b {
		var msg [8]byte
		binary.BigEndian.PutUint64(msg[:], uint64(b))
		mac := hmac.New(sha256.New, seed[:])
		mac.Write(p.label)
		mac.Write(msg[:])
		p.block, p.word, p.valid = b, binary.BigEndian.Uint64(mac.Sum(nil)[:8]), true
	}
	return int64(p.word>>uint(i%64)) & 1
}

// Sign returns the offset direction at index i, as -1 or +1.
func (p *Pattern) Sign(i int64) int {
	if p.Bit(i) == 1 {
		return 1
	}
	return -1
}
