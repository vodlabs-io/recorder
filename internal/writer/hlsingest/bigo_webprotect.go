package hlsingest

// BIGO-SPECIFIC. Nothing here serves any other platform, and BIGO is expected to
// keep changing the scheme, so this is written to be thrown away.
//
// TO REMOVE, when BIGO support is dropped or the scheme stops being worth
// chasing, delete in full:
//
//   - this file and bigo_webprotect_test.go
//   - the `bigo bigoState` field on `ingest` (ingest.go)
//   - the `o.bigo.observe(body)` line in pollPlaylist (origin.go)
//   - the `if o.bigo.active` block in ingest.run (ingest.go)
//
// Each is marked BIGO-SPECIFIC at its site, and nothing outside those four
// places refers to any of it. Everything this file declares is bigo-prefixed so
// a grep for "bigo" in this package finds all of it.
//
// WHAT IT DOES. BIGO masks the head of every media segment and publishes the
// mask's seed in an EXT-X-BIGO-WEB-PROTECTION playlist tag, so that only a
// player reading the tag can demux what it serves. The first two MPEG-TS packets
// of a segment carry the PAT and the PMT; masked, their sync bytes are garbage
// and neither table is ever found. A demuxer handed those bytes falls back to
// probing raw payload, recognises the audio — an ADTS syncword is trivial to
// spot — and gives up on the video, which needs the PMT's stream type. The
// result is an audio-only recording that no stage reports as a failure.

import (
	"regexp"
	"strconv"
)

// Standard MPEG-TS values, declared here rather than shared so that this file
// deletes cleanly.
const (
	bigoTSPacketSize = 188
	bigoTSSyncByte   = 0x47
)

const (
	// bigoMaskedPackets is how many leading packets carry the mask, and
	// bigoMaskedBytes how much of each.
	bigoMaskedPackets = 2
	bigoMaskedBytes   = 16
	// bigoKnuth mixes the packet index into the seed.
	bigoKnuth = 2654435769
	// bigoSeedFallback replaces a seed that mixes to zero, bigoMaskFallback a
	// keystream byte that would be a no-op.
	bigoSeedFallback = 1831565813
	bigoMaskFallback = 165
)

var bigoSeedPattern = regexp.MustCompile(`(?m)^#EXT-X-BIGO-WEB-PROTECTION:.*?\bSEED=(\d+)`)

// bigoProtection is the segment mask a media playlist announced. A playlist that
// announces none leaves active false, which is every Origin but BIGO and, so
// far, most of BIGO's own edges.
type bigoProtection struct {
	seed   uint32
	active bool
}

// parseBigoProtection reads the mask seed from a media playlist body. The seed
// is taken modulo 2^32, the width the keystream runs at.
func parseBigoProtection(playlist []byte) bigoProtection {
	m := bigoSeedPattern.FindSubmatch(playlist)
	if m == nil {
		return bigoProtection{}
	}
	seed, err := strconv.ParseUint(string(m[1]), 10, 64)
	if err != nil {
		return bigoProtection{}
	}
	return bigoProtection{seed: uint32(seed), active: true}
}

// bigoState is everything one ingest keeps for the mask: the seed the current
// playlist announced, and the latches that hold each log line to one.
type bigoState struct {
	bigoProtection
	unmaskLogged     bool
	unmaskFailLogged bool
}

// observe records the mask the playlist just fetched announces, replacing what
// the last one did — a playlist that stops carrying the tag stops unmasking, and
// an Origin that rotates its seed is followed.
func (s *bigoState) observe(playlist []byte) {
	s.bigoProtection = parseBigoProtection(playlist)
}

// bigoUnmaskResult is what one segment needed.
type bigoUnmaskResult int

const (
	// bigoUnmaskNotNeeded is a segment that already begins on sync bytes, which
	// an Origin may serve alongside masked ones.
	bigoUnmaskNotNeeded bigoUnmaskResult = iota
	// bigoUnmaskDone is a segment whose PAT and PMT were recovered.
	bigoUnmaskDone
	// bigoUnmaskFailed is a masked segment the seed no longer reverses — the
	// expected shape of BIGO changing the scheme.
	bigoUnmaskFailed
)

// unmask reverses the mask over a segment's leading packets, in place. Each
// packet is XORed with its own xorshift32 keystream, seeded from the playlist
// seed and the packet's index.
//
// It is safe to call on any segment: one that already starts on sync bytes is
// left alone, and a result that does not is discarded rather than written, so a
// seed that stops matching the mask costs the PAT and PMT rather than the
// payload behind them.
func (b bigoProtection) unmask(body []byte) bigoUnmaskResult {
	if !b.active || len(body) < bigoMaskedPackets*bigoTSPacketSize {
		return bigoUnmaskNotNeeded
	}
	if bigoSyncBytesIntact(body) {
		return bigoUnmaskNotNeeded
	}

	work := make([]byte, bigoMaskedPackets*bigoTSPacketSize)
	copy(work, body)
	for num := uint32(0); num < bigoMaskedPackets; num++ {
		r := b.seed ^ ((num + 1) * bigoKnuth)
		if r == 0 {
			r = bigoSeedFallback
		}
		base := int(num) * bigoTSPacketSize
		for off := 0; off < bigoMaskedBytes; off++ {
			r ^= r << 13
			r ^= r >> 17
			r ^= r << 5
			mask := byte(r)
			if mask == 0 {
				mask = bigoMaskFallback
			}
			work[base+off] ^= mask
		}
	}

	if !bigoSyncBytesIntact(work) {
		return bigoUnmaskFailed
	}
	copy(body, work)
	return bigoUnmaskDone
}

// bigoSyncBytesIntact reports whether the masked packets begin where an MPEG-TS
// packet should, which is what tells a segment that needs unmasking from one
// that does not.
func bigoSyncBytesIntact(b []byte) bool {
	for num := 0; num < bigoMaskedPackets; num++ {
		if b[num*bigoTSPacketSize] != bigoTSSyncByte {
			return false
		}
	}
	return true
}

// unmaskBigoSegment reverses the mask on one segment, in place. A segment the
// mask does not reverse is handed on as it came, since its payload is still the
// broadcast; the group's video-track check then fails the job rather than
// leaving a silent audio-only VOD.
func (o *ingest) unmaskBigoSegment(body []byte, segmentURL string) {
	switch o.bigo.unmask(body) {
	case bigoUnmaskDone:
		if !o.bigo.unmaskLogged {
			o.bigo.unmaskLogged = true
			o.logger.Info("Unmasking BIGO web-protected segments", "seed", o.bigo.seed)
		}
	case bigoUnmaskFailed:
		if !o.bigo.unmaskFailLogged {
			o.bigo.unmaskFailLogged = true
			o.logger.Warn("BIGO web-protected segment did not unmask; recording may lack video",
				"seed", o.bigo.seed, "segment_url", segmentURL, "bytes", len(body))
		}
	}
}
