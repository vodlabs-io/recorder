package processing

import (
	"strings"
	"testing"

	"github.com/vodlabs-io/recorder/internal/recording"
)

// countOpt returns how many times an ffmpeg option name appears, and the value
// of its last occurrence — which is the one ffmpeg honours.
func countOpt(args []string, name string) (int, string) {
	n, last := 0, ""
	for i, a := range args {
		if a == name && i+1 < len(args) {
			n++
			last = args[i+1]
		}
	}
	return n, last
}

func assertProvenance(t *testing.T, args []string, wantFaststart bool) {
	t.Helper()
	// ffmpeg keeps only the last -movflags, so a second occurrence would
	// silently drop the first one's flags.
	n, movflags := countOpt(args, "-movflags")
	if wantFaststart {
		if n != 1 {
			t.Errorf("-movflags appears %d times, want exactly 1: %v", n, args)
		}
		for _, want := range []string{"faststart", "use_metadata_tags"} {
			if !strings.Contains(movflags, want) {
				t.Errorf("-movflags %q missing %q", movflags, want)
			}
		}
	} else if n != 0 {
		t.Errorf("-movflags appears %d times, want none: %v", n, args)
	}

	joined := strings.Join(args, " ")
	for _, want := range []string{"comment=VLWM1", "vl_wm=VLWM1"} {
		if !strings.Contains(joined, want) {
			t.Errorf("args missing %q: %v", want, args)
		}
	}
	if strings.Contains(joined, "-metadata encoder=") {
		t.Error("must not set encoder; ffmpeg overwrites it with its own identifier")
	}
	// The output path stays last so callers can keep appending inputs safely.
	if args[len(args)-1] != "/out.mp4" {
		t.Errorf("output path is not last: %v", args)
	}
}

func TestFLVConvertArgsCarryProvenance(t *testing.T) {
	assertProvenance(t, flvConvertArgs("/in.ts", "/out.mp4", "VLWM1 prod=vodlabs-recorder"), true)
}

func TestConcatArgsCarryProvenance(t *testing.T) {
	for _, tc := range []struct {
		writer        string
		wantFaststart bool
	}{
		// The HLS branch passes no -movflags today; the standard "comment" key
		// needs none, so that branch must not grow one.
		{"flvwriter", true},
		{"hlswriter", false},
	} {
		t.Run(tc.writer, func(t *testing.T) {
			j := &job{vod: recording.Output{WriterName: tc.writer, SessionID: "QtZte88eXN"}}
			assertProvenance(t, j.concatArgs("/list.txt", "/out.mp4"), tc.wantFaststart)
		})
	}
}
