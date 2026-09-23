package processing

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/vodlabs-io/recorder/internal/ffmpeg"
	"github.com/vodlabs-io/recorder/internal/recording"
)

// TestGroupLostVideo pins the check that stands between a failed remux and a
// silently audio-only VOD. ffmpeg drops a stream whose codec it cannot
// determine, writes the rest and exits 0, so nothing else in the pipeline
// notices.
func TestGroupLostVideo(t *testing.T) {
	tests := []struct {
		name    string
		variant string
		probe   ffmpeg.ProbeResult
		want    bool
	}{
		{
			name:    "video and audio",
			variant: "best",
			probe:   ffmpeg.ProbeResult{VideoCodec: "h264", AudioCodec: "aac"},
			want:    false,
		},
		{
			name:    "video with no audio",
			variant: "best",
			probe:   ffmpeg.ProbeResult{VideoCodec: "h264"},
			want:    false,
		},
		{
			// The failure this exists to catch: the remux kept the audio and
			// dropped the video track.
			name:    "audio only",
			variant: "best",
			probe:   ffmpeg.ProbeResult{AudioCodec: "aac", Duration: 5296},
			want:    true,
		},
		{
			name:    "empty probe",
			variant: "best",
			probe:   ffmpeg.ProbeResult{},
			want:    true,
		},
		{
			// An unset variant is the default, which is "best" and so expects
			// video.
			name:    "audio only on an unset variant",
			variant: "",
			probe:   ffmpeg.ProbeResult{AudioCodec: "aac"},
			want:    true,
		},
		{
			// A session that asked for audio has no video track by design, and
			// failing it would break a supported configuration.
			name:    "audio only on an audio variant",
			variant: "audio",
			probe:   ffmpeg.ProbeResult{AudioCodec: "aac"},
			want:    false,
		},
		{
			name:    "audio only on the audio_only spelling",
			variant: "audio_only",
			probe:   ffmpeg.ProbeResult{AudioCodec: "aac"},
			want:    false,
		},
		{
			name:    "audio only on the ao spelling",
			variant: "AO",
			probe:   ffmpeg.ProbeResult{AudioCodec: "aac"},
			want:    false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := groupLostVideo(tc.variant, tc.probe); got != tc.want {
				t.Errorf("groupLostVideo(%q, %+v) = %v, want %v",
					tc.variant, tc.probe, got, tc.want)
			}
		})
	}
}

// TestVerifyGroupVideoUnprobableIsNotAFailure pins that a group is failed only
// on evidence. An ffprobe that cannot run — an older embedded build ships none —
// must not condemn a group that is probably fine.
func TestVerifyGroupVideoUnprobableIsNotAFailure(t *testing.T) {
	j := &job{vod: recording.Output{Variant: "best"}}
	missing := filepath.Join(t.TempDir(), "absent.mp4")
	if err := j.verifyGroupVideo(context.Background(), slog.New(slog.DiscardHandler), missing); err != nil {
		t.Errorf("verifyGroupVideo on an unprobable file = %v, want nil", err)
	}
}

// TestVerifyGroupVideoAcceptsARealVideoFile runs the check against media the
// bundled ffmpeg produces, so the probe wiring is exercised and not just the
// decision.
func TestVerifyGroupVideoAcceptsARealVideoFile(t *testing.T) {
	ffPath, err := ffmpeg.Path()
	if err != nil {
		t.Skipf("no bundled ffmpeg: %v", err)
	}
	if _, err := ffmpeg.ProbePath(); err != nil {
		t.Skipf("no bundled ffprobe: %v", err)
	}

	out := filepath.Join(t.TempDir(), "vod1.mp4")
	cmd := exec.CommandContext(t.Context(), ffPath,
		"-hide_banner", "-v", "error",
		"-f", "lavfi", "-i", "testsrc=size=320x240:rate=10",
		"-t", "1", "-c:v", "libx264", "-pix_fmt", "yuv420p", "-y", out)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("bundled ffmpeg cannot synthesise test media: %v: %s", err, b)
	}
	if _, err := os.Stat(out); err != nil {
		t.Skipf("no test media produced: %v", err)
	}

	j := &job{vod: recording.Output{Variant: "best"}}
	if err := j.verifyGroupVideo(context.Background(), slog.New(slog.DiscardHandler), out); err != nil {
		t.Errorf("verifyGroupVideo on a file with video = %v, want nil", err)
	}
}
