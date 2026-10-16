package webm

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"
)

// flvStream builds an FLV stream the way rtcflv writes a VP8/Opus channel.
type flvStream struct{ bytes.Buffer }

func newFLV() *flvStream {
	f := &flvStream{}
	f.Write([]byte{'F', 'L', 'V', 1, 0x05, 0, 0, 0, 9, 0, 0, 0, 0})
	return f
}

func (f *flvStream) tag(typ byte, ts uint32, payload []byte) {
	n := len(payload)
	f.Write([]byte{typ, byte(n >> 16), byte(n >> 8), byte(n), byte(ts >> 16), byte(ts >> 8), byte(ts), byte(ts >> 24), 0, 0, 0})
	f.Write(payload)
	binary.Write(f, binary.BigEndian, uint32(11+n))
}

func (f *flvStream) vp8(ts uint32, key bool) {
	head := []byte{0x80 | 2<<4 | 1, 'v', 'p', '0', '8'}
	frame := []byte{0x01, 0, 0, 0xaa, 0xbb}
	if key {
		head[0] = 0x80 | 1<<4 | 1
		// Key frame tag, start code, 640x360.
		frame = []byte{0x00, 0, 0, 0x9d, 0x01, 0x2a, 0x80, 0x02, 0x68, 0x01, 0xcc}
	}
	f.tag(9, ts, append(head, frame...))
}

func (f *flvStream) vp8Config(ts uint32) {
	f.tag(9, ts, []byte{0x80 | 1<<4 | 0, 'v', 'p', '0', '8', 0, 10, 0x82, 2, 2, 2, 0, 0})
}

func (f *flvStream) opusHead(ts uint32) {
	h := []byte("OpusHead")
	h = append(h, 1, 2, 0x38, 0x01, 0x80, 0xbb, 0, 0, 0, 0, 0)
	f.tag(8, ts, append([]byte{0x90, 'O', 'p', 'u', 's'}, h...))
}

func (f *flvStream) opus(ts uint32) {
	f.tag(8, ts, []byte{0x91, 'O', 'p', 'u', 's', 0xfc, 0xff, 0xfe})
}

// element is one parsed EBML element.
type element struct {
	id   uint32
	data []byte // nil for an unknown-size element, whose children follow it
}

// walk lists the elements of a WebM byte stream depth-first, descending into
// the master elements a remuxer writes.
func walk(t *testing.T, b []byte) []element {
	t.Helper()
	masters := map[uint32]bool{ebmlHeader: true, mkvSegment: true, mkvInfo: true, mkvTracks: true,
		mkvTrackEntry: true, mkvVideo: true, mkvAudio: true, mkvCluster: true}
	var out []element
	for len(b) > 0 {
		idLen := 1
		for m := byte(0x80); idLen <= 4 && b[0]&m == 0; m >>= 1 {
			idLen++
		}
		var id uint32
		for _, c := range b[:idLen] {
			id = id<<8 | uint32(c)
		}
		b = b[idLen:]
		szLen := 1
		for m := byte(0x80); b[0]&m == 0; m >>= 1 {
			szLen++
		}
		size := uint64(b[0]) & (0xff >> szLen)
		for _, c := range b[1:szLen] {
			size = size<<8 | uint64(c)
		}
		unknown := bytes.Equal(b[:szLen], unknownSize)
		b = b[szLen:]
		if unknown {
			out = append(out, element{id: id})
			continue
		}
		if size > uint64(len(b)) {
			t.Fatalf("element %x overruns the stream: %d > %d", id, size, len(b))
		}
		data := b[:size]
		b = b[size:]
		out = append(out, element{id: id, data: data})
		if masters[id] {
			out = append(out, walk(t, data)...)
		}
	}
	return out
}

func count(els []element, id uint32) int {
	n := 0
	for _, e := range els {
		if e.id == id {
			n++
		}
	}
	return n
}

func find(els []element, id uint32) []byte {
	for _, e := range els {
		if e.id == id {
			return e.data
		}
	}
	return nil
}

func remux(t *testing.T, in []byte, chunk int) []byte {
	t.Helper()
	var out bytes.Buffer
	r := NewRemuxer(&out)
	for len(in) > 0 {
		n := min(chunk, len(in))
		if _, err := r.Write(in[:n]); err != nil {
			t.Fatalf("Write: %v", err)
		}
		in = in[n:]
	}
	return out.Bytes()
}

func TestRemuxVP8Opus(t *testing.T) {
	f := newFLV()
	f.opusHead(0)
	f.opus(0)       // before the first keyframe: dropped
	f.vp8(5, false) // before the first keyframe: dropped
	f.vp8Config(10)
	f.vp8(10, true)
	f.opus(20)
	f.vp8(43, false)
	f.opus(40)
	f.vp8(4010, true)
	f.opus(4020)

	whole := remux(t, f.Bytes(), f.Len())
	if !bytes.Equal(whole, remux(t, f.Bytes(), 1)) {
		t.Fatal("output depends on how the input is split")
	}

	els := walk(t, whole)
	if got := string(find(els, ebmlDocType)); got != "webm" {
		t.Errorf("DocType = %q", got)
	}
	if count(els, mkvTrackEntry) != 2 {
		t.Fatalf("want 2 tracks, got %d", count(els, mkvTrackEntry))
	}
	if got := string(find(els, mkvCodecID)); got != "V_VP8" {
		t.Errorf("first CodecID = %q", got)
	}
	if w, h := find(els, mkvPixelWidth), find(els, mkvPixelHeight); !bytes.Equal(w, []byte{0x02, 0x80}) || !bytes.Equal(h, []byte{0x01, 0x68}) {
		t.Errorf("size = %x x %x, want 640x360", w, h)
	}
	if p := find(els, mkvCodecPrivate); !bytes.HasPrefix(p, []byte("OpusHead")) {
		t.Errorf("CodecPrivate = %x", p)
	}
	if got := count(els, mkvCluster); got != 2 {
		t.Errorf("clusters = %d, want one per keyframe (2)", got)
	}
	if got := count(els, mkvSimpleBlock); got != 6 {
		t.Errorf("blocks = %d, want 6", got)
	}
	// The first block is the keyframe, at the first cluster's start.
	for _, e := range els {
		if e.id == mkvSimpleBlock {
			if e.data[0] != 0x80|videoTrack || e.data[1] != 0 || e.data[2] != 0 || e.data[3] != 0x80 {
				t.Errorf("first block head = %x", e.data[:4])
			}
			break
		}
	}
}

// Without an Opus sequence header within audioWaitMs of the first keyframe, the
// file is video-only, and audio that turns up later is left out.
func TestRemuxVideoOnly(t *testing.T) {
	f := newFLV()
	f.vp8(0, true)
	f.vp8(1000, false)
	f.vp8(2500, false)
	f.opusHead(3000)
	f.opus(3000)
	f.vp8(3033, false)

	els := walk(t, remux(t, f.Bytes(), 7))
	if count(els, mkvTrackEntry) != 1 || count(els, mkvAudio) != 0 {
		t.Fatalf("want one video track, got %d tracks", count(els, mkvTrackEntry))
	}
	if got := count(els, mkvSimpleBlock); got != 4 {
		t.Errorf("blocks = %d, want the 4 video frames", got)
	}
}

func TestRemuxRejectsOtherCodecs(t *testing.T) {
	cases := map[string]func(*flvStream){
		"avc": func(f *flvStream) { f.tag(9, 0, []byte{0x17, 0, 0, 0, 0, 1, 0x64}) },
		"aac": func(f *flvStream) { f.tag(8, 0, []byte{0xaf, 0, 0x12, 0x10}) },
	}
	for name, add := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFLV()
			add(f)
			_, err := NewRemuxer(&bytes.Buffer{}).Write(f.Bytes())
			if !errors.Is(err, ErrUnsupportedCodec) {
				t.Fatalf("err = %v, want ErrUnsupportedCodec", err)
			}
		})
	}
}
