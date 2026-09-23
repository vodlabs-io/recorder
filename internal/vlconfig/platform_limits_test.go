package vlconfig

import (
	"encoding/json"
	"testing"
	"time"
)

func TestLimitsFallBackWhenUnset(t *testing.T) {
	restore := Platforms()
	t.Cleanup(func() { SetPlatforms(restore) })
	SetPlatforms([]Platform{{Name: "bare"}})

	if got := StatusLimitFor("bare"); got != defaultStatusLimit {
		t.Errorf("StatusLimitFor(bare) = %+v, want the default %+v", got, defaultStatusLimit)
	}
	if got := StreamLimitFor("bare"); got != defaultStreamLimit {
		t.Errorf("StreamLimitFor(bare) = %+v, want the default %+v", got, defaultStreamLimit)
	}
	// An unknown platform is paced too — a URL-only lookup has no name until
	// the cloud matches it, and no budget would mean no checks at all.
	if got := StatusLimitFor("nonexistent"); got != defaultStatusLimit {
		t.Errorf("StatusLimitFor(unknown) = %+v, want the default", got)
	}
	if got := StatusLimitFor(""); got.Burst == 0 || got.Interval == 0 {
		t.Errorf("StatusLimitFor(\"\") = %+v, want a usable budget", got)
	}
}

// The limits decide how hard this machine knocks on a platform's door, so a
// value from the cloud is used but never unbounded.
func TestLimitsAreClamped(t *testing.T) {
	restore := Platforms()
	t.Cleanup(func() { SetPlatforms(restore) })
	SetPlatforms([]Platform{
		{Name: "huge", StatusLimit: &RateLimit{Burst: 10_000, Interval: time.Microsecond}},
		{Name: "tiny", StatusLimit: &RateLimit{Burst: 1, Interval: 10 * time.Hour}},
		{Name: "zero", StatusLimit: &RateLimit{}},
	})

	if got := StatusLimitFor("huge"); got.Burst != maxLimitBurst || got.Interval != minLimitInterval {
		t.Errorf("huge = %+v, want burst %d and interval %s", got, maxLimitBurst, minLimitInterval)
	}
	if got := StatusLimitFor("tiny"); got.Interval != maxLimitInterval {
		t.Errorf("tiny interval = %s, want it capped at %s", got.Interval, maxLimitInterval)
	}
	// A zero-valued object says nothing; it must read as absent rather than as
	// a limiter that never hands out a token.
	if got := StatusLimitFor("zero"); got != defaultStatusLimit {
		t.Errorf("zero = %+v, want the default", got)
	}
}

// SetPlatforms's change detection is what decides whether a new pace is picked
// up at all, so it has to see a limit change.
func TestSetPlatformsNoticesALimitChange(t *testing.T) {
	restore := Platforms()
	t.Cleanup(func() { SetPlatforms(restore) })

	base := []Platform{{Name: "kick", StatusLimit: &RateLimit{Burst: 3, Interval: time.Second}}}
	SetPlatforms(base)
	if SetPlatforms([]Platform{{Name: "kick", StatusLimit: &RateLimit{Burst: 3, Interval: time.Second}}}) {
		t.Error("an identical list reported a change")
	}
	if !SetPlatforms([]Platform{{Name: "kick", StatusLimit: &RateLimit{Burst: 9, Interval: time.Second}}}) {
		t.Error("a retuned limit went unnoticed: the new pace would never be used")
	}
	if got := StatusLimitFor("kick"); got.Burst != 9 {
		t.Errorf("burst = %d, want the backend's 9", got.Burst)
	}
}

// The wire carries milliseconds; the cache is written and read back through the
// same shape, so a limit has to survive the round trip.
func TestRateLimitJSONRoundTrip(t *testing.T) {
	raw := []byte(`{"name":"youtube","stream_limit":{"burst":4,"interval_ms":1500}}`)
	var p Platform
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if p.StreamLimit == nil || p.StreamLimit.Burst != 4 ||
		p.StreamLimit.Interval != 1500*time.Millisecond {
		t.Fatalf("decoded %+v, want burst 4 / 1.5s", p.StreamLimit)
	}

	out, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back Platform
	if err := json.Unmarshal(out, &back); err != nil {
		t.Fatalf("re-unmarshal: %v", err)
	}
	if *back.StreamLimit != *p.StreamLimit {
		t.Errorf("round trip changed the limit: %+v -> %+v", p.StreamLimit, back.StreamLimit)
	}
}

// A list without limits (e.g. a name-only cache) must not strip the pace
// YouTube's short-lived grants need.
func TestEmbeddedLimitsBackfillWhatTheBackendOmits(t *testing.T) {
	restore := Platforms()
	t.Cleanup(func() { SetPlatforms(restore) })
	SetPlatforms([]Platform{{Name: "youtube"}})

	got := StreamLimitFor("youtube")
	if got == defaultStreamLimit {
		t.Fatal("youtube fell back to the default: the embedded stream_limit was dropped")
	}
	if got.Interval > 2*time.Second {
		t.Errorf("youtube stream interval = %s, want the embedded pace", got.Interval)
	}

	// A cloud value still wins over the embedded one.
	SetPlatforms([]Platform{{Name: "youtube", StreamLimit: &RateLimit{Burst: 2, Interval: 5 * time.Second}}})
	if got := StreamLimitFor("youtube"); got.Interval != 5*time.Second {
		t.Errorf("interval = %s, want the backend's 5s to win", got.Interval)
	}
}
