package livejasmin

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"testing"
	"time"
)

var (
	testAVCC = []byte{1, 0x4d, 0x40, 0x28, 0xff, 0xe1, 0, 4, 0x67, 0x4d, 0x40, 0x28, 1, 0, 2, 0x68, 0xee}
	testASC  = []byte{0x11, 0x90}
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

// testInit is an init segment: video track 1 (ms), audio track 2 (48 kHz).
func testInit() []byte {
	avc3 := box("avc3", make([]byte, 78), box("avcC", testAVCC))
	esd := []byte{0x03, 0x19, 0, 1, 0, 0x04, 0x11, 0x40, 0x15}
	esd = append(esd, make([]byte, 11)...)
	esd = append(esd, 0x05, byte(len(testASC)))
	esd = append(esd, testASC...)
	esd = append(esd, 0x06, 1, 2)
	mp4a := box("mp4a", make([]byte, 28), fullBox("esds", 0, 0, esd))
	return append(box("ftyp", []byte("iso5")), box("moov",
		trak(1, "vide", 1000, avc3), trak(2, "soun", 48000, mp4a))...)
}

// testFrag is one fragment of one sample, as the site sends them.
func testFrag(track uint32, dts uint64, dur uint32, cts uint32, data []byte) []byte {
	var trun []byte
	build := func(off uint32) []byte {
		if track == 1 {
			return fullBox("trun", 0, trDataOffset|trFirstSampleFlags|trDuration|trSize|trCTS,
				u32(1), u32(off), u32(0), u32(dur), u32(uint32(len(data))), u32(cts))
		}
		return fullBox("trun", 0, trDataOffset|trDuration|trSize, u32(1), u32(off), u32(dur), u32(uint32(len(data))))
	}
	moof := func(off uint32) []byte {
		trun = build(off)
		return box("moof", fullBox("mfhd", 0, 0, u32(1)), box("traf",
			fullBox("tfhd", 0, tfDefaultBaseIsMoof, u32(track)),
			fullBox("tfdt", 1, 0, u64(dts)), trun))
	}
	m := moof(0)
	m = moof(uint32(len(m) + 8))
	return append(m, box("mdat", data)...)
}

func videoAU(key bool, payload byte) []byte {
	nal := byte(0x41)
	if key {
		nal = 0x65
	}
	return []byte{0, 0, 0, 2, nal, payload}
}

func TestDemuxer(t *testing.T) {
	var s bytes.Buffer
	s.Write(testInit())
	s.Write(testFrag(1, 1000, 33, 66, videoAU(true, 1)))
	s.Write(testFrag(2, 48000, 1024, 0, []byte{0xaa, 0xbb}))
	s.Write(testFrag(1, 1033, 33, 0, videoAU(false, 2)))
	d := newDemuxer(&s)

	want := []sample{
		{video: true, dts: 1000, cts: 66, key: true, data: videoAU(true, 1)},
		{video: false, dts: 1000, key: true, data: []byte{0xaa, 0xbb}},
		{video: true, dts: 1033, key: false, data: videoAU(false, 2)},
	}
	for i, w := range want {
		got, err := d.next()
		if err != nil {
			t.Fatalf("sample %d: %v", i, err)
		}
		if got.video != w.video || got.dts != w.dts || got.cts != w.cts || got.key != w.key || !bytes.Equal(got.data, w.data) {
			t.Fatalf("sample %d = %+v, want %+v", i, got, w)
		}
	}
	if !bytes.Equal(d.videoConfig(), testAVCC) || !bytes.Equal(d.audioConfig(), testASC) {
		t.Fatalf("config = %x / %x", d.videoConfig(), d.audioConfig())
	}
	if _, err := d.next(); err != io.EOF {
		t.Fatalf("end = %v, want io.EOF", err)
	}
}

func TestURLRoundTrip(t *testing.T) {
	r := Room{Name: "AbbyMoore", Height: 720}
	got, err := Parse(r.URL())
	if err != nil || got != r {
		t.Fatalf("Parse(%q) = %+v, %v", r.URL(), got, err)
	}
	for _, bad := range []string{"https://www.livejasmin.com/en/chat/x", "livejasmin://", "livejasmin://a/b?height=x"} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("Parse(%q) accepted", bad)
		}
	}
}

// fakeSeat streams a fixed fMP4 and lets go when it has been read.
type fakeSeat struct {
	r    io.Reader
	done chan struct{}
	once sync.Once
}

func (f *fakeSeat) Stream() io.Reader     { return f.r }
func (f *fakeSeat) Done() <-chan struct{} { return f.done }
func (f *fakeSeat) Close()                { f.once.Do(func() { close(f.done) }) }

// seatStreamOf builds a seat's stream: frames from dts, the first a keyframe,
// each followed by an audio frame.
func seatStreamOf(dts uint64, frames int) []byte {
	var b bytes.Buffer
	b.Write(testInit())
	for i := 0; i < frames; i++ {
		b.Write(testFrag(1, dts+uint64(i)*33, 33, 0, videoAU(i == 0, byte(i))))
		b.Write(testFrag(2, (dts+uint64(i)*33)*48, 1584, 0, []byte{byte(i)}))
	}
	return b.Bytes()
}

type flvTag struct {
	typ byte
	ts  int64
	seq bool
}

func readFLV(t *testing.T, b []byte) []flvTag {
	t.Helper()
	if len(b) < 13 || string(b[:3]) != "FLV" {
		t.Fatalf("not an FLV stream")
	}
	b = b[13:]
	var tags []flvTag
	for len(b) >= 15 {
		n := int(b[1])<<16 | int(b[2])<<8 | int(b[3])
		ts := int64(b[4])<<16 | int64(b[5])<<8 | int64(b[6]) | int64(b[7])<<24
		payload := b[11 : 11+n]
		tags = append(tags, flvTag{typ: b[0], ts: ts, seq: payload[1] == 0})
		b = b[15+n:]
	}
	return tags
}

func TestSplicesSeatsOntoOneTimeline(t *testing.T) {
	streams := [][]byte{seatStreamOf(5000, 10), seatStreamOf(90000, 10)}
	var mu sync.Mutex
	opened := 0
	defer func(f func(context.Context, Room, *http.Client, string, *slog.Logger) (seatStream, error)) {
		openSeatFunc = f
	}(openSeatFunc)
	openSeatFunc = func(context.Context, Room, *http.Client, string, *slog.Logger) (seatStream, error) {
		mu.Lock()
		defer mu.Unlock()
		if opened == len(streams) {
			return nil, ErrEnded
		}
		s := &fakeSeat{r: bytes.NewReader(streams[opened]), done: make(chan struct{})}
		opened++
		return s, nil
	}

	stream, err := Open(context.Background(), Room{Name: "x"}.URL(), nil, "ua", slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	done := make(chan []byte)
	go func() {
		b, _ := io.ReadAll(stream)
		done <- b
	}()
	var out []byte
	select {
	case out = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("recording did not end when the room did")
	}

	var video []int64
	seqHeaders := 0
	for _, tag := range readFLV(t, out) {
		if tag.seq {
			seqHeaders++
			continue
		}
		if tag.typ == flvVideo {
			video = append(video, tag.ts)
		}
	}
	if len(video) != 20 {
		t.Fatalf("got %d video frames, want 20", len(video))
	}
	for i, ts := range video {
		if want := int64(i) * 33; ts != want {
			t.Fatalf("frame %d at %d ms, want %d (timeline %v)", i, ts, want, video)
		}
	}
	// The second seat's configuration is the first's: no new headers.
	if seqHeaders != 2 {
		t.Fatalf("got %d sequence headers, want 2", seqHeaders)
	}
}
