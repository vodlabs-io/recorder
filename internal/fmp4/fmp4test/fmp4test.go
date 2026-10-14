// Package fmp4test builds small fragmented MP4 and reads FLV streams for
// tests.
package fmp4test

import (
	"bytes"
	"encoding/binary"
	"testing"
)

var (
	AVCC = []byte{1, 0x4d, 0x40, 0x28, 0xff, 0xe1, 0, 4, 0x67, 0x4d, 0x40, 0x28, 1, 0, 2, 0x68, 0xee}
	ASC  = []byte{0x11, 0x90}
)

// trun and tfhd flags the fragments use.
const (
	trDataOffset        = 0x1
	trFirstSampleFlags  = 0x4
	trDuration          = 0x100
	trSize              = 0x200
	trCTS               = 0x800
	tfDefaultBaseIsMoof = 0x20000
)

func box(typ string, parts ...[]byte) []byte {
	body := bytes.Join(parts, nil)
	b := make([]byte, 8, 8+len(body))
	binary.BigEndian.PutUint32(b, uint32(8+len(body)))
	copy(b[4:], typ)
	return append(b, body...)
}

func u32(v uint32) []byte { return binary.BigEndian.AppendUint32(nil, v) }
func u64(v uint64) []byte { return binary.BigEndian.AppendUint64(nil, v) }

func fullBox(typ string, ver byte, flags uint32, parts ...[]byte) []byte {
	return box(typ, append([]byte{ver, byte(flags >> 16), byte(flags >> 8), byte(flags)}, bytes.Join(parts, nil)...))
}

func trak(id uint32, handler string, timescale uint32, entry []byte) []byte {
	tkhd := fullBox("tkhd", 0, 3, make([]byte, 8), u32(id), make([]byte, 68))
	mdhd := fullBox("mdhd", 0, 0, make([]byte, 8), u32(timescale), make([]byte, 8))
	hdlr := fullBox("hdlr", 0, 0, u32(0), []byte(handler), make([]byte, 13))
	stsd := fullBox("stsd", 0, 0, u32(1), entry)
	return box("trak", tkhd, box("mdia", mdhd, hdlr, box("minf", box("stbl", stsd))))
}

// Init is an init segment: video track 1 (ms), audio track 2 (48 kHz).
func Init() []byte {
	avc3 := box("avc3", make([]byte, 78), box("avcC", AVCC))
	esd := []byte{0x03, 0x19, 0, 1, 0, 0x04, 0x11, 0x40, 0x15}
	esd = append(esd, make([]byte, 11)...)
	esd = append(esd, 0x05, byte(len(ASC)))
	esd = append(esd, ASC...)
	esd = append(esd, 0x06, 1, 2)
	mp4a := box("mp4a", make([]byte, 28), fullBox("esds", 0, 0, esd))
	return append(box("ftyp", []byte("iso5")), box("moov",
		trak(1, "vide", 1000, avc3), trak(2, "soun", 48000, mp4a))...)
}

// Frag is one fragment of one sample.
func Frag(track uint32, dts uint64, dur uint32, cts uint32, data []byte) []byte {
	build := func(off uint32) []byte {
		if track == 1 {
			return fullBox("trun", 0, trDataOffset|trFirstSampleFlags|trDuration|trSize|trCTS,
				u32(1), u32(off), u32(0), u32(dur), u32(uint32(len(data))), u32(cts))
		}
		return fullBox("trun", 0, trDataOffset|trDuration|trSize, u32(1), u32(off), u32(dur), u32(uint32(len(data))))
	}
	moof := func(off uint32) []byte {
		return box("moof", fullBox("mfhd", 0, 0, u32(1)), box("traf",
			fullBox("tfhd", 0, tfDefaultBaseIsMoof, u32(track)),
			fullBox("tfdt", 1, 0, u64(dts)), build(off)))
	}
	m := moof(0)
	m = moof(uint32(len(m) + 8))
	return append(m, box("mdat", data)...)
}

// VideoAU is a one-NAL access unit, an IDR slice when key.
func VideoAU(key bool, payload byte) []byte {
	nal := byte(0x41)
	if key {
		nal = 0x65
	}
	return []byte{0, 0, 0, 2, nal, payload}
}

// Stream is an init segment and frames 33 ms apart from dts, the first a
// keyframe, each followed by an audio frame.
func Stream(dts uint64, frames int) []byte {
	var b bytes.Buffer
	b.Write(Init())
	for i := 0; i < frames; i++ {
		b.Write(Frag(1, dts+uint64(i)*33, 33, 0, VideoAU(i == 0, byte(i))))
		b.Write(Frag(2, (dts+uint64(i)*33)*48, 1584, 0, []byte{byte(i)}))
	}
	return b.Bytes()
}

// FLV tag types.
const (
	FLVAudio = 8
	FLVVideo = 9
)

// Tag is one FLV tag: its type, timestamp, and whether it is a sequence
// header.
type Tag struct {
	Type byte
	TS   int64
	Seq  bool
}

// ReadFLV reads the tags of an FLV stream.
func ReadFLV(t testing.TB, b []byte) []Tag {
	t.Helper()
	if len(b) < 13 || string(b[:3]) != "FLV" {
		t.Fatalf("not an FLV stream")
	}
	b = b[13:]
	var tags []Tag
	for len(b) >= 15 {
		n := int(b[1])<<16 | int(b[2])<<8 | int(b[3])
		ts := int64(b[4])<<16 | int64(b[5])<<8 | int64(b[6]) | int64(b[7])<<24
		payload := b[11 : 11+n]
		tags = append(tags, Tag{Type: b[0], TS: ts, Seq: payload[1] == 0})
		b = b[15+n:]
	}
	return tags
}
