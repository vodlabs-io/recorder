package provenance

import (
	"bytes"
	"image"
	"image/jpeg"
	"strings"
	"testing"

	"github.com/vodlabs-io/recorder/internal/version"
)

func TestRecordGrammar(t *testing.T) {
	old, oldC := version.Version, version.Commit
	version.Version, version.Commit = "0.9.4", "8362050bd1f2"
	defer func() { version.Version, version.Commit = old, oldC }()

	got := Record("flvwriter", "QtZte88eXN", "vod1.mp4")
	want := "VLWM1 prod=vodlabs-recorder ver=0.9.4 commit=8362050bd1f2 spec=1 " +
		"writer=flvwriter sid=QtZte88eXN part=vod1.mp4"
	if got != want {
		t.Fatalf("Record()\n got %q\nwant %q", got, want)
	}
}

func TestRecordOmitsEmptyFieldsAndSanitizes(t *testing.T) {
	old, oldC := version.Version, version.Commit
	version.Version, version.Commit = "devel", ""
	defer func() { version.Version, version.Commit = old, oldC }()

	// An unstamped build drops commit entirely rather than emitting an empty
	// value, and a value carrying a separator can never break the grammar.
	got := Record("hls writer", "a=b", "vod1.mp4")
	if strings.Contains(got, "commit=") {
		t.Errorf("empty commit should be omitted: %q", got)
	}
	for _, field := range strings.Split(got, " ")[1:] {
		if strings.Count(field, "=") != 1 {
			t.Errorf("field %q has more than one separator in %q", field, got)
		}
	}
	if !strings.Contains(got, "writer=hls_writer") || !strings.Contains(got, "sid=a_b") {
		t.Errorf("values not sanitized: %q", got)
	}
}

func TestMovFlagsFolds(t *testing.T) {
	// ffmpeg honours only the last -movflags, so the existing value must be
	// carried through rather than a second option appended.
	for _, tc := range []struct{ in, want string }{
		{"+faststart", "+faststart+use_metadata_tags"},
		{"", "+use_metadata_tags"},
		{"+faststart+use_metadata_tags", "+faststart+use_metadata_tags"},
	} {
		if got := MovFlags(tc.in); got != tc.want {
			t.Errorf("MovFlags(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestMetadataArgsPair(t *testing.T) {
	args := MetadataArgs("VLWM1 prod=vodlabs-recorder")
	if len(args)%2 != 0 {
		t.Fatalf("odd argument count: %v", args)
	}
	for i := 0; i < len(args); i += 2 {
		if args[i] != "-metadata" {
			t.Fatalf("arg %d = %q, want -metadata", i, args[i])
		}
	}
	if strings.Contains(strings.Join(args, " "), "encoder=") {
		t.Error("must not set encoder; ffmpeg overwrites it with its own identifier")
	}
}

func TestPlaylistCommentIsInert(t *testing.T) {
	line := PlaylistComment("VLWM1 prod=vodlabs-recorder")
	if !strings.HasPrefix(line, "#") {
		t.Fatalf("not a comment: %q", line)
	}
	if strings.HasPrefix(line, "#EXT") {
		t.Fatalf("would parse as a tag: %q", line)
	}
	if strings.ContainsAny(line, "\r\n") {
		t.Fatalf("must stay on one line: %q", line)
	}
}

// oneByOneJPEG is the smallest real JPEG the stdlib will produce.
func oneByOneJPEG(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	img := image.NewGray(image.Rect(0, 0, 1, 1))
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestStampJPEGRoundTrips(t *testing.T) {
	src := oneByOneJPEG(t)
	rec := "VLWM1 prod=vodlabs-recorder ver=0.9.4"

	out, err := StampJPEG(src, rec)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(out, []byte(rec)) {
		t.Error("record not present in stamped image")
	}
	if got := out[:2]; got[0] != 0xFF || got[1] != 0xD8 {
		t.Errorf("SOI clobbered: % x", got)
	}
	if _, err := jpeg.Decode(bytes.NewReader(out)); err != nil {
		t.Fatalf("stamped image no longer decodes: %v", err)
	}
	// The pixel data must be untouched: everything after the inserted segment
	// is the original file, byte for byte.
	if !bytes.Equal(out[2+4+len(rec):], src[2:]) {
		t.Error("bytes after the comment segment differ from the source")
	}
}

func TestStampJPEGRejectsNonJPEG(t *testing.T) {
	if _, err := StampJPEG([]byte("not a jpeg"), "x"); err != ErrNotJPEG {
		t.Fatalf("err = %v, want ErrNotJPEG", err)
	}
}
