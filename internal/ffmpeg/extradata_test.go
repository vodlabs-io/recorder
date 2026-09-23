package ffmpeg

import (
	"encoding/hex"
	"fmt"
	"strings"
	"testing"
)

// The parameter sets from a real two-part douyin recording. Both parts carry
// the identical SPS and PPS; the second part's muxer just listed each twice.
const (
	sps = "2764001fac131a483c045fb9b808080a000003000200000300641e2c4b24"
	pps = "28ee0672c0"
	// avcC header (version, High, level 31, 4-byte NAL lengths) and the
	// High-profile trailer.
	avccHead    = "0164001fff"
	avccTrailer = "fdf8f800"
)

func avcc(t *testing.T, spsCount, ppsCount int) []byte {
	t.Helper()
	s := avccHead + hex.EncodeToString([]byte{byte(0xe0 | spsCount)})
	for i := 0; i < spsCount; i++ {
		s += "001e" + sps
	}
	s += hex.EncodeToString([]byte{byte(ppsCount)})
	for i := 0; i < ppsCount; i++ {
		s += "0005" + pps
	}
	b, err := hex.DecodeString(s + avccTrailer)
	if err != nil {
		t.Fatalf("build avcC: %v", err)
	}
	return b
}

// The defect this canonicalization exists for: repeating an identical
// parameter set changes the bytes but not the decoder configuration, and a
// stream copy across the two is safe.
func TestCanonicalAVCCIgnoresRepeatedParameterSets(t *testing.T) {
	one, ok := canonicalAVCC(avcc(t, 1, 1))
	if !ok {
		t.Fatal("1×SPS/1×PPS record should parse")
	}
	two, ok := canonicalAVCC(avcc(t, 2, 2))
	if !ok {
		t.Fatal("2×SPS/2×PPS record should parse")
	}
	if string(one) != string(two) {
		t.Errorf("duplicated parameter sets must canonicalize alike:\n %x\n %x", one, two)
	}
}

func TestCanonicalAVCCSeparatesDifferentParameterSets(t *testing.T) {
	base, _ := canonicalAVCC(avcc(t, 1, 1))

	// Same length, one bit of the SPS flipped: a real config difference.
	altHex := avccHead + "e1001e" + sps[:len(sps)-1] + "5" + "01" + "0005" + pps + avccTrailer
	alt, err := hex.DecodeString(altHex)
	if err != nil {
		t.Fatalf("build alt: %v", err)
	}
	got, ok := canonicalAVCC(alt)
	if !ok {
		t.Fatal("alt record should parse")
	}
	if string(got) == string(base) {
		t.Error("a changed SPS must change the canonical form")
	}
}

func TestCanonicalAVCCRejectsMalformed(t *testing.T) {
	full := avcc(t, 1, 1)
	cases := map[string][]byte{
		"empty":            {},
		"too short":        full[:6],
		"wrong version":    append([]byte{0x02}, full[1:]...),
		"truncated SPS":    full[:12],
		"SPS overruns end": append(append([]byte{}, avccHead...), 0xe1, 0xff, 0xff),
	}
	for name, b := range cases {
		if _, ok := canonicalAVCC(b); ok {
			t.Errorf("%s should not parse as avcC", name)
		}
	}
}

func TestHexDumpBytes(t *testing.T) {
	// Verbatim ffprobe -show_data output, ASCII column and all.
	dump := "00000000: 0164 001f ffe1 001e 2764 001f ac13 1a48  .d......'d.....H\n" +
		"00000010: 3c04 5fb9 b808 080a 0000 0300 0200 0003  <._.............\n" +
		"00000020: 0064 1e2c 4b24 0100 0528 ee06 72c0 fdf8  .d.,K$...(..r...\n" +
		"00000030: f800                                     .."
	got, ok := hexDumpBytes(dump)
	if !ok {
		t.Fatal("dump should parse")
	}
	if want := avcc(t, 1, 1); string(got) != string(want) {
		t.Errorf("decoded %x, want %x", got, want)
	}
}

func TestHexDumpBytesRejectsGarbage(t *testing.T) {
	for name, dump := range map[string]string{
		"no offset prefix": "0164 001f",
		"not hex":          "00000000: zzzz zzzz                               ....",
		"empty":            "",
	} {
		if _, ok := hexDumpBytes(dump); ok {
			t.Errorf("%s should not parse", name)
		}
	}
}

// Anything that isn't a parseable avcC keeps ffprobe's raw-byte hash, which
// is conservative rather than wrong.
func TestExtradataFingerprintFallsBackToRawHash(t *testing.T) {
	const raw = "MD5:deadbeef"
	if got := extradataFingerprint("", raw); got != raw {
		t.Errorf("no dump should fall back, got %q", got)
	}
	// An AudioSpecificConfig is not an avcC.
	if got := extradataFingerprint("00000000: 1210                                     ..", raw); got != raw {
		t.Errorf("non-avcC should fall back, got %q", got)
	}
}

// dumpOf renders bytes the way ffprobe -show_data does: 16 bytes per line as
// eight space-separated 2-byte groups, padded to a fixed width, then ASCII.
func dumpOf(b []byte) string {
	var sb strings.Builder
	for off := 0; off < len(b); off += 16 {
		end := min(off+16, len(b))
		hexCol := ""
		for i := off; i < end; i += 2 {
			if i > off {
				hexCol += " "
			}
			hexCol += hex.EncodeToString(b[i:min(i+2, end)])
		}
		fmt.Fprintf(&sb, "%08x: %-*s  %s\n", off, hexColumnWidth, hexCol, strings.Repeat(".", end-off))
	}
	return sb.String()
}

func TestExtradataFingerprintMatchesAcrossLayouts(t *testing.T) {
	one := extradataFingerprint(dumpOf(avcc(t, 1, 1)), "MD5:aaa")
	two := extradataFingerprint(dumpOf(avcc(t, 2, 2)), "MD5:bbb")
	if one != two {
		t.Errorf("fingerprints differ across layouts: %q vs %q", one, two)
	}
	if one == "MD5:aaa" {
		t.Error("expected a canonical fingerprint, got the fallback")
	}
}
