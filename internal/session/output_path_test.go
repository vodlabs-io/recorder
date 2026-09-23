package session

import (
	"testing"
	"time"

	"github.com/vodlabs-io/recorder/internal/vlconfig"
)

func TestRenderOutputDir(t *testing.T) {
	// Fixed instant: 2026-06-02 09:30:05 in a -0700 zone (== 16:30:05 UTC,
	// which is Unix epoch 1780417805). Using a non-UTC zone verifies that
	// calendar components use local time while {timestamp} is UTC.
	ts := time.Date(2026, 6, 2, 9, 30, 5, 0, time.FixedZone("-0700", -7*60*60))

	const (
		platform  = "tiktok"
		username  = "alice"
		sessionID = "V1StGXR8Z5"
	)

	tests := []struct {
		name string
		tmpl string
		want string
	}{
		{
			name: "empty falls back to default",
			tmpl: "",
			want: "tiktok/alice/2026-06-02_09-30-05-0700",
		},
		{
			name: "default template",
			tmpl: vlconfig.DefaultOutputTemplate,
			want: "tiktok/alice/2026-06-02_09-30-05-0700",
		},
		{
			name: "every placeholder",
			tmpl: "{platform}/{username}/{session_id}/{datetime}/{date}/{time}/" +
				"{year}/{month}/{day}/{hour}/{minute}/{second}/{utc_offset}/{timestamp}",
			want: "tiktok/alice/V1StGXR8Z5/2026-06-02_09-30-05-0700/2026-06-02/09-30-05/" +
				"2026/06/02/09/30/05/-0700/1780417805",
		},
		{
			name: "custom rearranged",
			tmpl: "{year}/{month}/{day}/{platform}-{username}",
			want: "2026/06/02/tiktok-alice",
		},
		{
			name: "unknown placeholder left literal",
			tmpl: "{platform}/{usrname}/{datetime}",
			want: "tiktok/{usrname}/2026-06-02_09-30-05-0700",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := renderOutputDir(tt.tmpl, platform, username, sessionID, ts); got != tt.want {
				t.Errorf("renderOutputDir() = %q, want %q", got, tt.want)
			}
		})
	}
}
