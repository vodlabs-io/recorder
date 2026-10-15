package hlsingest

import (
	"testing"

	"github.com/vodlabs-io/recorder/internal/stream"
)

func TestRenameSegments(t *testing.T) {
	in := "#EXTM3U\n#EXT-X-MEDIA-SEQUENCE:1\n#EXTINF:4.0,\nkszt_a_snma350_origin/1019965.ts?bucket=br&tsc=origin\n" +
		"#EXTINF:4.0,\nkszt_a_snma350_origin/1019969.ts?bucket=br\n"
	want := "#EXTM3U\n#EXT-X-MEDIA-SEQUENCE:1\n#EXTINF:4.0,\nkszt_a_origin/1019965.ts?bucket=br&tsc=origin\n" +
		"#EXTINF:4.0,\nkszt_a_origin/1019969.ts?bucket=br\n"
	r := &stream.SegmentRename{From: "kszt_a_snma350_origin/", To: "kszt_a_origin/"}
	if got := string(renameSegments([]byte(in), r)); got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
	if got := string(renameSegments([]byte(in), nil)); got != in {
		t.Error("nil rename changed the playlist")
	}
	// Tags are left alone even when they carry From.
	tag := "#EXT-X-MAP:URI=\"kszt_a_snma350_origin/init.mp4\"\n"
	if got := string(renameSegments([]byte(tag), r)); got != tag {
		t.Errorf("tag rewritten: %q", got)
	}
}
