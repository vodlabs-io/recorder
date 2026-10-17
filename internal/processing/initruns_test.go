package processing

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vodlabs-io/recorder/internal/ffmpeg"
	"github.com/vodlabs-io/recorder/internal/recording"
)

// writePart writes an HLS part directory in the hls writer's layout: init
// segments and fragments named by the given playlist body lines.
func writePart(t *testing.T, dir string, files map[string][]byte, lines ...string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), body, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	pl := "#EXTM3U\n#EXT-X-VERSION:6\n#EXT-X-TARGETDURATION:2\n#EXT-X-MEDIA-SEQUENCE:0\n#EXT-X-PLAYLIST-TYPE:VOD\n" +
		strings.Join(lines, "\n") + "\n#EXT-X-ENDLIST\n"
	path := filepath.Join(dir, "index.m3u8")
	if err := os.WriteFile(path, []byte(pl), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestInitRunsSplitAtANewInit(t *testing.T) {
	dir := t.TempDir()
	playlist := writePart(t, dir, map[string][]byte{
		"init_00.mp4": []byte("init-a"),
		"init_01.mp4": []byte("init-b"),
	},
		`#EXT-X-MAP:URI="init_00.mp4"`, "#EXTINF:2.000,", "000000.m4s", "#EXTINF:2.000,", "000001.m4s",
		`#EXT-X-MAP:URI="init_01.mp4"`, "#EXTINF:2.000,", "000002.m4s",
	)

	runs, err := initRuns(playlist)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 2 || !mixedInits(runs) {
		t.Fatalf("got %d runs (mixed inits %v), want 2 mixed: %v", len(runs), mixedInits(runs), runs)
	}
	for i, want := range []struct{ init, segs string }{
		{"init_00.mp4", "000000.m4s,000001.m4s"},
		{"init_01.mp4", "000002.m4s"},
	} {
		body, err := os.ReadFile(runs[i].playlist)
		if err != nil {
			t.Fatal(err)
		}
		s := string(body)
		if strings.Count(s, "#EXT-X-MAP") != 1 || !strings.Contains(s, want.init) {
			t.Errorf("run %d: want exactly one map for %s:\n%s", i, want.init, s)
		}
		for _, seg := range strings.Split(want.segs, ",") {
			if !strings.Contains(s, seg) {
				t.Errorf("run %d: missing %s:\n%s", i, seg, s)
			}
		}
		if !strings.HasPrefix(s, "#EXTM3U\n") || !strings.HasSuffix(s, "#EXT-X-ENDLIST\n") {
			t.Errorf("run %d: not a complete VOD playlist:\n%s", i, s)
		}
	}
	if got := playlistDuration(playlist); got != 6 {
		t.Errorf("playlistDuration = %v, want 6", got)
	}
}

func TestInitRunsKeepARepeatedIdenticalInitInOneRun(t *testing.T) {
	dir := t.TempDir()
	playlist := writePart(t, dir, map[string][]byte{
		"init_00.mp4": []byte("same"),
		"init_01.mp4": []byte("same"),
	},
		`#EXT-X-MAP:URI="init_00.mp4"`, "#EXTINF:2.000,", "000000.m4s",
		`#EXT-X-MAP:URI="init_01.mp4"`, "#EXTINF:2.000,", "000001.m4s",
	)
	runs, err := initRuns(playlist)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].playlist != playlist {
		t.Fatalf("runs = %v, want the playlist itself", runs)
	}
}

func TestGroupTooShort(t *testing.T) {
	tests := []struct {
		name            string
		inBytes, out    int64
		wantSec, gotSec float64
		want            bool
	}{
		// The TwitCasting failure: 40 KB and 6 s kept of 55 MB and 197 s.
		{"stopped at an init rotation", 55651871, 40960, 197, 6, true},
		{"bytes alone, no probe", 55651871, 40960, 0, 0, true},
		{"container overhead only", 10_000_000, 9_200_000, 300, 299.5, false},
		{"duration short, bytes fine", 10_000_000, 9_000_000, 300, 120, true},
		{"a few seconds short of a short clip", 1_000_000, 900_000, 12, 8, false},
		{"nothing known", 0, 0, 0, 0, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := groupTooShort(tc.inBytes, tc.out, tc.wantSec, tc.gotSec); got != tc.want {
				t.Errorf("groupTooShort(%d, %d, %v, %v) = %v, want %v",
					tc.inBytes, tc.out, tc.wantSec, tc.gotSec, got, tc.want)
			}
		})
	}
}

// silentADTS writes n frames of silent AAC-LC (44.1 kHz stereo) as ADTS: the
// bundled ffmpeg has no audio source to synthesise any.
func silentADTS(t *testing.T, n int) string {
	t.Helper()
	payload := []byte{0x21, 0x00, 0x49, 0x90, 0x02, 0x19, 0x00, 0x23, 0x80}
	l := 7 + len(payload)
	frame := append([]byte{0xFF, 0xF1, 0x50, 0x80 | byte(l>>11), byte(l >> 3), byte(l&7)<<5 | 0x1F, 0xFC}, payload...)
	path := filepath.Join(t.TempDir(), "silence.aac")
	if err := os.WriteFile(path, bytes.Repeat(frame, n), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// fragmentedMP4 encodes 6 s of test pattern and silence as fragmented MP4 with
// the bundled ffmpeg, and splits it into its init segment (ftyp+moov) and
// fragments (moof+mdat), as an fMP4 HLS origin serves them. extra args shape
// the init (track order, timescale, profile).
func fragmentedMP4(t *testing.T, ffPath, size string, extra ...string) (init []byte, frags [][]byte) {
	t.Helper()
	out := filepath.Join(t.TempDir(), "frag.mp4")
	args := []string{"-hide_banner", "-v", "error",
		"-f", "lavfi", "-i", "testsrc=size=" + size + ":rate=10",
		"-f", "aac", "-i", silentADTS(t, 300), "-t", "6",
		"-c:v", "libx264", "-g", "20", "-pix_fmt", "yuv420p",
		"-c:a", "copy", "-bsf:a", "aac_adtstoasc",
		"-movflags", "frag_keyframe+empty_moov+default_base_moof"}
	args = append(append(args, extra...), "-y", out)
	if b, err := exec.CommandContext(t.Context(), ffPath, args...).CombinedOutput(); err != nil {
		t.Skipf("bundled ffmpeg cannot synthesise test media: %v: %s", err, b)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	for len(data) >= 8 {
		n := int(binary.BigEndian.Uint32(data))
		typ := string(data[4:8])
		if n < 8 || n > len(data) {
			t.Fatalf("bad box %q size %d", typ, n)
		}
		box := data[:n]
		switch typ {
		case "ftyp", "moov":
			init = append(init, box...)
		case "moof":
			frags = append(frags, append([]byte(nil), box...))
		case "mdat":
			frags[len(frags)-1] = append(frags[len(frags)-1], box...)
		}
		data = data[n:]
	}
	if len(init) == 0 || len(frags) < 2 {
		t.Fatalf("unexpected fMP4 layout: init %d bytes, %d fragments", len(init), len(frags))
	}
	return init, frags
}

// TestConcatGroupJoinsAcrossAnInitRotation merges a part recorded across an
// encoder switch the way TwitCasting serves one: the second init lists audio
// before video under different track IDs, on a millisecond timescale (here also
// at a new resolution and profile), and around the switch the playlist names
// the old init above a fragment of the new layout. Both runs must reach the
// output and decode.
func TestConcatGroupJoinsAcrossAnInitRotation(t *testing.T) {
	ffPath, err := ffmpeg.Path()
	if err != nil {
		t.Skipf("no bundled ffmpeg: %v", err)
	}
	probePath, err := ffmpeg.ProbePath()
	if err != nil {
		t.Skipf("no bundled ffprobe: %v", err)
	}

	initA, fragsA := fragmentedMP4(t, ffPath, "320x240", "-map", "0:v", "-map", "1:a", "-profile:v", "high")
	initB, fragsB := fragmentedMP4(t, ffPath, "640x360", "-map", "1:a", "-map", "0:v", "-profile:v", "baseline",
		"-video_track_timescale", "1000",
		"-streamid", "0:1", "-streamid", "1:256", "-use_stream_ids_as_track_ids", "1")
	if len(fragsB) < 3 {
		t.Fatalf("want at least 3 fragments of the second layout, got %d", len(fragsB))
	}

	base := t.TempDir()
	files := map[string][]byte{}
	var lines []string
	seq := 0
	addRun := func(initName string, init []byte, frags [][]byte) {
		files[initName] = init
		lines = append(lines, `#EXT-X-MAP:URI="`+initName+`"`)
		for _, f := range frags {
			name := fmt.Sprintf("%06d.m4s", seq)
			seq++
			files[name] = f
			lines = append(lines, "#EXTINF:2.000,", name)
		}
	}
	addRun("init_00.mp4", initA, fragsA)
	addRun("init_01.mp4", initB, fragsB[:1])
	addRun("init_02.mp4", initA, fragsB[1:2]) // the old init named above a new fragment
	addRun("init_03.mp4", initB, fragsB[2:])
	partDir := filepath.Join(base, "pt1_high")
	writePart(t, partDir, files, lines...)

	j := &job{groupInputs: []string{"pt1_high"}, groupOutput: "vod.mp4"}
	j.vod.WriterName = "hlswriter"
	j.vod.BaseOutDir = base
	j.vod.Variant = "best"
	out := filepath.Join(base, "vod.mp4")
	if err := j.concatGroup(context.Background(), discardLogger(), j.groupInputPaths(), out); err != nil {
		t.Fatalf("concatGroup: %v", err)
	}

	probe, err := ffmpeg.Probe(context.Background(), discardLogger(), out)
	if err != nil {
		t.Fatal(err)
	}
	if probe.Duration < 11 || probe.VideoCodec != "h264" || probe.AudioCodec != "aac" {
		t.Errorf("output %.1fs video %q audio %q, want ~12s of h264 and aac",
			probe.Duration, probe.VideoCodec, probe.AudioCodec)
	}

	// Decode every frame: each run must come out at its own resolution.
	b, err := exec.CommandContext(t.Context(), probePath, "-v", "error",
		"-select_streams", "v", "-show_entries", "frame=width,height", "-of", "csv=p=0", out).CombinedOutput()
	if err != nil {
		t.Fatalf("ffprobe frames: %v: %s", err, b)
	}
	counts := map[string]int{}
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		counts[strings.TrimRight(l, ",")]++
	}
	if counts["320,240"] != 60 || counts["640,360"] != 60 || len(counts) != 2 {
		t.Errorf("decoded frames by size = %v, want 60 at 320,240 and 60 at 640,360", counts)
	}

	left, _ := filepath.Glob(filepath.Join(partDir, "*.run*"))
	if len(left) != 0 {
		t.Errorf("intermediates left behind: %v", left)
	}
}

// TestVerifyGroupLengthRejectsAShortOutput pins that a merge which kept a
// sliver of its part fails, removes the sliver and leaves the part alone.
func TestVerifyGroupLengthRejectsAShortOutput(t *testing.T) {
	base := t.TempDir()
	partDir := filepath.Join(base, "pt1_high")
	writePart(t, partDir, map[string][]byte{
		"init_00.mp4": []byte("init"),
		"000000.m4s":  make([]byte, 1<<20),
	}, `#EXT-X-MAP:URI="init_00.mp4"`, "#EXTINF:2.000,", "000000.m4s")
	out := filepath.Join(base, "vod.mp4")
	if err := os.WriteFile(out, make([]byte, 4096), 0o644); err != nil {
		t.Fatal(err)
	}

	j := &job{groupOutput: "vod.mp4"}
	j.vod.BaseOutDir = base
	err := j.verifyGroupLength(context.Background(), discardLogger(), pathSize(partDir), 0, out)
	if err == nil {
		t.Fatal("verifyGroupLength accepted a 4 KB output of a 1 MB part")
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Errorf("short output still on disk (stat err %v)", err)
	}
	if _, err := os.Stat(filepath.Join(partDir, "000000.m4s")); err != nil {
		t.Errorf("part segment gone: %v", err)
	}
}

// TestFailedGroupsErrFailsTheSession pins the finalize check: a session with a
// failed group finishes failed, and the daemon deletes raw parts only for a
// finished session.
func TestFailedGroupsErrFailsTheSession(t *testing.T) {
	s := newTestService(t)
	ctx := context.Background()
	out := recording.Output{BaseOutDir: t.TempDir(), WriterName: "hlswriter",
		Platform: "twitcasting", Username: "alice", SessionID: "sess"}
	for _, name := range []string{"vod1.mp4", "vod2.mp4"} {
		if err := s.enqueueJob(ctx, recording.Job{Output: out, Kind: recording.JobGroup,
			GroupInputs: []string{"pt1_high"}, GroupOutput: name}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.failedGroupsErr(ctx, "sess"); err != nil {
		t.Fatalf("no group failed yet, got %v", err)
	}

	if _, err := s.db.ExecContext(ctx,
		`UPDATE postprocess_jobs SET status = 'failed' WHERE group_output = 'vod2.mp4'`); err != nil {
		t.Fatal(err)
	}
	err := s.failedGroupsErr(ctx, "sess")
	if err == nil || !strings.Contains(err.Error(), "vod2.mp4") {
		t.Fatalf("failedGroupsErr = %v, want an error naming vod2.mp4", err)
	}
	if err := s.failedGroupsErr(ctx, "other"); err != nil {
		t.Errorf("another session's failure leaked: %v", err)
	}
}
