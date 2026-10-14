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
