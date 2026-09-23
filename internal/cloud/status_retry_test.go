package cloud

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/vodlabs-io/recorder/internal/streamer"
	"github.com/vodlabs-io/recorder/internal/vlconfig"
	"github.com/vodlabs-io/recorder/internal/vllog"
)

// The cloud pushes an update only when a status changes, so a status fetch
// dropped on a transient error would strand an already-live streamer. These
// cover the retry that recovers it and the limits on that retry.
func TestRetryStatusFetch(t *testing.T) {
	// Always-failing stand-in for the cloud, so the retry path runs without
	// touching the real API.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	origURL, origBackoff := apiBaseURL, statusFetchRetryBackoff
	apiBaseURL = srv.URL
	statusFetchRetryBackoff = time.Millisecond
	t.Cleanup(func() { apiBaseURL, statusFetchRetryBackoff = origURL, origBackoff })

	logger, err := vllog.NewFileLogger(vlconfig.LoggerConfig{Mode: "off", Level: "error"}, "test")
	if err != nil {
		t.Fatalf("logger: %v", err)
	}
	// A retry passes the daemon-wide ceiling like any other request, so the
	// Client under test needs one that is not in the way.
	c := &Client{
		logger:     logger,
		httpClient: srv.Client(),
		ceiling:    newTokenBucket(t.Context(), vlconfig.RateLimit{Burst: 64, Interval: time.Millisecond}),
	}
	log := logger.With("func", "test")
	id := streamer.StreamerRef{
		StreamerKey: streamer.StreamerKey{Platform: "twitch", Username: "alice"},
	}

	// A limiter whose status budget starts full with a few spare tokens and
	// refills far too slowly to top itself up during the test, so what the
	// retry consumed is exactly what is missing at the end.
	newLimiter := func(burst int) *platformLimiter {
		return &platformLimiter{
			platform: "twitch",
			status:   newTokenBucket(t.Context(), vlconfig.RateLimit{Burst: burst, Interval: time.Hour}),
			streams:  newTokenBucket(t.Context(), vlconfig.RateLimit{Burst: burst, Interval: time.Hour}),
		}
	}

	t.Run("takesOneTokenPerAttemptAndGivesUp", func(t *testing.T) {
		const spare = 2
		l := newLimiter(statusFetchRetries + spare)
		tokens := l.bucket(kindStatus).tokens

		c.retryStatusFetch(context.Background(), log, l, id, kindStatus)

		consumed := (statusFetchRetries + spare) - len(tokens)
		if consumed != statusFetchRetries {
			t.Fatalf("retry consumed %d tokens, want %d: retries must be bounded and "+
				"rate-limited through the same bucket as first attempts", consumed, statusFetchRetries)
		}
	})

	t.Run("stopsOnContextCancel", func(t *testing.T) {
		l := newLimiter(statusFetchRetries)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		done := make(chan struct{})
		go func() {
			c.retryStatusFetch(ctx, log, l, id, kindStatus)
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("retry kept going after its context was cancelled")
		}
	})
}
