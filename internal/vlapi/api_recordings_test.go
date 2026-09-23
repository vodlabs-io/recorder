package vlapi

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/vodlabs-io/recorder/internal/vodindex"
)

// session builds a listing entry carrying n files, which is what makes session
// size vary by two orders of magnitude.
func session(i, files int) vodindex.VOD {
	s := vodindex.VOD{
		Dir:       fmt.Sprintf("/Volumes/archive/recordings/tiktok/streamer%03d/2026-08-06_12-00-00", i),
		Root:      "/Volumes/archive/recordings",
		Platform:  "tiktok",
		Username:  fmt.Sprintf("streamer%03d", i),
		SessionID: fmt.Sprintf("sess%06d", i),
		State:     "complete",
	}
	for f := range files {
		s.Files = append(s.Files, vodindex.File{
			Name:     fmt.Sprintf("2026-08-06_12-00-00_part%03d.mp4", f),
			Bytes:    1 << 30,
			HasThumb: true,
		})
	}
	return s
}

func pageBytes(t *testing.T, sessions []vodindex.VOD) int {
	t.Helper()
	b, err := json.Marshal(sessions)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return len(b)
}

// The count limit alone cannot bound a reply: NATS refuses a payload over its
// max rather than truncating it. A page of large VODs has to be trimmed by size.
func TestFitSessionsTrimsOversizedPage(t *testing.T) {
	var sessions []vodindex.VOD
	for i := range vodListMaxLimit {
		sessions = append(sessions, session(i, 98))
	}
	if got := pageBytes(t, sessions); got <= vodListMaxBytes {
		t.Fatalf("test fixture is too small to exercise the cap: %d bytes", got)
	}

	fitted := fitSessions(sessions)
	if len(fitted) == 0 {
		t.Fatal("fitSessions returned no sessions; an empty page reads as end-of-archive")
	}
	if len(fitted) >= len(sessions) {
		t.Errorf("fitSessions kept %d of %d sessions, want it to trim", len(fitted), len(sessions))
	}
	if got := pageBytes(t, fitted); got > vodListMaxBytes {
		t.Errorf("fitted page = %d bytes, want <= %d", got, vodListMaxBytes)
	}
}

// A page that already fits must pass through untouched, or every archive pays
// extra round trips.
func TestFitSessionsKeepsPageThatFits(t *testing.T) {
	var sessions []vodindex.VOD
	for i := range 100 {
		sessions = append(sessions, session(i, 3))
	}
	if got := pageBytes(t, sessions); got > vodListMaxBytes {
		t.Fatalf("fixture unexpectedly exceeds the cap: %d bytes", got)
	}
	if got := len(fitSessions(sessions)); got != len(sessions) {
		t.Errorf("fitSessions kept %d of %d sessions, want all of them", got, len(sessions))
	}
}

// One VOD larger than the whole budget still has to be returned: an empty page
// reads as the end of the VOD Archive to the client's paging loop.
func TestFitSessionsAlwaysKeepsOneSession(t *testing.T) {
	huge := session(0, 200000)
	if got := pageBytes(t, []vodindex.VOD{huge}); got <= vodListMaxBytes {
		t.Fatalf("fixture is not oversized: %d bytes", got)
	}
	if got := len(fitSessions([]vodindex.VOD{huge, session(1, 1)})); got != 1 {
		t.Errorf("fitSessions kept %d sessions, want exactly 1", got)
	}
}

// The whole point of the byte cap: whatever the count limit allows, the
// serialized page must stay deliverable over a 1 MiB NATS payload.
func TestFitSessionsStaysUnderNATSPayloadLimit(t *testing.T) {
	const natsMaxPayload = 1 << 20
	for _, files := range []int{1, 3, 20, 98} {
		var sessions []vodindex.VOD
		for i := range vodListMaxLimit {
			sessions = append(sessions, session(i, files))
		}
		if got := pageBytes(t, fitSessions(sessions)); got >= natsMaxPayload {
			t.Errorf("page of %d-file sessions = %d bytes, want well under %d",
				files, got, natsMaxPayload)
		}
	}
}
