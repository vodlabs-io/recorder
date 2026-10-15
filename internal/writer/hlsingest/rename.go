package hlsingest

import (
	"bytes"
	"strings"

	"github.com/vodlabs-io/recorder/internal/stream"
)

// renameSegments applies r to every segment URI line of a media playlist. A
// nil r, or a playlist that never names r.From, is returned unchanged.
func renameSegments(body []byte, r *stream.SegmentRename) []byte {
	if r == nil || r.From == "" || !bytes.Contains(body, []byte(r.From)) {
		return body
	}
	lines := strings.Split(string(body), "\n")
	for i, line := range lines {
		if t := strings.TrimSpace(line); t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		lines[i] = r.Apply(line)
	}
	return []byte(strings.Join(lines, "\n"))
}
