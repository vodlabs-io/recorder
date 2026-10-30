package preview

import (
	"bytes"
	"image"
	"image/jpeg"
	"testing"
	"time"
)

func tileJPEG(t *testing.T, w, h int) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, blackYCbCr(w, h), nil); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestSheetWriterResumeAndVTTRoundTrip(t *testing.T) {
	tile := Tile{144, 256}
	files := map[string][]byte{}
	write := func(name string, data []byte) error { files[name] = data; return nil }
	sw := NewSheetWriter(tile, 2, 2, "thumbs.", write)
	for i := range 5 {
		if err := sw.Add(time.Duration(i)*2*time.Second, tileJPEG(t, 144, 256)); err != nil {
			t.Fatal(err)
		}
	}
	if len(files) != 1 || sw.Pending() != 1 {
		t.Fatalf("after 5 tiles of 2x2: %d sheets, %d pending", len(files), sw.Pending())
	}
	// A reconnect ends the part: its partial sheet is written too.
	if err := sw.Flush(); err != nil {
		t.Fatal(err)
	}
	var vtt bytes.Buffer
	if err := WriteVTT(&vtt, sw.Cues()); err != nil {
		t.Fatal(err)
	}
	cues, err := ParseVTT(vtt.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if len(cues) != 5 || cues[3] != (Cue{Start: 6 * time.Second, End: 8 * time.Second, Image: "thumbs.000.jpg", X: 144, Y: 256, W: 144, H: 256}) {
		t.Fatalf("cues = %+v", cues)
	}
	cfg, _ := jpeg.DecodeConfig(bytes.NewReader(files["thumbs.001.jpg"]))
	if (image.Point{cfg.Width, cfg.Height}) != (image.Point{144, 256}) {
		t.Fatalf("partial sheet is %dx%d", cfg.Width, cfg.Height)
	}

	next := NewSheetWriter(tile, 2, 2, "thumbs.", write)
	next.Resume(cues)
	if err := next.Add(20*time.Second, tileJPEG(t, 144, 256)); err != nil {
		t.Fatal(err)
	}
	got, err := next.Close(22 * time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if last := got[len(got)-1]; last.Image != "thumbs.002.jpg" || last.Start != 20*time.Second || got[4].End != 20*time.Second {
		t.Fatalf("resumed cues = %+v", got)
	}
}
