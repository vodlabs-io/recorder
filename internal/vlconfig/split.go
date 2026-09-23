package vlconfig

import (
	"fmt"
	"strings"
	"time"

	"github.com/dustin/go-humanize"
)

const (
	minSplitSize     int64         = 10 * 1000 * 1000 // 10 MB (SI)
	minSplitDuration time.Duration = time.Minute
)

// splitLimits holds the resolved, parsed split limits for a Session.
// Zero values mean "no limit".
type splitLimits struct {
	Size     int64
	Duration time.Duration
}

// Enabled reports whether at least one split limit is active.
func (l splitLimits) Enabled() bool {
	return l.Size > 0 || l.Duration > 0
}

// IsSplitOff reports whether raw is an explicit "disable" token ("off" or "0").
func IsSplitOff(raw string) bool {
	t := strings.TrimSpace(strings.ToLower(raw))
	return t == "off" || t == "0"
}

// ParseSplitSize parses a human-readable byte size string (e.g. "2GB", "500MB").
// Empty string or an off-token returns (0, nil). Values below minSplitSize
// return an error.
func ParseSplitSize(raw string) (int64, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || IsSplitOff(raw) {
		return 0, nil
	}
	n, err := humanize.ParseBytes(raw)
	if err != nil {
		return 0, fmt.Errorf("invalid split_size %q: %w", raw, err)
	}
	if int64(n) < minSplitSize {
		return 0, fmt.Errorf("split_size %q is below the minimum of 10MB", raw)
	}
	return int64(n), nil
}

// ParseSplitDuration parses a Go duration string (e.g. "1h30m", "45m").
// Empty string or an off-token returns (0, nil). Values below minSplitDuration
// return an error.
func ParseSplitDuration(raw string) (time.Duration, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || IsSplitOff(raw) {
		return 0, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("invalid split_duration %q: %w", raw, err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("split_duration %q must be positive", raw)
	}
	if d < minSplitDuration {
		return 0, fmt.Errorf("split_duration %q is below the minimum of 1m", raw)
	}
	return d, nil
}
