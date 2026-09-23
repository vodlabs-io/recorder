package hlsingest

import (
	"fmt"
	"regexp"
	"strings"
)

var dateAttrPattern = regexp.MustCompile(`([A-Z0-9\-]+(?:DATE|TIME))="([^"]+)"`)

// sanitizePlaylistTimestamps gives a timezone designator to date-time values
// that carry none, before the playlist is parsed. Some Origins emit a local
// time with no offset, which the parser rejects — taking the whole playlist
// with it — where reading it as UTC costs nothing.
func sanitizePlaylistTimestamps(b []byte) []byte {
	if len(b) == 0 {
		return b
	}
	lines := strings.Split(string(b), "\n")
	if len(lines) == 0 {
		return b
	}
	for i, line := range lines {
		hasCR := strings.HasSuffix(line, "\r")
		trimmed := strings.TrimSuffix(line, "\r")
		sanitized := trimmed
		if strings.HasPrefix(trimmed, "#EXT-X-PROGRAM-DATE-TIME:") {
			sanitized = sanitizeProgramDateTimeLine(trimmed)
		} else if strings.HasPrefix(trimmed, "#EXT-X-DATERANGE:") {
			sanitized = sanitizeDateRangeLine(trimmed)
		}
		if hasCR {
			sanitized += "\r"
		}
		lines[i] = sanitized
	}
	return []byte(strings.Join(lines, "\n"))
}

func sanitizeProgramDateTimeLine(line string) string {
	const prefix = "#EXT-X-PROGRAM-DATE-TIME:"
	value := strings.TrimSpace(line[len(prefix):])
	if normalized, changed := ensureTimezone(value); changed {
		return prefix + normalized
	}
	return line
}

func sanitizeDateRangeLine(line string) string {
	return dateAttrPattern.ReplaceAllStringFunc(line, func(match string) string {
		subs := dateAttrPattern.FindStringSubmatch(match)
		if len(subs) != 3 {
			return match
		}
		key := subs[1]
		value := subs[2]
		if normalized, changed := ensureTimezone(value); changed {
			return fmt.Sprintf(`%s="%s"`, key, normalized)
		}
		return match
	})
}

func ensureTimezone(value string) (string, bool) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return value, false
	}
	if hasTimezoneDesignator(trimmed) {
		return trimmed, false
	}
	return trimmed + "Z", true
}

func hasTimezoneDesignator(value string) bool {
	if strings.ContainsAny(value, "Zz") {
		return true
	}
	if idx := strings.IndexByte(value, 'T'); idx != -1 {
		tail := value[idx+1:]
		return strings.ContainsAny(tail, "+-")
	}
	return false
}
