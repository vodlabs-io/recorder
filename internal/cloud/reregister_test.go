package cloud

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vodlabs-io/recorder/internal/vlconfig"
	"github.com/vodlabs-io/recorder/internal/vllog"
)

func reregisterTestClient(t *testing.T) *Client {
	t.Helper()
	log, err := vllog.NewFileLogger(vlconfig.LoggerConfig{Mode: "off", Level: "error"}, "test")
	if err != nil {
		t.Fatalf("NewFileLogger: %v", err)
	}
	return &Client{logger: log, clientID: "old-client"}
}

// holdRegistration points the client at a service whose registration
// challenge never answers, so a started registration stays in flight until the
// test ends. It returns the context to pass to noteResponse, the number of
// challenges requested, and a channel closed on the first one.
func holdRegistration(t *testing.T, c *Client) (context.Context, *atomic.Int32, <-chan struct{}) {
	t.Helper()
	var challenges atomic.Int32
	first := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == registerChallengeEndpoint && challenges.Add(1) == 1 {
			close(first)
		}
		<-r.Context().Done()
	}))
	prev := apiBaseURL
	apiBaseURL = srv.URL
	c.httpClient = srv.Client()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		deadline := time.Now().Add(5 * time.Second)
		for c.reregistering.Load() && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		srv.Close()
		apiBaseURL = prev
	})
	return ctx, &challenges, first
}

func waitFor(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

func unauthorized(unknown bool) *http.Response {
	resp := &http.Response{StatusCode: http.StatusUnauthorized, Header: http.Header{}}
	if unknown {
		resp.Header.Set(clientUnknownHeader, "1")
	}
	return resp
}

// The service losing its record of a client is the one 401 a daemon can fix,
// and nothing else ever re-registers it.
func TestUnknownClientStartsReregistration(t *testing.T) {
	c := reregisterTestClient(t)
	ctx, _, first := holdRegistration(t, c)

	c.noteResponse(ctx, unauthorized(true))
	waitFor(t, first, "the registration challenge")
	if !c.reregistering.Load() {
		t.Fatal("an unknown-client 401 did not start a registration")
	}
}

// Any other 401 is a fault to report. Re-registering would throw away a good
// identity without fixing a bad signature or a skewed clock.
func TestOtherUnauthorizedIsLeftAlone(t *testing.T) {
	c := reregisterTestClient(t)
	c.noteResponse(context.Background(), unauthorized(false))
	if c.reregistering.Load() {
		t.Fatal("a plain 401 started a registration")
	}
	if c.clientID != "old-client" {
		t.Fatalf("client_id = %q, want the identity left in place", c.clientID)
	}
}

func TestSuccessfulResponsesAreLeftAlone(t *testing.T) {
	c := reregisterTestClient(t)
	for _, code := range []int{200, 204, 426, 429, 500, 503} {
		c.noteResponse(context.Background(), &http.Response{StatusCode: code, Header: http.Header{}})
		if c.reregistering.Load() {
			t.Fatalf("HTTP %d started a registration", code)
		}
	}
	c.noteResponse(context.Background(), nil)
}

// Every signed call in flight sees the same 401, and they must not each start
// their own registration.
func TestConcurrentRefusalsStartOneRegistration(t *testing.T) {
	c := reregisterTestClient(t)
	ctx, challenges, first := holdRegistration(t, c)

	c.noteResponse(ctx, unauthorized(true))
	waitFor(t, first, "the first registration challenge")
	c.noteResponse(ctx, unauthorized(true))

	if !c.reregistering.Load() {
		t.Fatal("the registration in flight was not marked")
	}
	if n := challenges.Load(); n != 1 {
		t.Fatalf("%d registrations started, want 1", n)
	}
}
