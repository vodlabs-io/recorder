package hlsingest

import (
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// maskedSegmentHex is the first three MPEG-TS packets of a real web-protected
// segment, masked as the Origin served it. Its seed is maskedSegmentSeed.
// Sourced from the streamlink project's fixture for the same mask.
const maskedSegmentSeed = 2239971888

const maskedSegmentHex = "" +
	"ccf42fbfdc1217ee518d9c76139d5ba6012e701905ffffffffffffffffffffffffffffffffffffffffffffffffffffff" +
	"ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff" +
	"ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff" +
	"ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffe7870a1c" +
	"ae3e065f410941cfa189f248000fe101f0001be100f000f2d91563ffffffffffffffffffffffffffffffffffffffffff" +
	"ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff" +
	"ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff" +
	"ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff474100300710d19c" +
	"35c57e00000001e058ec80c00a3d8ce1efb11d8ce1d7150000000109f00000016764001eacd302c0bfe59a808080a000" +
	"0003002000000611e2c5a700000168ea8f2c000001658882003e637f9f812cfbdfe0523c2139ef339c109bd3c247f04c" +
	"43c654333fe0000003000003000003000c6088d6d0ed000ce3b5fc69f63bcbc5f7c7a1f221fd3f65d97b94704446bfba" +
	"88bece98dc14dff91a8ac3dfe737f2e07dcb8c2d79b7358763d4151def0934240b777ce6"

func maskedSegment(t *testing.T) []byte {
	t.Helper()
	b, err := hex.DecodeString(maskedSegmentHex)
	if err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	if len(b) != 3*bigoTSPacketSize {
		t.Fatalf("fixture is %d bytes, want %d", len(b), 3*bigoTSPacketSize)
	}
	return b
}

func TestParseBigoProtection(t *testing.T) {
	tests := []struct {
		name     string
		playlist string
		want     bigoProtection
	}{
		{
			name:     "tag present",
			playlist: "#EXTM3U\n#EXT-X-BIGO-WEB-PROTECTION:VERSION=1,SEED=2239971888\n#EXTINF:2.0,\n0.ts\n",
			want:     bigoProtection{seed: 2239971888, active: true},
		},
		{
			name:     "seed ahead of version",
			playlist: "#EXTM3U\n#EXT-X-BIGO-WEB-PROTECTION:SEED=1,VERSION=1\n",
			want:     bigoProtection{seed: 1, active: true},
		},
		{
			name:     "carriage returns",
			playlist: "#EXTM3U\r\n#EXT-X-BIGO-WEB-PROTECTION:VERSION=1,SEED=7\r\n",
			want:     bigoProtection{seed: 7, active: true},
		},
		{
			// The keystream runs at 32 bits, so a wider seed is taken modulo 2^32
			// rather than dropped.
			name:     "seed wider than 32 bits",
			playlist: "#EXT-X-BIGO-WEB-PROTECTION:SEED=4294967297\n",
			want:     bigoProtection{seed: 1, active: true},
		},
		{
			name:     "no tag",
			playlist: "#EXTM3U\n#EXTINF:2.0,\n0.ts\n",
			want:     bigoProtection{},
		},
		{
			name:     "tag without a seed",
			playlist: "#EXT-X-BIGO-WEB-PROTECTION:VERSION=1\n",
			want:     bigoProtection{},
		},
		{
			// Only a tag of its own, never one quoted inside another attribute.
			name:     "tag name inside another tag",
			playlist: `#EXT-X-DATERANGE:X="#EXT-X-BIGO-WEB-PROTECTION:SEED=5"` + "\n",
			want:     bigoProtection{},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseBigoProtection([]byte(tc.playlist)); got != tc.want {
				t.Errorf("parseBigoProtection() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestBigoUnmaskRecoversTables is the whole point of the mask: without the PAT and
// the PMT a demuxer cannot learn that the segment carries video, and writes an
// audio-only file without reporting an error.
func TestBigoUnmaskRecoversTables(t *testing.T) {
	body := maskedSegment(t)
	tail := append([]byte(nil), body[2*bigoTSPacketSize:]...)

	w := bigoProtection{seed: maskedSegmentSeed, active: true}
	if got := w.unmask(body); got != bigoUnmaskDone {
		t.Fatalf("unmask() = %v, want bigoUnmaskDone", got)
	}

	for i := 0; i < 3; i++ {
		if got := body[i*bigoTSPacketSize]; got != bigoTSSyncByte {
			t.Errorf("packet %d starts with 0x%02X, want 0x%02X", i, got, bigoTSSyncByte)
		}
	}
	// PID 0 carrying table_id 0 is the PAT; the PMT names its own PID.
	if pid := uint16(body[1]&0x1F)<<8 | uint16(body[2]); pid != 0 {
		t.Errorf("packet 0 PID = 0x%04X, want 0x0000 (PAT)", pid)
	}
	if tableID := body[5]; tableID != 0x00 {
		t.Errorf("packet 0 table_id = 0x%02X, want 0x00 (PAT)", tableID)
	}
	if tableID := body[bigoTSPacketSize+5]; tableID != 0x02 {
		t.Errorf("packet 1 table_id = 0x%02X, want 0x02 (PMT)", tableID)
	}
	// 0x1B is H.264 and 0x0F is AAC: the two stream types the demuxer could not
	// find while the tables were masked.
	pmt := body[bigoTSPacketSize : 2*bigoTSPacketSize]
	if !strings.Contains(string(pmt), string([]byte{0x1B, 0xE1, 0x00})) {
		t.Error("PMT does not declare an H.264 elementary stream")
	}
	if !strings.Contains(string(pmt), string([]byte{0x0F, 0xE1, 0x01})) {
		t.Error("PMT does not declare an AAC elementary stream")
	}
	// Only the masked head is rewritten; every packet after it is the payload.
	if !bytesEqual(body[2*bigoTSPacketSize:], tail) {
		t.Error("unmask rewrote bytes past the masked packets")
	}
}

func TestBigoUnmaskSkipsAndRefuses(t *testing.T) {
	tests := []struct {
		name string
		w    bigoProtection
		body func(t *testing.T) []byte
		want bigoUnmaskResult
	}{
		{
			name: "no tag leaves every Origin untouched",
			w:    bigoProtection{},
			body: maskedSegment,
			want: bigoUnmaskNotNeeded,
		},
		{
			// An Origin may serve unmasked segments alongside masked ones, and
			// masking one of those is the corruption this exists to undo.
			name: "already on sync bytes",
			w:    bigoProtection{seed: maskedSegmentSeed, active: true},
			body: func(t *testing.T) []byte {
				b := maskedSegment(t)
				w := bigoProtection{seed: maskedSegmentSeed, active: true}
				if got := w.unmask(b); got != bigoUnmaskDone {
					t.Fatalf("setup unmask() = %v", got)
				}
				return b
			},
			want: bigoUnmaskNotNeeded,
		},
		{
			name: "shorter than the masked head",
			w:    bigoProtection{seed: maskedSegmentSeed, active: true},
			body: func(t *testing.T) []byte { return maskedSegment(t)[:bigoTSPacketSize] },
			want: bigoUnmaskNotNeeded,
		},
		{
			// A seed that stops matching must cost the tables, not the payload.
			name: "wrong seed",
			w:    bigoProtection{seed: maskedSegmentSeed + 1, active: true},
			body: maskedSegment,
			want: bigoUnmaskFailed,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			body := tc.body(t)
			before := append([]byte(nil), body...)
			if got := tc.w.unmask(body); got != tc.want {
				t.Fatalf("unmask() = %v, want %v", got, tc.want)
			}
			if !bytesEqual(body, before) {
				t.Error("unmask rewrote a segment it did not recover")
			}
		})
	}
}

// TestBigoUnmaskIsReversible pins the keystream: masking is its own inverse, so
// applying the transform to a recovered segment returns the Origin's bytes.
func TestBigoUnmaskIsReversible(t *testing.T) {
	masked := maskedSegment(t)
	body := maskedSegment(t)

	w := bigoProtection{seed: maskedSegmentSeed, active: true}
	if got := w.unmask(body); got != bigoUnmaskDone {
		t.Fatalf("unmask() = %v, want bigoUnmaskDone", got)
	}
	// Re-applying the XOR by hand, since unmask itself declines a segment that
	// already starts on sync bytes.
	for num := uint32(0); num < bigoMaskedPackets; num++ {
		r := w.seed ^ ((num + 1) * bigoKnuth)
		if r == 0 {
			r = bigoSeedFallback
		}
		for off := 0; off < bigoMaskedBytes; off++ {
			r ^= r << 13
			r ^= r >> 17
			r ^= r << 5
			mask := byte(r)
			if mask == 0 {
				mask = bigoMaskFallback
			}
			body[int(num)*bigoTSPacketSize+off] ^= mask
		}
	}
	if !bytesEqual(body, masked) {
		t.Error("re-masking did not reproduce the Origin's bytes")
	}
}

// protectedOrigin serves one masked segment under a playlist that announces the
// mask, so an ingest has to read the tag to hand over anything demuxable.
func protectedOrigin(t *testing.T, tag string, segment []byte) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/live/media.m3u8", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:2\n#EXT-X-MEDIA-SEQUENCE:0\n")
		if tag != "" {
			fmt.Fprint(w, tag+"\n")
		}
		fmt.Fprint(w, "#EXTINF:2.000,\na0.ts\n#EXT-X-ENDLIST\n")
	})
	mux.HandleFunc("/live/a0.ts", func(w http.ResponseWriter, r *http.Request) {
		w.Write(segment)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// TestIngestUnmasksBigoBeforeTheSink pins where the mask is undone. Every Sink
// archives what it is handed, so a Sink that receives masked bytes writes an
// unplayable segment however careful it is; doing it in the ingest is what
// keeps the two Sinks from having to agree.
func TestIngestUnmasksBigoBeforeTheSink(t *testing.T) {
	tests := []struct {
		name       string
		tag        string
		wantSync   bool
		wantChange bool
	}{
		{
			name:       "playlist announces the mask",
			tag:        "#EXT-X-BIGO-WEB-PROTECTION:VERSION=1,SEED=2239971888",
			wantSync:   true,
			wantChange: true,
		},
		{
			// Without the tag there is nothing to reverse, and a guess would
			// corrupt every other Origin's segments.
			name:       "no tag",
			tag:        "",
			wantSync:   false,
			wantChange: false,
		},
		{
			// The seed is the Origin's to rotate; one that no longer matches
			// leaves the segment as it arrived.
			name:       "seed does not match",
			tag:        "#EXT-X-BIGO-WEB-PROTECTION:VERSION=1,SEED=1",
			wantSync:   false,
			wantChange: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			masked := maskedSegment(t)
			srv := protectedOrigin(t, tc.tag, masked)
			sink := &capture{}
			if err := ingestOrigin(t, srv, sink, 0); err != nil {
				t.Fatalf("ingest: %v", err)
			}

			bodies := sink.bodies()
			if len(bodies) != 1 {
				t.Fatalf("handed over %d segments, want 1", len(bodies))
			}
			got := []byte(bodies[0])
			if len(got) != len(masked) {
				t.Fatalf("segment is %d bytes, want %d", len(got), len(masked))
			}
			if gotSync := bigoSyncBytesIntact(got); gotSync != tc.wantSync {
				t.Errorf("bigoSyncBytesIntact(handed over) = %v, want %v", gotSync, tc.wantSync)
			}
			if changed := !bytesEqual(got, masked); changed != tc.wantChange {
				t.Errorf("segment changed = %v, want %v", changed, tc.wantChange)
			}
			// The payload behind the masked head is never touched.
			if !bytesEqual(got[bigoMaskedPackets*bigoTSPacketSize:], masked[bigoMaskedPackets*bigoTSPacketSize:]) {
				t.Error("bytes past the masked packets were rewritten")
			}
		})
	}
}
