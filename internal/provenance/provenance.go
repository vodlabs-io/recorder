// Package provenance describes the software that produced a recording, in a
// form that survives into the artifacts themselves: container metadata tags,
// playlist comments, the session manifest, and JPEG comment segments.
//
// The record is diagnostic. It names the build and the session so a file found
// in the wild can be traced back to the code that wrote it, which is what makes
// a bug report from a stray VOD actionable. It carries no viewer identity, no
// platform, and no streamer name.
package provenance

import (
	"encoding/binary"
	"errors"
	"fmt"
	"strings"

	"github.com/vodlabs-io/recorder/internal/version"
)

const (
	// SpecVersion identifies the record layout. Bump it when the grammar or
	// the set of required keys changes, never for an added optional key.
	SpecVersion = 1

	// Producer names the software, and is the stable anchor a reader greps for.
	Producer = "vodlabs-recorder"

	// prefix opens every record: the magic plus the spec version, no space.
	prefix = "VLWM"
)

// Record renders the provenance record for one output file.
//
// Grammar: "VLWM<spec>" followed by space-separated key=value pairs. Readers
// must ignore keys they do not know and must not depend on field order.
// Values are printable ASCII with space and '=' excluded; anything else is
// replaced so a malformed value can never break the grammar.
func Record(writerName, sessionID, partName string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s%d", prefix, SpecVersion)
	for _, kv := range [][2]string{
		{"prod", Producer},
		{"ver", version.Version},
		{"commit", version.Commit},
		{"spec", fmt.Sprint(SpecVersion)},
		{"writer", writerName},
		{"sid", sessionID},
		{"part", partName},
	} {
		if kv[1] == "" {
			continue
		}
		fmt.Fprintf(&b, " %s=%s", kv[0], sanitizeValue(kv[1]))
	}
	return b.String()
}

// sanitizeValue coerces v into the value alphabet: printable ASCII minus space
// and '='. Out-of-range bytes become '_', so a value can never introduce a
// field separator or terminate the line.
func sanitizeValue(v string) string {
	out := []byte(v)
	for i, c := range out {
		if c < '!' || c > '~' || c == '=' {
			out[i] = '_'
		}
	}
	return string(out)
}

// MetadataArgs returns the -metadata pairs carrying rec for an ffmpeg mov
// output. "comment" is a standard mov key and is written without any muxer
// flag; "vl_wm" needs +use_metadata_tags (see MovFlags) and survives tools that
// rewrite the well-known tags.
//
// "encoder" is deliberately absent: ffmpeg overwrites it with its own libavformat
// identifier, which is itself worth keeping.
func MetadataArgs(rec string) []string {
	return []string{
		"-metadata", "comment=" + rec,
		"-metadata", "vl_wm=" + rec,
	}
}

// MovFlags folds the flags the provenance tags need into an existing -movflags
// value. ffmpeg keeps only the last -movflags occurrence, so callers must pass
// their current value through this rather than appending a second option.
func MovFlags(base string) string {
	const need = "use_metadata_tags"
	if strings.Contains(base, need) {
		return base
	}
	if base == "" {
		return "+" + need
	}
	return base + "+" + need
}

// PlaylistComment renders rec as an M3U8 comment line. Any line starting with
// '#' that is not a recognised tag is ignored by players, per RFC 8216.
func PlaylistComment(rec string) string {
	return "# " + rec
}

// ErrNotJPEG is returned by StampJPEG for input that does not open with SOI.
var ErrNotJPEG = errors.New("provenance: not a JPEG")

// StampJPEG inserts rec as a JPEG comment (COM) segment immediately after the
// SOI marker, returning the new image. The pixel data is untouched: this is a
// byte splice, not a re-encode.
func StampJPEG(data []byte, rec string) ([]byte, error) {
	if len(data) < 2 || data[0] != 0xFF || data[1] != 0xD8 {
		return nil, ErrNotJPEG
	}
	payload := []byte(rec)
	// The COM length field covers itself plus the payload, and is 16-bit.
	if len(payload)+2 > 0xFFFF {
		return nil, fmt.Errorf("provenance: record too long for a JPEG comment: %d bytes", len(payload))
	}
	seg := make([]byte, 4+len(payload))
	seg[0], seg[1] = 0xFF, 0xFE
	binary.BigEndian.PutUint16(seg[2:4], uint16(len(payload)+2))
	copy(seg[4:], payload)

	out := make([]byte, 0, len(data)+len(seg))
	out = append(out, data[:2]...)
	out = append(out, seg...)
	out = append(out, data[2:]...)
	return out, nil
}
