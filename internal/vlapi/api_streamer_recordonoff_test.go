package vlapi

import (
	"strings"
	"testing"

	"github.com/vodlabs-io/recorder/internal/streamer"
)

// record_off must resolve a URL before keying a rule on it: a URL-keyed rule
// matches nothing at record time (streamerRule.matches compares a resolved ID,
// whose URL is empty).
func TestRecordOff_URLKeysOnResolvedID(t *testing.T) {
	key := streamer.StreamerKey{Platform: "twitch", Username: "streamer_two"}
	url := "https://www.twitch.tv/streamer_two"
	s, sf, path := newAddService(t, resolveTo(key))

	sf.AddID(streamer.StreamerRef{StreamerKey: key})
	if err := sf.Write(); err != nil {
		t.Fatalf("seed: %v", err)
	}

	req := addRequest(t, streamer.StreamerRef{URL: url})
	s.handleRecordOff(req)
	if env := req.envelope(t); !env.OK {
		t.Fatalf("record_off failed: %+v", env.Error)
	}

	// The property that matters: the streamer a recording carries is off.
	if sf.IsRecording(streamer.StreamerRef{StreamerKey: key}) {
		t.Errorf("%s still recording after disable by URL:\n%s", key, fileContents(t, path))
	}
	if strings.Contains(fileContents(t, path), url) {
		t.Errorf("rule still keyed on the URL:\n%s", fileContents(t, path))
	}
}

// The mirror image: record_on by URL must clear the rule keyed on the resolved
// identity.
func TestRecordOn_URLClearsResolvedRule(t *testing.T) {
	key := streamer.StreamerKey{Platform: "twitch", Username: "streamer_two"}
	url := "https://www.twitch.tv/streamer_two"
	s, sf, path := newAddService(t, resolveTo(key))

	sf.AddID(streamer.StreamerRef{StreamerKey: key})
	if err := sf.AddRecordOffRule(streamer.StreamerRef{StreamerKey: key}); err != nil {
		t.Fatalf("seed rule: %v", err)
	}
	if err := sf.Write(); err != nil {
		t.Fatalf("write: %v", err)
	}

	req := addRequest(t, streamer.StreamerRef{URL: url})
	s.handleRecordOn(req)
	if env := req.envelope(t); !env.OK {
		t.Fatalf("record_on failed: %+v", env.Error)
	}
	if !sf.IsRecording(streamer.StreamerRef{StreamerKey: key}) {
		t.Errorf("%s still disabled after enable by URL:\n%s", key, fileContents(t, path))
	}
}

// Enabling by URL must also clear a rule keyed on the raw URL, or the streamer
// stays off with no rule the resolved path can see.
func TestRecordOn_URLClearsStaleURLRule(t *testing.T) {
	key := streamer.StreamerKey{Platform: "twitch", Username: "streamer_two"}
	url := "https://www.twitch.tv/streamer_two"
	s, sf, path := newAddService(t, resolveTo(key))

	sf.AddID(streamer.StreamerRef{StreamerKey: key})
	if err := sf.AddRecordOffRule(streamer.StreamerRef{URL: url}); err != nil {
		t.Fatalf("seed stale rule: %v", err)
	}
	if err := sf.Write(); err != nil {
		t.Fatalf("write: %v", err)
	}

	req := addRequest(t, streamer.StreamerRef{URL: url})
	s.handleRecordOn(req)
	if env := req.envelope(t); !env.OK {
		t.Fatalf("record_on failed: %+v", env.Error)
	}
	// The rule, not the [streamers] entry: seeding through AddRecordOffRule also
	// lists the URL as a tracked streamer, and enabling is not remove's job.
	if !sf.IsRecording(streamer.StreamerRef{StreamerKey: key}) {
		t.Errorf("%s still disabled after enable by URL:\n%s", key, fileContents(t, path))
	}
	if strings.Contains(fileContents(t, path), "record = false") {
		t.Errorf("stale URL-keyed record=false rule survived:\n%s", fileContents(t, path))
	}
}

// `vod streamer remove url <url>` has to find the streamer under the identity
// the add resolved it to, not the URL the user typed.
func TestRemoveStreamer_ResolvesURL(t *testing.T) {
	key := streamer.StreamerKey{Platform: "twitch", Username: "streamer_two"}
	s, sf, path := newAddService(t, resolveTo(key))

	sf.AddID(streamer.StreamerRef{StreamerKey: key})
	if err := sf.Write(); err != nil {
		t.Fatalf("seed: %v", err)
	}

	req := addRequest(t, streamer.StreamerRef{URL: "https://www.twitch.tv/streamer_two"})
	s.handleRemoveStreamer(req)
	if env := req.envelope(t); !env.OK {
		t.Fatalf("remove failed: %+v", env.Error)
	}
	if strings.Contains(fileContents(t, path), "streamer_two") {
		t.Errorf("streamer survived remove by URL:\n%s", fileContents(t, path))
	}
}
