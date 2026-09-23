package processing

import (
	"strings"
	"testing"

	"github.com/vodlabs-io/recorder/internal/ffmpeg"
	"github.com/vodlabs-io/recorder/internal/recording"
)

// inputOpts is everything ffmpeg reads before the first -i; an option placed
// after it applies to the output and would silently do nothing here.
func inputOpts(t *testing.T, args []string) string {
	t.Helper()
	for i, a := range args {
		if a == "-i" {
			return strings.Join(args[:i], " ")
		}
	}
	t.Fatalf("no -i in args: %q", args)
	return ""
}

// A part whose video starts after the default analysis window leaves ffmpeg
// with no decoder config. On MPEG-TS (and the HLS playlists that resolve to
// it) there is no container-level config to fall back on, so a stream copy
// drops the video track, writes an audio-only file and still exits 0 —
// silently, and the raw inputs are deleted afterwards.
func TestTSPathsRaiseAnalyzeDuration(t *testing.T) {
	want := "-analyzeduration " + ffmpeg.AnalyzeDuration

	t.Run("flv convert", func(t *testing.T) {
		if got := inputOpts(t, flvConvertArgs("/vods/pt1.ts", "/vods/pt1.mp4", "")); !strings.Contains(got, want) {
			t.Errorf("input options = %q, want %q", got, want)
		}
	})

	for _, writer := range []string{"hlswriter", "flvwriter"} {
		t.Run("concat "+writer, func(t *testing.T) {
			j := &job{vod: recording.Output{WriterName: writer}}
			if got := inputOpts(t, j.concatArgs("/vods/concat_list.txt", "/vods/vod1.mp4")); !strings.Contains(got, want) {
				t.Errorf("input options = %q, want %q", got, want)
			}
		})
	}

	t.Run("thumbnail", func(t *testing.T) {
		if got := inputOpts(t, thumbnailArgs("30", "/vods/vod1.mp4", "/vods/vod1.jpg")); !strings.Contains(got, want) {
			t.Errorf("input options = %q, want %q", got, want)
		}
	})
}
