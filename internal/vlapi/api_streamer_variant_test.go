package vlapi

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/vodlabs-io/recorder/internal/streamer"
	"github.com/vodlabs-io/recorder/internal/vlconfig"
)

func variantRequest(t *testing.T, id streamer.StreamerRef, variant string) *fakeRequest {
	t.Helper()
	b, err := json.Marshal(SetVariantRequest{StreamerRef: id, Variant: variant})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	return &fakeRequest{data: b}
}

// resolveTo answers every preflight with the same platform/username, standing in
// for the cloud decoding a watch URL.
func resolveTo(key streamer.StreamerKey) func(context.Context, streamer.StreamerRef) (streamer.StreamerRef, error) {
	return func(_ context.Context, id streamer.StreamerRef) (streamer.StreamerRef, error) {
		if id.StreamerKey.Valid() {
			return id, nil
		}
		return streamer.StreamerRef{StreamerKey: key}, nil
	}
}

// TestSetStreamerVariant_URLKeysOnResolvedID checks that a variant set by URL
// is keyed on the resolved identity: streamerRule.matches compares a resolved
// ID, whose URL is empty, so a URL-keyed rule would never fire.
func TestSetStreamerVariant_URLKeysOnResolvedID(t *testing.T) {
	key := streamer.StreamerKey{Platform: "kick", Username: "streamer_ten"}
	s, sf, path := newAddService(t, resolveTo(key))

	req := variantRequest(t, streamer.StreamerRef{URL: "https://kick.com/streamer_ten"}, "worst")
	s.handleSetStreamerVariant(req)
	if env := req.envelope(t); !env.OK {
		t.Fatalf("set variant failed: %+v", env.Error)
	}

	// The property that matters: the rule applies to the ID a recording carries.
	if got := sf.OptionsFor(streamer.StreamerRef{StreamerKey: key}).Variant; got != "worst" {
		t.Errorf("variant for %s = %q, want %q\n%s", key, got, "worst", fileContents(t, path))
	}
	if strings.Contains(fileContents(t, path), "https://kick.com/streamer_ten") {
		t.Errorf("rule still keyed on the URL:\n%s", fileContents(t, path))
	}
}

// Setting the variant by URL must replace a rule keyed on the raw URL, not
// leave both, or clearing it later would miss one.
func TestSetStreamerVariant_ReplacesStaleURLRule(t *testing.T) {
	key := streamer.StreamerKey{Platform: "kick", Username: "streamer_ten"}
	url := "https://kick.com/streamer_ten"
	s, sf, path := newAddService(t, resolveTo(key))

	if err := sf.SetVariantRule(streamer.StreamerRef{URL: url}, "worst"); err != nil {
		t.Fatalf("seed stale rule: %v", err)
	}
	if err := sf.Write(); err != nil {
		t.Fatalf("write: %v", err)
	}

	req := variantRequest(t, streamer.StreamerRef{URL: url}, "720p")
	s.handleSetStreamerVariant(req)
	if env := req.envelope(t); !env.OK {
		t.Fatalf("set variant failed: %+v", env.Error)
	}

	if got := sf.OptionsFor(streamer.StreamerRef{StreamerKey: key}).Variant; got != "720p" {
		t.Errorf("variant for %s = %q, want %q\n%s", key, got, "720p", fileContents(t, path))
	}
	if strings.Contains(fileContents(t, path), url) {
		t.Errorf("stale URL rule survived:\n%s", fileContents(t, path))
	}
}

// A platform/username request must not take the resolve path at all: it is
// already the identity a recording carries, and daemons run without a cloud
// client wired.
func TestSetStreamerVariant_KeyIDNeedsNoResolve(t *testing.T) {
	key := streamer.StreamerKey{Platform: "twitch", Username: "streamer_four"}
	s, sf, _ := newAddService(t, func(context.Context, streamer.StreamerRef) (streamer.StreamerRef, error) {
		t.Error("resolve called for a platform/username ID")
		return streamer.StreamerRef{}, nil
	})

	req := variantRequest(t, streamer.StreamerRef{StreamerKey: key}, "best")
	s.handleSetStreamerVariant(req)
	if env := req.envelope(t); !env.OK {
		t.Fatalf("set variant failed: %+v", env.Error)
	}
	if got := sf.OptionsFor(streamer.StreamerRef{StreamerKey: key}).Variant; got != "best" {
		t.Errorf("variant = %q, want %q", got, "best")
	}
}

// An unresolvable URL must fail loudly rather than write a rule that provably
// cannot match.
func TestSetStreamerVariant_UnresolvableURLFails(t *testing.T) {
	s, _, path := newAddService(t, func(context.Context, streamer.StreamerRef) (streamer.StreamerRef, error) {
		return streamer.StreamerRef{}, &rejection{msg: "no such streamer", definitive: true}
	})

	req := variantRequest(t, streamer.StreamerRef{URL: "https://kick.com/ghost"}, "worst")
	s.handleSetStreamerVariant(req)
	if env := req.envelope(t); env.OK {
		t.Fatal("expected failure for an unresolvable URL")
	}
	if c := fileContents(t, path); strings.Contains(c, "ghost") {
		t.Errorf("wrote a rule for a rejected URL:\n%s", c)
	}
}

// Without a cloud client there is nothing to resolve against, so the request is
// taken at its word — the same concession preflight makes for the add path.
func TestSetStreamerVariant_NoCloudKeepsURL(t *testing.T) {
	path := t.TempDir() + "/streamers.toml"
	sf, err := vlconfig.NewStreamersFile(path)
	if err != nil {
		t.Fatalf("streamers file: %v", err)
	}
	s := &Service{streamersFile: sf, signals: testSignals()}

	url := "https://kick.com/streamer_ten"
	req := variantRequest(t, streamer.StreamerRef{URL: url}, "worst")
	s.handleSetStreamerVariant(req)
	if env := req.envelope(t); !env.OK {
		t.Fatalf("set variant failed: %+v", env.Error)
	}
	if got := sf.OptionsFor(streamer.StreamerRef{URL: url}).Variant; got != "worst" {
		t.Errorf("variant for %s = %q, want %q", url, got, "worst")
	}
}

// countingResolve wraps a resolver so a test can assert how many round trips a
// handler made.
func countingResolve(n *int, inner func(context.Context, streamer.StreamerRef) (streamer.StreamerRef, error)) func(context.Context, streamer.StreamerRef) (streamer.StreamerRef, error) {
	return func(ctx context.Context, id streamer.StreamerRef) (streamer.StreamerRef, error) {
		*n++
		return inner(ctx, id)
	}
}

// TestAddStreamer_VariantAppliedInOneCall checks that a variant carried on the
// add is written before the subscribe that can start recording a live
// streamer, with a single resolve.
func TestAddStreamer_VariantAppliedInOneCall(t *testing.T) {
	key := streamer.StreamerKey{Platform: "pandalive", Username: "streamer_eleven"}
	resolves := 0
	s, sf, path := newAddService(t, countingResolve(&resolves, resolveTo(key)))

	b, err := json.Marshal(AddRequest{
		StreamerRef: streamer.StreamerRef{URL: "https://www.pandalive.co.kr/play/streamer_eleven"},
		Variant:     "worst",
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	req := &fakeRequest{data: b}
	s.handleAddStreamer(req)
	if env := req.envelope(t); !env.OK {
		t.Fatalf("add failed: %+v", env.Error)
	}

	if got := sf.OptionsFor(streamer.StreamerRef{StreamerKey: key}).Variant; got != "worst" {
		t.Errorf("variant for %s = %q, want %q\n%s", key, got, "worst", fileContents(t, path))
	}
	if resolves != 1 {
		t.Errorf("resolve calls = %d, want 1 (the add already knows the identity)", resolves)
	}
}

// An add carrying no variant is the payload every other client sends, and must
// still parse and still leave the streamer inheriting the config default.
func TestAddStreamer_BareRefStillParses(t *testing.T) {
	key := streamer.StreamerKey{Platform: "kick", Username: "streamer_ten"}
	s, sf, _ := newAddService(t, resolveTo(key))

	req := addRequest(t, streamer.StreamerRef{URL: "https://kick.com/streamer_ten"})
	s.handleAddStreamer(req)
	if env := req.envelope(t); !env.OK {
		t.Fatalf("add failed: %+v", env.Error)
	}
	if got := sf.OptionsFor(streamer.StreamerRef{StreamerKey: key}).Variant; got != "" {
		t.Errorf("variant = %q, want empty", got)
	}
}

// record_once takes the variant the same way, for the same reason.
func TestRecordOnce_VariantAppliedInOneCall(t *testing.T) {
	key := streamer.StreamerKey{Platform: "pandalive", Username: "streamer_eleven"}
	resolves := 0
	s, sf, path := newAddService(t, countingResolve(&resolves, resolveTo(key)))

	b, err := json.Marshal(AddRequest{
		StreamerRef: streamer.StreamerRef{URL: "https://www.pandalive.co.kr/play/streamer_eleven"},
		Variant:     "720p",
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	req := &fakeRequest{data: b}
	s.handleRecordOnce(req)
	if env := req.envelope(t); !env.OK {
		t.Fatalf("record_once failed: %+v", env.Error)
	}

	id := streamer.StreamerRef{StreamerKey: key}
	if got := sf.OptionsFor(id).Variant; got != "720p" {
		t.Errorf("variant for %s = %q, want %q\n%s", key, got, "720p", fileContents(t, path))
	}
	if !sf.HasOneShotRule(id) {
		t.Errorf("one-shot rule missing\n%s", fileContents(t, path))
	}
	if resolves != 1 {
		t.Errorf("resolve calls = %d, want 1", resolves)
	}
}
