package encoding

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vodlabs-io/recorder/internal/ffmpeg"
)

func TestConcatListContent(t *testing.T) {
	parts := []partProbe{
		{path: "/vods/session/vod1.mp4"},
		{path: "/vods/o'brien/vod2.mp4"}, // single quote must be escaped
	}
	got := concatListContent(parts)
	want := "file '/vods/session/vod1.mp4'\n" +
		`file '/vods/o'\''brien/vod2.mp4'` + "\n"
	if got != want {
		t.Errorf("concat list mismatch:\n got: %q\nwant: %q", got, want)
	}
}

func TestBuildReencodeFilterWithAudio(t *testing.T) {
	got := buildReencodeFilter(2, 1920, 1080, true, "")
	want := "[0:v]scale=1920:1080:force_original_aspect_ratio=decrease:out_color_matrix=bt709:out_range=tv:out_primaries=bt709:out_transfer=bt709,pad=1920:1080:(ow-iw)/2:(oh-ih)/2:color=black,setsar=1,format=yuv420p[v0];" +
		"[0:a]aresample=48000,aformat=sample_fmts=fltp:channel_layouts=stereo[a0];" +
		"[1:v]scale=1920:1080:force_original_aspect_ratio=decrease:out_color_matrix=bt709:out_range=tv:out_primaries=bt709:out_transfer=bt709,pad=1920:1080:(ow-iw)/2:(oh-ih)/2:color=black,setsar=1,format=yuv420p[v1];" +
		"[1:a]aresample=48000,aformat=sample_fmts=fltp:channel_layouts=stereo[a1];" +
		"[v0][a0][v1][a1]concat=n=2:v=1:a=1[outv][outa]"
	if got != want {
		t.Errorf("filter mismatch:\n got: %q\nwant: %q", got, want)
	}
}

func TestBuildReencodeFilterVideoOnly(t *testing.T) {
	got := buildReencodeFilter(2, 1280, 720, false, "")
	want := "[0:v]scale=1280:720:force_original_aspect_ratio=decrease:out_color_matrix=bt709:out_range=tv:out_primaries=bt709:out_transfer=bt709,pad=1280:720:(ow-iw)/2:(oh-ih)/2:color=black,setsar=1,format=yuv420p[v0];" +
		"[1:v]scale=1280:720:force_original_aspect_ratio=decrease:out_color_matrix=bt709:out_range=tv:out_primaries=bt709:out_transfer=bt709,pad=1280:720:(ow-iw)/2:(oh-ih)/2:color=black,setsar=1,format=yuv420p[v1];" +
		"[v0][v1]concat=n=2:v=1:a=0[outv]"
	if got != want {
		t.Errorf("filter mismatch:\n got: %q\nwant: %q", got, want)
	}
	// Video-only graph must not resample audio.
	if strings.Contains(got, "aresample") || strings.Contains(got, "[a0]") {
		t.Errorf("video-only filter should have no audio processing: %q", got)
	}
}

// uniformProbe is a part that would stream-copy cleanly; each test mutates
// one field of it.
func uniformProbe() ffmpeg.ProbeResult {
	return ffmpeg.ProbeResult{
		VideoCodec: "h264", Width: 720, Height: 1280,
		VideoProfile: "High", PixFmt: "yuv420p", VideoExtradataFingerprint: "MD5:aaa",
		AudioCodec: "aac", AudioProfile: "LC", AudioSampleRate: 48000,
		AudioChannels: 2, AudioExtradataFingerprint: "MD5:bbb",
	}
}

func TestCopySignatureMatchesIdenticalParts(t *testing.T) {
	if copySignature(uniformProbe()) != copySignature(uniformProbe()) {
		t.Error("identical probes must share a signature")
	}
}

// Everything the mp4 muxer takes from the first input only: a part differing
// in any of these decodes with the wrong config after a stream copy.
func TestCopySignatureSeparatesDecoderConfigs(t *testing.T) {
	base := copySignature(uniformProbe())
	cases := map[string]func(*ffmpeg.ProbeResult){
		"resolution":          func(r *ffmpeg.ProbeResult) { r.Height = 1920 },
		"video codec":         func(r *ffmpeg.ProbeResult) { r.VideoCodec = "hevc" },
		"video profile":       func(r *ffmpeg.ProbeResult) { r.VideoProfile = "Main" },
		"pixel format":        func(r *ffmpeg.ProbeResult) { r.PixFmt = "yuv420p10le" },
		"video extradata":     func(r *ffmpeg.ProbeResult) { r.VideoExtradataFingerprint = "MD5:ccc" },
		"audio codec":         func(r *ffmpeg.ProbeResult) { r.AudioCodec = "mp3" },
		"audio profile":       func(r *ffmpeg.ProbeResult) { r.AudioProfile = "HE-AACv2" },
		"audio sample rate":   func(r *ffmpeg.ProbeResult) { r.AudioSampleRate = 22050 },
		"audio channel count": func(r *ffmpeg.ProbeResult) { r.AudioChannels = 1 },
		"audio extradata":     func(r *ffmpeg.ProbeResult) { r.AudioExtradataFingerprint = "MD5:ddd" },
		"missing audio":       func(r *ffmpeg.ProbeResult) { *r = ffmpeg.ProbeResult{VideoCodec: "h264", Width: 720, Height: 1280} },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			res := uniformProbe()
			mutate(&res)
			if got := copySignature(res); got == base {
				t.Errorf("%s change must break the copy signature, got %q", name, got)
			}
		})
	}
}

// The output muxer is stated rather than inferred, so the copy path keeps
// working whatever the caller names the output file.
func TestCopyConcatArgsNameTheOutputMuxer(t *testing.T) {
	for _, tc := range []struct {
		out    string
		toWebM bool
		want   string
	}{
		{"/staging/vod.mp4", false, "-f mp4"},
		{"/staging/vod.webm", true, "-f webm"},
	} {
		args := copyConcatArgs("/staging/concat_list.txt", tc.out+mergingSuffix, "", tc.toWebM)
		joined := strings.Join(args, " ")
		if !strings.Contains(joined, tc.want) {
			t.Errorf("copy args must name the output format %q: %q", tc.want, joined)
		}
		if tc.toWebM && strings.Contains(joined, "-movflags") {
			t.Errorf("WebM output takes no -movflags: %q", joined)
		}
		if args[len(args)-1] != tc.out+mergingSuffix {
			t.Errorf("output must come last: %q", joined)
		}
	}
}

// VP8 parts of different frame sizes still copy together; any other codec's
// size change breaks the signature.
func TestCopySignatureIgnoresVP8FrameSize(t *testing.T) {
	a := ffmpeg.ProbeResult{VideoCodec: "vp8", Width: 640, Height: 360, PixFmt: "yuv420p",
		AudioCodec: "opus", AudioSampleRate: 48000, AudioChannels: 2}
	b := a
	b.Width, b.Height = 1280, 720
	if copySignature(a) != copySignature(b) {
		t.Errorf("VP8 size change broke the signature: %q vs %q", copySignature(a), copySignature(b))
	}
}

func TestMergeOutputName(t *testing.T) {
	for _, tc := range []struct {
		inputs []string
		want   string
	}{
		{[]string{"vod1.webm", "vod2.webm"}, "vod.webm"},
		{[]string{"vod1.mp4", "vod2.mp4"}, "vod.mp4"},
		{[]string{"vod1.webm", "vod2.mp4"}, "vod.mp4"},
	} {
		if got := mergeOutputName(tc.inputs); got != tc.want {
			t.Errorf("mergeOutputName(%v) = %q, want %q", tc.inputs, got, tc.want)
		}
	}
}

// A part that opens on audio only reveals its pixel format past ffmpeg's
// default analysis window, and a filter_complex input with no pixel format
// fails the whole graph — so every `-i` must raise -analyzeduration first.
func TestReencodeInputArgsRaiseAnalyzeDurationPerPart(t *testing.T) {
	parts := []partProbe{{path: "/vods/session/vod1.mp4"}, {path: "/vods/session/vod2.mp4"}}
	args := reencodeInputArgs(parts, "")

	var seen int
	for i, a := range args {
		if a != "-i" {
			continue
		}
		seen++
		before := strings.Join(args[:i], " ")
		if n := strings.Count(before, "-analyzeduration "+ffmpeg.AnalyzeDuration); n != seen {
			t.Errorf("input %d (%s): %d -analyzeduration before it, want %d",
				seen, args[i+1], n, seen)
		}
	}
	if seen != len(parts) {
		t.Fatalf("got %d inputs, want %d", seen, len(parts))
	}
	// The resolution-change guard must survive alongside it.
	if !strings.Contains(strings.Join(args, " "), "-reinit_filter 0") {
		t.Errorf("re-encode inputs must keep -reinit_filter 0: %q", args)
	}
}

// The concat demuxer opens each listed part itself, so the copy path needs
// the same widened analysis window.
func TestCopyConcatArgsRaiseAnalyzeDuration(t *testing.T) {
	args := copyConcatArgs("/staging/concat_list.txt", "/staging/vod.mp4", "", false)
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "-analyzeduration "+ffmpeg.AnalyzeDuration+" -f concat") {
		t.Errorf("copy args must raise -analyzeduration before the input: %q", joined)
	}
}

func TestReencodeFilterSkipsDitherWhenDurationUnknown(t *testing.T) {
	dir := t.TempDir()
	filter, path, err := writeDither(dir, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if filter != "" || path != "" {
		t.Errorf("unknown duration should write no dither, got filter %q path %q", filter, path)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("unknown duration left files behind: %v", entries)
	}
	graph := buildReencodeFilter(2, 1280, 720, true, filter)
	// The embedded ffmpeg fails to configure the graph when sendcmd has no
	// commands.
	if strings.Contains(graph, "sendcmd") || strings.Contains(graph, "eq=brightness") {
		t.Errorf("graph carries the dither: %s", graph)
	}
	if !strings.Contains(graph, "concat=n=2:v=1:a=1[outv][outa]") {
		t.Errorf("concat should feed [outv] directly: %s", graph)
	}

	filter, path, err = writeDither(dir, 0, 2)
	if err != nil {
		t.Fatal(err)
	}
	if path == "" {
		t.Fatal("known duration should write a dither script")
	}
	if graph := buildReencodeFilter(2, 1280, 720, true, filter); !strings.Contains(graph, "[cv]sendcmd=f=") {
		t.Errorf("graph lacks the dither: %s", graph)
	}
}

// Windows caps a command line at 32K characters, so a merge's command must not
// grow with its graph or with the parts' directory.
func TestReencodeArgsStayShortForManyParts(t *testing.T) {
	dir := filepath.Join("C:", "Users", "someone", "Downloads", "recordings", "tiktok", "streamer", "2026-10-31_06-01-53-0700")
	plan := &mergePlan{targetW: 720, targetH: 1280, allAudio: true}
	for i := 1; i <= 300; i++ {
		plan.parts = append(plan.parts, partProbe{path: filepath.Join(dir, fmt.Sprintf("vod%d.mp4", i))})
	}
	job := &Job{SessionID: "s1", Output: "vod.mp4"}
	joined := strings.Join(reencodeArgs(job, plan, nil, 0, "/staging/filter_complex.txt", partsDir(plan.parts), "/staging/vod.mp4"), " ")
	if strings.Contains(joined, "concat=") || strings.Contains(joined, dir) {
		t.Errorf("args carry the graph or absolute part paths: %s", joined)
	}
	if !strings.Contains(joined, "-/filter_complex /staging/filter_complex.txt") {
		t.Errorf("args must read the graph from its file: %s", joined)
	}
	if !strings.Contains(joined, "-i file:vod300.mp4") {
		t.Errorf("parts should be named relative to their directory: %s", joined)
	}
	if n := len(joined); n > 20000 {
		t.Errorf("300-part command line is %d characters", n)
	}
}

func TestPartsDir(t *testing.T) {
	a := filepath.Join("v", "a", "vod1.mp4")
	b := filepath.Join("v", "a", "vod2.mp4")
	c := filepath.Join("v", "b", "vod3.mp4")
	if got := partsDir([]partProbe{{path: a}, {path: b}}); got != filepath.Join("v", "a") {
		t.Errorf("shared dir = %q", got)
	}
	if got := partsDir([]partProbe{{path: a}, {path: c}}); got != "" {
		t.Errorf("mixed dirs = %q, want empty", got)
	}
	if got := reencodeInputArgs([]partProbe{{path: a}, {path: c}}, ""); got[len(got)-1] != c {
		t.Errorf("without a dir, inputs keep their paths: %v", got)
	}
}
