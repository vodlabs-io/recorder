package cloud

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/nats-io/nkeys"

	"github.com/vodlabs-io/recorder/internal/vlstate"
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

// TestFailedConnectIsNotSubscribed covers a subscribe the cloud accepted whose
// NATS connect then failed. Left marked subscribed, with a JWT valid for weeks,
// the ticker would never retry and no notification would reach the daemon.
func TestFailedConnectIsNotSubscribed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req subscribeRequest
		json.NewDecoder(r.Body).Decode(&req)
		json.NewEncoder(w).Encode(subscribeResponse{
			UserJWT:      "jwt",
			ExpiresAt:    time.Now().Add(30 * 24 * time.Hour).Unix(),
			ConsumerName: "consumer",
			StreamerIDs:  req.StreamerIDs,
		})
	}))
	defer srv.Close()
	// A port nothing listens on.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	deadNATS := "ws://" + ln.Addr().String()
	ln.Close()
	prevAPI, prevNATS := apiBaseURL, natsEndpoint
	apiBaseURL, natsEndpoint = srv.URL, deadNATS
	defer func() { apiBaseURL, natsEndpoint = prevAPI, prevNATS }()

	c, _ := diffClient(t, "[streamers]\ntwitch = [\"somebody\"]\n")
	_, kv := newTestKV(t)
	c.kv, c.store = kv, vlstate.NewStore(kv)
	c.httpClient = srv.Client()
	kp, _ := nkeys.CreateUser()
	seed, _ := kp.Seed()
	c.nkeySeed = string(seed)

	if err := c.newSubscription(context.Background()); err == nil {
		t.Fatal("newSubscription succeeded against a dead NATS endpoint")
	}
	c.mu.RLock()
	subscribed := c.isSubscribed
	c.mu.RUnlock()
	if subscribed {
		t.Fatal("a cycle whose connect failed left the client marked subscribed")
	}
}
