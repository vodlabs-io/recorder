package cloud

import (
	"context"
	"net/http"
	"testing"

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
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the attempt itself returns at once; the guard is what we check

	c.noteResponse(ctx, unauthorized(true))
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
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	c.noteResponse(ctx, unauthorized(true))
	first := c.reregistering.Load()
	c.noteResponse(ctx, unauthorized(true))

	if !first {
		t.Fatal("the first refusal did not start a registration")
	}
}
