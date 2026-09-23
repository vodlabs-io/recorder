package encoding

import (
	"strings"
	"testing"
)

func TestCopyConcatArgsCarryProvenance(t *testing.T) {
	args := copyConcatArgs("/list.txt", "/out.mp4", "VLWM1 prod=vodlabs-recorder")

	n, movflags := 0, ""
	for i, a := range args {
		if a == "-movflags" && i+1 < len(args) {
			n++
			movflags = args[i+1]
		}
	}
	// ffmpeg keeps only the last -movflags, so a second occurrence would
	// silently drop faststart.
	if n != 1 {
		t.Errorf("-movflags appears %d times, want exactly 1: %v", n, args)
	}
	for _, want := range []string{"faststart", "use_metadata_tags"} {
		if !strings.Contains(movflags, want) {
			t.Errorf("-movflags %q missing %q", movflags, want)
		}
	}
	joined := strings.Join(args, " ")
	for _, want := range []string{"comment=VLWM1", "vl_wm=VLWM1", "-progress pipe:1"} {
		if !strings.Contains(joined, want) {
			t.Errorf("args missing %q: %v", want, args)
		}
	}
	if args[len(args)-1] != "/out.mp4" {
		t.Errorf("output path is not last: %v", args)
	}
}
