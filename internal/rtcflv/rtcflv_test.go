package rtcflv

import (
	"bytes"
	"encoding/binary"
	"testing"
	"time"
)

type flvTag struct {
	typ     byte
	ts      uint32
	payload []byte
}

func readFLV(t *testing.T, b []byte) []flvTag {
	t.Helper()
	if !bytes.HasPrefix(b, []byte("FLV\x01\x05")) {
		t.Fatalf("bad FLV header % x", b[:min(len(b), 13)])
	}
	b = b[13:]
	var tags []flvTag
	for len(b) > 0 {
		if len(b) < 15 {
			t.Fatalf("truncated tag")
		}
		n := int(b[1])<<16 | int(b[2])<<8 | int(b[3])
		ts := uint32(b[4])<<16 | uint32(b[5])<<8 | uint32(b[6]) | uint32(b[7])<<24
		if prev := binary.BigEndian.Uint32(b[11+n:]); prev != uint32(11+n) {
			t.Fatalf("PreviousTagSize %d, want %d", prev, 11+n)
		}
		tags = append(tags, flvTag{typ: b[0], ts: ts, payload: b[11 : 11+n]})
		b = b[15+n:]
	}
	return tags
}

func avcc(nals ...[]byte) []byte {
	var out []byte
	for _, n := range nals {
		out = binary.BigEndian.AppendUint32(out, uint32(len(n)))
		out = append(out, n...)
	}
	return out
}

func TestFLVMuxer(t *testing.T) {
	sps := []byte{0x67, 0x64, 0x00, 0x1f, 0xac}
	pps := []byte{0x68, 0xee, 0x3c}
	idr := []byte{0x65, 0x88, 0x84}
	slice := []byte{0x41, 0x9a}
	var buf bytes.Buffer
	m := newFLVMuxer(&buf, 2)

	// A frame before any parameter set is not written.
	if err := m.writeVideo(0, avcc(slice)); err != nil {
		t.Fatal(err)
	}
	if err := m.writeVideo(0, avcc([]byte{0x09, 0xf0}, sps, pps, idr)); err != nil {
		t.Fatal(err)
	}
	if err := m.writeAudio(5, []byte{0xfc, 1, 2}); err != nil {
		t.Fatal(err)
	}
	if err := m.writeVideo(33, avcc(slice)); err != nil {
		t.Fatal(err)
	}
	if err := m.writeAudio(25, []byte{0xfc, 3}); err != nil {
		t.Fatal(err)
	}

	tags := readFLV(t, buf.Bytes())
	if len(tags) != 6 {
		t.Fatalf("got %d tags, want 6", len(tags))
	}
	seq := tags[0]
	if seq.typ != flvVideo || !bytes.Equal(seq.payload[:5], []byte{0x17, 0, 0, 0, 0}) {
		t.Fatalf("first tag is not an AVC sequence header: % x", seq.payload[:5])
	}
	if rec := seq.payload[5:]; rec[1] != 0x64 || rec[3] != 0x1f || !bytes.Contains(rec, sps) || !bytes.Contains(rec, pps) {
		t.Fatalf("avcC % x lacks the parameter sets", rec)
	}
	key := tags[1]
	if key.payload[0] != 0x17 || key.payload[1] != 1 || !bytes.Equal(key.payload[5:], avcc(idr)) {
		t.Fatalf("keyframe tag % x: want IDR alone, AUD and parameter sets stripped", key.payload)
	}
	opusSeq := tags[2]
	if opusSeq.typ != flvAudio || !bytes.Equal(opusSeq.payload[:5], []byte{0x90, 'O', 'p', 'u', 's'}) ||
		!bytes.HasPrefix(opusSeq.payload[5:], []byte("OpusHead\x01\x02")) {
		t.Fatalf("Opus sequence start % x", opusSeq.payload)
	}
	if a := tags[3]; a.ts != 5 || !bytes.Equal(a.payload, []byte{0x91, 'O', 'p', 'u', 's', 0xfc, 1, 2}) {
		t.Fatalf("Opus frame tag %+v", a)
	}
	if v := tags[4]; v.ts != 33 || v.payload[0] != 0x27 {
		t.Fatalf("inter frame tag %+v", v)
	}
}

func TestFLVMuxerResendsChangedConfig(t *testing.T) {
	var buf bytes.Buffer
	m := newFLVMuxer(&buf, 2)
	sps1 := []byte{0x67, 0x64, 0x00, 0x1f}
	sps2 := []byte{0x67, 0x64, 0x00, 0x28}
	pps := []byte{0x68, 0xee}
	idr := []byte{0x65, 0x88}
	for i, sps := range [][]byte{sps1, sps1, sps2} {
		if err := m.writeVideo(int64(i*33), avcc(sps, pps, idr)); err != nil {
			t.Fatal(err)
		}
	}
	var headers int
	for _, tag := range readFLV(t, buf.Bytes()) {
		if tag.payload[1] == 0 {
			headers++
		}
	}
	if headers != 2 {
		t.Fatalf("got %d sequence headers, want 2 (one per distinct SPS)", headers)
	}
}

func TestHasIDR(t *testing.T) {
	if !hasIDR(avcc([]byte{0x67, 1}, []byte{0x65, 2})) {
		t.Error("IDR not found")
	}
	if hasIDR(avcc([]byte{0x41, 1})) || hasIDR([]byte{0, 0, 0, 9, 1}) {
		t.Error("IDR found where there is none")
	}
}

func TestMediaClock(t *testing.T) {
	c := newMediaClock()
	start := time.Unix(1_800_000_000, 0)

	// No Sender Report yet: samples wait.
	if _, ok := c.wall(1, 90000, 1000, start); ok {
		t.Fatal("stamped before a Sender Report or the wait expired")
	}
	// With a report, a sample is placed relative to it, including across a
	// 32-bit wrap.
	ntp := uint64(1_800_000_000+2208988800) << 32
	srRTP := uint32(0xffffff00)
	c.senderReport(1, 90000, ntp, srRTP)
	at, ok := c.wall(1, 90000, srRTP+90000, start)
	if !ok || !at.Equal(start.Add(time.Second)) {
		t.Fatalf("got %v %v, want %v", at, ok, start.Add(time.Second))
	}

	// A later report that jitters within reanchorAfter leaves the anchor be;
	// one past it re-anchors.
	c.senderReport(1, 90000, ntp+(uint64(11)<<32)/10, srRTP+90000) // 100 ms late
	if at, _ := c.wall(1, 90000, srRTP+180000, start); !at.Equal(start.Add(2 * time.Second)) {
		t.Fatalf("jittery report moved the anchor: %v", at)
	}
	c.senderReport(1, 90000, ntp+(uint64(5)<<32), srRTP+90000) // 4 s off: the RTP clock jumped
	if at, _ := c.wall(1, 90000, srRTP+90000, start); !at.Equal(start.Add(5 * time.Second)) {
		t.Fatalf("large disagreement did not re-anchor: %v", at)
	}

	// A track never reported on falls back to arrival time once srWait passes.
	if _, ok := c.wall(2, 48000, 500, start); ok {
		t.Fatal("audio stamped before the wait expired")
	}
	late := start.Add(srWait + time.Millisecond)
	at, ok = c.wall(2, 48000, 500+48000, late)
	if !ok || !at.Equal(start.Add(time.Second)) {
		t.Fatalf("arrival fallback: got %v %v, want %v", at, ok, start.Add(time.Second))
	}
}
