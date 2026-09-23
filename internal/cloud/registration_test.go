package cloud

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"
)

// failingTransport fails every request without touching the network, standing
// in for the offline first run this file is about.
type failingTransport struct{ calls int }

func (t *failingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	t.calls++
	return nil, errors.New("dial tcp: no route to host")
}

func TestNextRegisterDelay(t *testing.T) {
	d := nextRegisterDelay(registerRetryMin)
	if d != 2*registerRetryMin {
		t.Fatalf("first backoff = %v, want %v", d, 2*registerRetryMin)
	}
	// Climbing from the minimum must settle exactly on the ceiling and stay.
	for i := 0; i < 20; i++ {
		d = nextRegisterDelay(d)
	}
	if d != registerRetryMax {
		t.Fatalf("backoff settled at %v, want the %v ceiling", d, registerRetryMax)
	}
	if got := nextRegisterDelay(registerRetryMax); got != registerRetryMax {
		t.Fatalf("backoff past the ceiling = %v, want %v", got, registerRetryMax)
	}
}

// A daemon shutting down while registration is still failing must not keep the
// process alive waiting on the next attempt.
func TestRetryRegistrationStopsOnContextCancel(t *testing.T) {
	c := testClient(t)
	c.httpClient = &http.Client{Transport: &failingTransport{}}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan bool, 1)
	go func() { done <- c.retryRegistration(ctx) }()

	select {
	case registered := <-done:
		if registered {
			t.Fatal("a cancelled retry reported success")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("retryRegistration ignored a cancelled context")
	}
}

// publishRegistration exists to explain a failure and must never cause one, so
// a client with no KV yet is not a panic.
func TestPublishRegistrationWithoutKV(t *testing.T) {
	c := testClient(t)
	c.publishRegistration(context.Background(), registrationState{Error: "boom"})
}
