package cloud

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nkeys"
	"github.com/vodlabs-io/recorder/internal/streamer"
)

// The message a refused add shows must name the failure that happened and say
// that nothing was added, without carrying the raw Go error.
func TestUnreachable_MessageByFailureKind(t *testing.T) {
	cause := errors.New(`Post "https://api.vodlabs.io:443/api/v1/streamers/resolve": ` +
		`context deadline exceeded (Client.Timeout exceeded while awaiting headers)`)

	expired, cancel := context.WithCancel(context.Background())
	cancel()

	tests := []struct {
		name     string
		ctx      context.Context
		err      error
		wantSaid string
		wantGone string
	}{
		{
			name:     "deadline",
			ctx:      expired,
			err:      cause,
			wantSaid: "took too long",
			wantGone: "Could not reach",
		},
		{
			name:     "deadline reported by the client only",
			ctx:      context.Background(),
			err:      context.DeadlineExceeded,
			wantSaid: "took too long",
			wantGone: "Could not reach",
		},
		{
			name:     "no route to the cloud",
			ctx:      context.Background(),
			err:      errors.New("dial tcp: no such host"),
			wantSaid: "Could not reach",
			wantGone: "took too long",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := unreachable(tc.ctx, tc.err)

			if !strings.Contains(got.Message, tc.wantSaid) {
				t.Errorf("Message = %q, want it to mention %q", got.Message, tc.wantSaid)
			}
			if strings.Contains(got.Message, tc.wantGone) {
				t.Errorf("Message = %q, must not mention %q", got.Message, tc.wantGone)
			}
			// The whole point: an add that was refused says so, so the retry
			// does not read as a conflict.
			if !strings.Contains(got.Message, "Nothing was added") {
				t.Errorf("Message = %q, want it to say nothing was added", got.Message)
			}
			if strings.Contains(got.Message, "http") || strings.Contains(got.Message, "context deadline") {
				t.Errorf("Message = %q leaks the transport error", got.Message)
			}
			// Still an unavailable verdict, so the add is refused rather than
			// the streamer blamed.
			if got.Definitive() {
				t.Errorf("Definitive() = true, want false for a transport failure")
			}
			// The detail stays reachable for anything that wants it.
			if !errors.Is(got, tc.err) {
				t.Errorf("errors.Is(got, cause) = false, want the cause preserved")
			}
		})
	}
}

// TestResolveStreamer_NotBoundByShortSharedClient pins that the preflight runs
// on the shared transport but not the shared client's deadline, which would
// pre-empt the cloud's own answer about which platform failed and why.
func TestResolveStreamer_NotBoundByShortSharedClient(t *testing.T) {
	const sharedCeiling = 20 * time.Millisecond

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(4 * sharedCeiling)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"platform":"pandalive","username":"streamer_eleven"}`))
	}))
	defer srv.Close()

	prev := apiBaseURL
	apiBaseURL = srv.URL
	defer func() { apiBaseURL = prev }()

	seed, err := nkeys.CreateUser()
	if err != nil {
		t.Fatalf("create user key: %v", err)
	}
	rawSeed, err := seed.Seed()
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	c := &Client{
		nkeySeed: string(rawSeed),
		// Stands in for the daemon's shared client, whose ceiling is sized for
		// ordinary API calls rather than a preflight.
		httpClient: &http.Client{Timeout: sharedCeiling},
	}

	// Generous, the way the daemon's add deadline is relative to the cloud's.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	got, err := c.ResolveStreamer(ctx, streamer.StreamerRef{URL: "https://www.pandalive.co.kr/play/streamer_eleven"})
	if err != nil {
		t.Fatalf("ResolveStreamer: %v (the shared ceiling is bounding the preflight)", err)
	}
	if got.StreamerKey.Platform != "pandalive" || got.StreamerKey.Username != "streamer_eleven" {
		t.Errorf("resolved = %+v, want pandalive/streamer_eleven", got.StreamerKey)
	}
}

// The caller's deadline is still honoured — dropping the shared ceiling must
// not leave the preflight unbounded.
func TestResolveStreamer_HonoursCallerDeadline(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(2 * time.Second)
	}))
	defer srv.Close()

	prev := apiBaseURL
	apiBaseURL = srv.URL
	defer func() { apiBaseURL = prev }()

	seed, err := nkeys.CreateUser()
	if err != nil {
		t.Fatalf("create user key: %v", err)
	}
	rawSeed, err := seed.Seed()
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	c := &Client{nkeySeed: string(rawSeed), httpClient: &http.Client{}}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err = c.ResolveStreamer(ctx, streamer.StreamerRef{URL: "https://www.pandalive.co.kr/play/streamer_eleven"})
	if err == nil {
		t.Fatal("ResolveStreamer = nil error, want the caller's deadline to bound it")
	}
	if !strings.Contains(err.Error(), "took too long") {
		t.Errorf("error = %q, want the deadline message", err.Error())
	}
}
