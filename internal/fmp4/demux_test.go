package fmp4

import (
	"bytes"
	"io"
	"testing"

	"github.com/vodlabs-io/recorder/internal/fmp4/fmp4test"
)

func TestDemuxer(t *testing.T) {
	var s bytes.Buffer
	s.Write(fmp4test.Init())
	s.Write(fmp4test.Frag(1, 1000, 33, 66, fmp4test.VideoAU(true, 1)))
	s.Write(fmp4test.Frag(2, 48000, 1024, 0, []byte{0xaa, 0xbb}))
	s.Write(fmp4test.Frag(1, 1033, 33, 0, fmp4test.VideoAU(false, 2)))
	d := NewDemuxer(&s)

	want := []Sample{
		{Video: true, DTS: 1000, CTS: 66, Key: true, Data: fmp4test.VideoAU(true, 1)},
		{Video: false, DTS: 1000, Key: true, Data: []byte{0xaa, 0xbb}},
		{Video: true, DTS: 1033, Key: false, Data: fmp4test.VideoAU(false, 2)},
	}
	for i, w := range want {
		got, err := d.Next()
		if err != nil {
			t.Fatalf("sample %d: %v", i, err)
		}
		if got.Video != w.Video || got.DTS != w.DTS || got.CTS != w.CTS || got.Key != w.Key || !bytes.Equal(got.Data, w.Data) {
			t.Fatalf("sample %d = %+v, want %+v", i, got, w)
		}
		if !bytes.Equal(got.AVCC, fmp4test.AVCC) || !bytes.Equal(got.ASC, fmp4test.ASC) {
			t.Fatalf("sample %d config = %x / %x", i, got.AVCC, got.ASC)
		}
	}
	if _, err := d.Next(); err != io.EOF {
		t.Fatalf("end = %v, want io.EOF", err)
	}
}
