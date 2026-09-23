package cloud

import (
	"errors"
	"net/http"
	"testing"
	"time"
)

// TestSubscribeRetryHoldGrows pins the shape of the backoff: the first retry is
// prompt, and repeated failure settles at a cadence the endpoint's hourly
// budget can absorb.
func TestSubscribeRetryHoldGrows(t *testing.T) {
	transport := errors.New("connection refused")

	if got := subscribeRetryHold(0, nil); got != 0 {
		t.Errorf("hold with no failures = %v, want 0", got)
	}
	if got := subscribeRetryHold(1, transport); got != subscribeRetryBase {
		t.Errorf("first hold = %v, want %v", got, subscribeRetryBase)
	}
	if got := subscribeRetryHold(2, transport); got != 2*subscribeRetryBase {
		t.Errorf("second hold = %v, want %v", got, 2*subscribeRetryBase)
	}

	prev := time.Duration(0)
	for n := 1; n <= 20; n++ {
		got := subscribeRetryHold(n, transport)
		if got < prev {
			t.Fatalf("hold(%d) = %v, went backwards from %v", n, got, prev)
		}
		if got > subscribeRetryMax {
			t.Fatalf("hold(%d) = %v, over the ceiling %v", n, got, subscribeRetryMax)
		}
		prev = got
	}
	if prev != subscribeRetryMax {
		t.Errorf("hold settled at %v, want the ceiling %v", prev, subscribeRetryMax)
	}
}

// TestSubscribeRetryHoldRefused covers the distinction the whole type exists
// for: a request the cloud reasoned about and refused will not become
// acceptable by being re-sent, so it waits longer than a dropped connection.
func TestSubscribeRetryHoldRefused(t *testing.T) {
	overCap := &subscribeRefused{status: http.StatusBadRequest, reason: "400 Bad Request"}
	if got := subscribeRetryHold(1, overCap); got < subscribeRefusedFloor {
		t.Errorf("first hold after a 4xx = %v, want at least %v", got, subscribeRefusedFloor)
	}

	// 429 is the budget itself saying stop. Climbing to the ceiling would spend
	// what is left of the budget on the way up.
	tooMany := &subscribeRefused{status: http.StatusTooManyRequests, reason: "429 Too Many Requests"}
	if got := subscribeRetryHold(1, tooMany); got != subscribeRetryMax {
		t.Errorf("first hold after a 429 = %v, want the ceiling %v", got, subscribeRetryMax)
	}

	// A server-side fault is the cloud failing, not refusing: it may well be
	// gone by the next attempt, so it keeps the ordinary schedule.
	server := &subscribeRefused{status: http.StatusBadGateway, reason: "502 Bad Gateway"}
	if got := subscribeRetryHold(1, server); got != subscribeRetryBase {
		t.Errorf("first hold after a 5xx = %v, want %v", got, subscribeRetryBase)
	}
}

// TestOverCapacityIsNotDefinitive is the data-loss guard. definitiveRejections
// feeds DropURLs, which deletes the entry from streamers.toml — so treating a
// full subscription as a verdict would silently eat a URL entry the user can
// no longer recover, for a condition that ends the moment room is made.
func TestOverCapacityIsNotDefinitive(t *testing.T) {
	full := subscribeRejection{
		URL:      "https://www.youtube.com/@somebody",
		Platform: "youtube",
		Username: "somebody",
		Error:    codeOverCapacity,
	}
	if full.Definitive() {
		t.Error("over_capacity reported definitive; the entry would be deleted from config")
	}
	if urls := definitiveRejections([]subscribeRejection{full}); len(urls) != 0 {
		t.Errorf("definitiveRejections = %v, want none", urls)
	}

	// The codes that really are verdicts must still be acted on, or a junk
	// entry stays in the file and is re-sent forever.
	junk := subscribeRejection{URL: "https://example.com/nope", Error: codeUnsupportedSite}
	if !junk.Definitive() {
		t.Error("unsupported_site reported non-definitive; junk entries would never be cleaned up")
	}
	if urls := definitiveRejections([]subscribeRejection{junk}); len(urls) != 1 {
		t.Errorf("definitiveRejections = %v, want the junk URL", urls)
	}
}

// TestRejectionNamesTheStreamer matters because the log line is the only place
// an over-capacity rejection surfaces: it carries no URL, so reporting the URL
// alone prints an empty field for the one rejection a user can act on.
func TestRejectionNamesTheStreamer(t *testing.T) {
	full := subscribeRejection{Platform: "youtube", Username: "somebody", Error: codeOverCapacity}
	if got := full.name(); got != "youtube.somebody" {
		t.Errorf("name = %q, want youtube.somebody", got)
	}
	byURL := subscribeRejection{URL: "https://example.com/nope", Error: codeUnsupportedSite}
	if got := byURL.name(); got != "https://example.com/nope" {
		t.Errorf("name = %q, want the URL", got)
	}
}
