package session

import (
	"strconv"
	"strings"
	"time"

	"github.com/vodlabs-io/recorder/internal/vlconfig"
)

// renderOutputDir expands an output-path template into a directory path relative
// to the download folder. tmpl supports the placeholders documented in
// default_config.toml; unrecognized {tokens} are left verbatim. An empty tmpl
// falls back to vlconfig.DefaultOutputTemplate.
//
// All calendar components use t's local time (matching {datetime}'s offset);
// {timestamp} is the only UTC value (Unix epoch seconds).
func renderOutputDir(tmpl, platform, username, sessionID string, t time.Time) string {
	if tmpl == "" {
		tmpl = vlconfig.DefaultOutputTemplate
	}
	return strings.NewReplacer(
		"{platform}", platform,
		"{username}", username,
		"{session_id}", sessionID,
		"{datetime}", t.Format("2006-01-02_15-04-05-0700"),
		"{date}", t.Format("2006-01-02"),
		"{time}", t.Format("15-04-05"),
		"{year}", t.Format("2006"),
		"{month}", t.Format("01"),
		"{day}", t.Format("02"),
		"{hour}", t.Format("15"),
		"{minute}", t.Format("04"),
		"{second}", t.Format("05"),
		"{utc_offset}", t.Format("-0700"),
		"{timestamp}", strconv.FormatInt(t.UTC().Unix(), 10),
	).Replace(tmpl)
}
