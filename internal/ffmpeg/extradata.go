package ffmpeg

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strings"
)

// hexColumnWidth is the width of the hex column in ffprobe's -show_data dump
// ("00000000: 0164 001f …  .d……"): eight 4-char groups, single-space
// separated. The ASCII column that follows can contain anything, so it is
// sliced off by position rather than parsed.
const hexColumnWidth = 39

// hexDumpBytes decodes an ffprobe -show_data dump back into bytes. Reports
// false for anything that doesn't parse, leaving the caller on its fallback.
func hexDumpBytes(dump string) ([]byte, bool) {
	var out []byte
	for _, line := range strings.Split(strings.TrimSpace(dump), "\n") {
		_, rest, found := strings.Cut(strings.TrimRight(line, "\r"), ": ")
		if !found {
			return nil, false
		}
		if len(rest) > hexColumnWidth {
			rest = rest[:hexColumnWidth]
		}
		b, err := hex.DecodeString(strings.ReplaceAll(rest, " ", ""))
		if err != nil {
			return nil, false
		}
		out = append(out, b...)
	}
	return out, len(out) > 0
}

// canonicalAVCC rewrites an avcC decoder configuration record so that only
// its meaning survives, not its layout: a muxer that lists the same SPS or
// PPS more than once produces different bytes for an identical decoder
// configuration. Reports false when b is not a well-formed avcC.
func canonicalAVCC(b []byte) ([]byte, bool) {
	// 5-byte header, SPS count, SPS list, PPS count, PPS list, then a
	// trailer on High-profile records (chroma format, bit depths).
	if len(b) < 7 || b[0] != 1 {
		return nil, false
	}
	pos := 5
	numSPS := int(b[pos] & 0x1f)
	pos++
	sps, ok := readParamSets(b, &pos, numSPS)
	if !ok || pos >= len(b) {
		return nil, false
	}
	numPPS := int(b[pos])
	pos++
	pps, ok := readParamSets(b, &pos, numPPS)
	if !ok {
		return nil, false
	}

	out := append([]byte{}, b[:5]...) // version, profile, level, NAL length size
	for _, set := range distinctSets(sps) {
		out = append(out, set...)
	}
	for _, set := range distinctSets(pps) {
		out = append(out, set...)
	}
	return append(out, b[pos:]...), true // trailer, when the record has one
}

// readParamSets reads n length-prefixed parameter sets starting at *pos, and
// leaves *pos just past the last one. Each set keeps its length prefix, so
// two different splits can never canonicalize alike.
func readParamSets(b []byte, pos *int, n int) ([][]byte, bool) {
	sets := make([][]byte, 0, n)
	for i := 0; i < n; i++ {
		if *pos+2 > len(b) {
			return nil, false
		}
		size := int(b[*pos])<<8 | int(b[*pos+1])
		end := *pos + 2 + size
		if end > len(b) {
			return nil, false
		}
		sets = append(sets, b[*pos:end])
		*pos = end
	}
	return sets, true
}

// distinctSets drops repeats and orders what's left, so listing order and
// multiplicity cannot change the fingerprint.
func distinctSets(sets [][]byte) [][]byte {
	seen := make(map[string]bool, len(sets))
	out := make([][]byte, 0, len(sets))
	for _, s := range sets {
		if k := string(s); !seen[k] {
			seen[k] = true
			out = append(out, s)
		}
	}
	slices.SortFunc(out, bytes.Compare)
	return out
}

// extradataFingerprint identifies a stream's decoder configuration. H.264
// records are canonicalized first; anything else falls back to ffprobe's
// hash of the raw bytes, which is conservative (a layout-only difference
// reads as a real one) but never mistakes two configurations for one.
func extradataFingerprint(dump, rawHash string) string {
	raw, ok := hexDumpBytes(dump)
	if !ok {
		return rawHash
	}
	canon, ok := canonicalAVCC(raw)
	if !ok {
		return rawHash
	}
	sum := sha256.Sum256(canon)
	return "avcC:" + hex.EncodeToString(sum[:8])
}
