package fmp4

import (
	"bytes"
	"io"
	"testing"

	"github.com/vodlabs-io/recorder/internal/fmp4/fmp4test"
)

func TestSniffRemuxesFragmentedMP4(t *testing.T) {
	// The stream joins mid-GOP: frames before its first keyframe are dropped.
	var src bytes.Buffer
	src.Write(fmp4test.Init())
	src.Write(fmp4test.Frag(1, 4967, 33, 0, fmp4test.VideoAU(false, 9)))
	src.Write(fmp4test.Stream(5000, 5)[len(fmp4test.Init()):])

	out, err := io.ReadAll(Sniff(io.NopCloser(&src)))
	if err != nil {
		t.Fatal(err)
	}
	var video, audio []int64
	seq := 0
	for _, tag := range fmp4test.ReadFLV(t, out) {
		switch {
		case tag.Seq:
			seq++
		case tag.Type == fmp4test.FLVVideo:
			video = append(video, tag.TS)
		case tag.Type == fmp4test.FLVAudio:
			audio = append(audio, tag.TS)
		}
	}
	if seq != 2 {
		t.Fatalf("got %d sequence headers, want 2", seq)
	}
	if len(video) != 5 || video[0] != 0 || video[4] != 132 {
		t.Fatalf("video timeline %v, want 0..132 in 5 frames", video)
	}
	if len(audio) != 5 || audio[0] != 0 {
		t.Fatalf("audio timeline %v", audio)
	}
}

func TestSniffPassesFLVThrough(t *testing.T) {
	in := []byte("FLV\x01\x05\x00\x00\x00\x09\x00\x00\x00\x00rest")
	out, err := io.ReadAll(Sniff(io.NopCloser(bytes.NewReader(in))))
	if err != nil || !bytes.Equal(out, in) {
		t.Fatalf("got %q, %v", out, err)
	}
	short := []byte("FL")
	out, err = io.ReadAll(Sniff(io.NopCloser(bytes.NewReader(short))))
	if err != nil || !bytes.Equal(out, short) {
		t.Fatalf("short body: got %q, %v", out, err)
	}
}

func TestRemuxCarriesTheTimelineAcrossAClockRestart(t *testing.T) {
	// The second half restarts the stream's clock at 0, backwards; the third
	// jumps it far forward.
	var src bytes.Buffer
	src.Write(fmp4test.Stream(600000, 5))
	init := len(fmp4test.Init())
	src.Write(fmp4test.Stream(0, 5)[init:])
	src.Write(fmp4test.Stream(90000000, 5)[init:])

	out, err := io.ReadAll(Sniff(io.NopCloser(&src)))
	if err != nil {
		t.Fatal(err)
	}
	var video, audio []int64
	for _, tag := range fmp4test.ReadFLV(t, out) {
		switch {
		case tag.Seq:
		case tag.Type == fmp4test.FLVVideo:
			video = append(video, tag.TS)
		case tag.Type == fmp4test.FLVAudio:
			audio = append(audio, tag.TS)
		}
	}
	if len(video) != 15 {
		t.Fatalf("got %d video frames, want 15: %v", len(video), video)
	}
	for i, ts := range video {
		if want := int64(i) * 33; ts != want {
			t.Fatalf("frame %d at %d ms, want %d (timeline %v)", i, ts, want, video)
		}
	}
	if len(audio) != 15 || audio[14] != 14*33 {
		t.Fatalf("audio timeline %v", audio)
	}
}
