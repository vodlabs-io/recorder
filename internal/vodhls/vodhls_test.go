package vodhls

import (
	"bytes"
	"context"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/Eyevinn/mp4ff/mp4"

	"github.com/vodlabs-io/recorder/internal/ffmpeg"
)

// fixture records a seconds-long H.264 + AAC MP4 with a keyframe every gop
// seconds. The embedded ffmpeg has no audio source or PCM input, so the
// audio is silent AAC frames written by hand.
func fixture(t *testing.T, seconds, gop int) string {
	t.Helper()
	dir := t.TempDir()
	out := filepath.Join(dir, "vod1.mp4")
	// ADTS header (AAC-LC, 48 kHz, mono, 13-byte frame) and a silent frame.
	frame := []byte{0xFF, 0xF1, 0x4C, 0x40, 0x01, 0xBF, 0xFC, 0x00, 0xc8, 0x00, 0x80, 0x23, 0x80}
	aac := bytes.Repeat(frame, seconds*48000/1024+1)
	audio := filepath.Join(dir, "silence.aac")
	if err := os.WriteFile(audio, aac, 0o644); err != nil {
		t.Fatal(err)
	}
	cmd, err := ffmpeg.Command(context.Background(), slog.Default(),
		"-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc=size=320x240:rate=30:duration="+strconv.Itoa(seconds),
		"-f", "aac", "-i", audio,
		"-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p", "-bf", "2",
		"-g", strconv.Itoa(gop*30), "-keyint_min", strconv.Itoa(gop*30), "-sc_threshold", "0",
		"-c:a", "copy", "-shortest", out)
	if err != nil {
		t.Skip(err)
	}
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("no usable ffmpeg: %v %s", err, b)
	}
	return out
}

func TestPlanCutsAtKeyframesAndKeepsEverySample(t *testing.T) {
	path := fixture(t, 20, 2)
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	p, err := NewPlan(f)
	if err != nil {
		t.Fatal(err)
	}
	// 2 s keyframes, 6 s target: 6+6+6+2.
	d := p.Durations()
	if len(d) != 4 || math.Abs(d[0]-6) > 0.01 || math.Abs(p.Duration()-20) > 0.1 {
		t.Fatalf("segments %v, duration %v", d, p.Duration())
	}

	var stream bytes.Buffer
	stream.Write(p.Init())
	for n := range d {
		if err := p.WriteSegment(&stream, f, n); err != nil {
			t.Fatal(err)
		}
	}
	out, err := mp4.DecodeFile(bytes.NewReader(stream.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	want := map[uint32]uint32{}
	for _, tr := range p.tracks {
		want[tr.id] = uint32(len(tr.durs))
	}
	got := map[uint32]uint32{}
	for i, seg := range out.Segments {
		for _, frag := range seg.Fragments {
			for _, traf := range frag.Moof.Trafs {
				if traf.Trun == nil {
					continue
				}
				got[traf.Tfhd.TrackID] += traf.Trun.SampleCount()
				if traf.Tfhd.TrackID == p.tracks[0].id && traf.Trun.Samples[0].Flags != mp4.SyncSampleFlags {
					t.Fatalf("segment %d starts on a non-keyframe", i)
				}
			}
		}
	}
	if len(want) != 2 || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("samples per track: got %v, want %v", got, want)
	}

	// The stream decodes start to end.
	joined := filepath.Join(t.TempDir(), "joined.mp4")
	if err := os.WriteFile(joined, stream.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd, err := ffmpeg.Command(context.Background(), slog.Default(), "-hide_banner", "-loglevel", "error", "-xerror", "-i", joined, "-c:v", "mjpeg", "-c:a", "aac", "-f", "null", "-")
	if err != nil {
		t.Skip(err)
	}
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("decode: %v %s", err, b)
	}
}

func TestPlanRejectsNonMP4(t *testing.T) {
	if _, err := NewPlan(bytes.NewReader([]byte("\x1aE\xdf\xa3 not an mp4"))); err == nil {
		t.Fatal("planned a non-MP4")
	}
}

func TestCacheEvictsByWeight(t *testing.T) {
	path := fixture(t, 4, 2)
	c := NewCache(1)
	for _, key := range []string{"a", "b"} {
		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.Plan(key, f); err != nil {
			t.Fatal(err)
		}
		f.Close()
	}
	if _, ok := c.plans["a"]; ok || len(c.plans) != 1 {
		t.Fatalf("cache holds %d plans", len(c.plans))
	}
}
