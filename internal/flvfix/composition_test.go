package flvfix

import (
	"bytes"
	"testing"
)

// avcFrame builds a classic AVC coded-frame tag carrying composition time cts.
func avcFrame(ts int64, key bool, cts int32) []byte {
	first := byte(0x27)
	if key {
		first = 0x17
	}
	return buildTag(tagVideo, ts, []byte{first, 0x01,
		byte(cts >> 16), byte(cts >> 8), byte(cts), 0xde, 0xad})
}

// exKeyframe builds an enhanced-FLV keyframe tag for fourCC with packet type
// pktType. Only CodedFrames (1) carries a composition time, and only for AVC
// and HEVC; body stands in for the rest of the payload.
func exKeyframe(ts int64, fourCC string, pktType byte, cts int32, body ...byte) []byte {
	p := append([]byte{0x80 | 1<<4 | pktType}, fourCC...)
	if pktType == 1 && (fourCC == "avc1" || fourCC == "hvc1") {
		p = append(p, byte(cts>>16), byte(cts>>8), byte(cts))
	}
	return buildTag(tagVideo, ts, append(p, body...))
}

// exSeqStart builds an enhanced-FLV sequence-start tag for fourCC.
func exSeqStart(fourCC string) []byte {
	return buildTag(tagVideo, 0, append(append([]byte{0x80 | 1<<4}, fourCC...), 0x01, 0x64))
}

// videoMedia returns the output's video media tag payloads, sequence headers
// excluded.
func videoMedia(t *testing.T, out []byte) [][]byte {
	t.Helper()
	var media [][]byte
	for _, tag := range parseOutput(t, out) {
		if tag.typ == tagVideo && !parseVideoInfo(tag.payload).config {
			media = append(media, tag.payload)
		}
	}
	return media
}

func TestImplausibleCompositionTimeReplaced(t *testing.T) {
	// Around an uplink stall TikTok has sent frames whose composition time puts
	// them over a minute after their decode time. A remux presents such a frame
	// that late, and a re-encode then drops every frame behind it until the
	// timeline catches up, freezing the picture for the whole span.
	src := stream(fileHeader(), vCfgTag(0, 0x41, 22),
		avcFrame(0, true, 133),
		avcFrame(66, false, 133),
		avcFrame(133, false, 66),
		avcFrame(200, true, 64534), // 64.5s ahead
		avcFrame(266, false, 133),
		avcFrame(333, false, -9000), // far behind
		avcFrame(400, false, -33),   // small negative offsets are left alone
	)
	out, s, err := run(t, src)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	checkInvariants(t, parseOutput(t, out.Bytes()))

	want := []int32{133, 133, 66, 66, 133, 133, -33}
	media := videoMedia(t, out.Bytes())
	if len(media) != len(want) {
		t.Fatalf("got %d video frames, want %d", len(media), len(want))
	}
	for i, p := range media {
		if got := si24(p[2:5]); got != want[i] {
			t.Errorf("frame %d: composition time %dms, want %dms", i, got, want[i])
		}
	}
	if got := s.Stats().ctsRepaired; got != 2 {
		t.Errorf("ctsRepaired = %d, want 2", got)
	}

	// Output already repaired passes through a second run untouched.
	again, s2, err := run(t, out.Bytes())
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	for i, p := range videoMedia(t, again.Bytes()) {
		if got := si24(p[2:5]); got != want[i] {
			t.Errorf("second run frame %d: composition time %dms, want %dms", i, got, want[i])
		}
	}
	if got := s2.Stats().ctsRepaired; got != 0 {
		t.Errorf("second run ctsRepaired = %d, want 0", got)
	}
}

func TestEnhancedCompositionTime(t *testing.T) {
	t.Run("avc1 coded frames are repaired", func(t *testing.T) {
		src := stream(fileHeader(), exSeqStart("avc1"),
			exKeyframe(0, "avc1", 1, 100, 0xaa),
			exKeyframe(2000, "avc1", 1, 70000, 0xaa),
		)
		out, s, err := run(t, src)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		media := videoMedia(t, out.Bytes())
		if len(media) != 2 {
			t.Fatalf("got %d video frames, want 2", len(media))
		}
		if got := si24(media[1][5:8]); got != 100 {
			t.Errorf("composition time %dms, want 100ms", got)
		}
		if got := s.Stats().ctsRepaired; got != 1 {
			t.Errorf("ctsRepaired = %d, want 1", got)
		}
	})

	// Payloads without a composition time field must not have one read out of
	// (and written into) their coded data.
	for _, tc := range []struct {
		name    string
		fourCC  string
		pktType byte
	}{
		{"avc1 coded frames X", "avc1", 3},
		{"av01 coded frames", "av01", 1},
	} {
		t.Run(tc.name+" are untouched", func(t *testing.T) {
			body := []byte{0x7f, 0xff, 0xff, 0x12}
			src := stream(fileHeader(), exSeqStart(tc.fourCC),
				exKeyframe(0, tc.fourCC, tc.pktType, 0, body...))
			out, s, err := run(t, src)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			media := videoMedia(t, out.Bytes())
			if len(media) != 1 || !bytes.Equal(media[0][5:], body) {
				t.Fatalf("payload changed: %x", media)
			}
			if got := s.Stats().ctsRepaired; got != 0 {
				t.Errorf("ctsRepaired = %d, want 0", got)
			}
		})
	}
}

func TestRepairsDisabledKeepsCompositionTime(t *testing.T) {
	src := stream(fileHeader(), vCfgTag(0, 0x41, 22),
		avcFrame(0, true, 133), avcFrame(66, true, 64534))
	var out bytes.Buffer
	s := New(&out, nil, Options{RepairsDisabled: true})
	if _, err := s.Write(src); err != nil {
		t.Fatal(err)
	}
	if err := s.Finish(); err != nil {
		t.Fatal(err)
	}
	media := videoMedia(t, out.Bytes())
	if len(media) != 2 {
		t.Fatalf("got %d video frames, want 2", len(media))
	}
	if got := si24(media[1][2:5]); got != 64534 {
		t.Errorf("composition time %dms, want the stream's own 64534ms", got)
	}
}
