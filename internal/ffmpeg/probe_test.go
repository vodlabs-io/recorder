package ffmpeg

import (
	"strings"
	"testing"
)

// Trimmed to the fields parseProbe reads, from a real recorder part.
const sampleProbeJSON = `{
  "streams": [
    {"codec_type":"video","codec_name":"h264","profile":"High","width":540,"height":960,
     "pix_fmt":"yuv420p","avg_frame_rate":"30000/1001","r_frame_rate":"2000/1","bit_rate":"1200000",
     "extradata_hash":"MD5:0e5a37cf42810a345caa740018408414"},
    {"codec_type":"audio","codec_name":"aac","profile":"HE-AACv2","sample_rate":"44100",
     "channels":2,"bit_rate":"64000",
     "extradata_hash":"MD5:6df7d4c45943794720a7ac8a2dbd2378"}
  ],
  "format": {"duration":"1234.5","bit_rate":"1300000"}
}`

func TestParseProbe(t *testing.T) {
	res, err := parseProbe([]byte(sampleProbeJSON), "vod1.mp4")
	if err != nil {
		t.Fatalf("parseProbe: %v", err)
	}
	if res.VideoCodec != "h264" || res.Width != 540 || res.Height != 960 {
		t.Errorf("video basics: %+v", res)
	}
	if res.VideoProfile != "High" || res.PixFmt != "yuv420p" {
		t.Errorf("video config: profile=%q pix_fmt=%q", res.VideoProfile, res.PixFmt)
	}
	if res.VideoFrameRate < 29.97 || res.VideoFrameRate > 29.98 {
		t.Errorf("video frame rate: %v", res.VideoFrameRate)
	}
	if res.VideoExtradataFingerprint != "MD5:0e5a37cf42810a345caa740018408414" {
		t.Errorf("video extradata hash: %q", res.VideoExtradataFingerprint)
	}
	if res.AudioCodec != "aac" || res.AudioProfile != "HE-AACv2" {
		t.Errorf("audio codec: %q profile=%q", res.AudioCodec, res.AudioProfile)
	}
	if res.AudioSampleRate != 44100 || res.AudioChannels != 2 {
		t.Errorf("audio layout: %dHz %dch", res.AudioSampleRate, res.AudioChannels)
	}
	if res.AudioExtradataFingerprint != "MD5:6df7d4c45943794720a7ac8a2dbd2378" {
		t.Errorf("audio extradata hash: %q", res.AudioExtradataFingerprint)
	}
	if res.Duration != 1234.5 || res.VideoBitRate != 1200000 || res.AudioBitRate != 64000 {
		t.Errorf("rates/duration: %+v", res)
	}
}

// ffprobe names a profile when it can and emits a bare number when it can't.
func TestParseProbeNumericProfile(t *testing.T) {
	const j = `{"streams":[{"codec_type":"video","codec_name":"h264","profile":578,
	  "width":1280,"height":720}],"format":{}}`
	res, err := parseProbe([]byte(j), "vod1.mp4")
	if err != nil {
		t.Fatalf("parseProbe: %v", err)
	}
	if res.VideoProfile != "578" {
		t.Errorf("numeric profile: %q", res.VideoProfile)
	}
}

func TestParseProbeNoAudio(t *testing.T) {
	const j = `{"streams":[{"codec_type":"video","codec_name":"h264","width":1280,"height":720}],
	  "format":{"duration":"10.0"}}`
	res, err := parseProbe([]byte(j), "vod1.mp4")
	if err != nil {
		t.Fatalf("parseProbe: %v", err)
	}
	if res.AudioCodec != "" || res.AudioSampleRate != 0 || res.AudioChannels != 0 {
		t.Errorf("expected no audio, got %+v", res)
	}
}

// A stream with no average rate (0/0) falls back to its nominal one.
func TestParseProbeFrameRateFallback(t *testing.T) {
	const j = `{"streams":[{"codec_type":"video","codec_name":"h264","width":1280,"height":720,
	  "avg_frame_rate":"0/0","r_frame_rate":"25/1"}],"format":{"duration":"10.0"}}`
	res, err := parseProbe([]byte(j), "vod1.mp4")
	if err != nil {
		t.Fatalf("parseProbe: %v", err)
	}
	if res.VideoFrameRate != 25 {
		t.Errorf("frame rate = %v, want 25", res.VideoFrameRate)
	}
}

func TestParseProbeNoVideoIsAnError(t *testing.T) {
	const j = `{"streams":[{"codec_type":"audio","codec_name":"aac"}],"format":{}}`
	if _, err := parseProbe([]byte(j), "vod1.mp4"); err == nil {
		t.Fatal("expected an error for a file with no video stream")
	}
}

// A part can open on several seconds of audio; the default analysis window
// ends before the first video packet, leaving pix_fmt unknown. probesize is
// deliberately left alone — a merge opens every part at once, so a bigger
// probe buffer multiplies by the part count.
func TestProbeArgsRaiseAnalyzeDurationOnly(t *testing.T) {
	args := probeArgs("/vods/session/vod1.mp4")
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "-analyzeduration "+AnalyzeDuration) {
		t.Errorf("probe must raise -analyzeduration: %q", joined)
	}
	if strings.Contains(joined, "-probesize") {
		t.Errorf("probe must not raise -probesize: %q", joined)
	}
	if args[len(args)-1] != "/vods/session/vod1.mp4" {
		t.Errorf("path must come last: %q", joined)
	}
}
