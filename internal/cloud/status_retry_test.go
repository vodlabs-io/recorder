package cloud

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nkeys"

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
	// Requests are signed, so the Client needs a key to reach the stand-in.
	key, err := nkeys.CreateUser()
	if err != nil {
		t.Fatalf("create user key: %v", err)
	}
	seed, err := key.Seed()
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	// A retry passes the daemon-wide ceiling like any other request, so the
	// Client under test needs one that is not in the way.
	c := &Client{
		logger:     logger,
		nkeySeed:   string(seed),
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

	t.Run("givingUpReportsTheLastError", func(t *testing.T) {
		l := newLimiter(statusFetchRetries)
		h := &recordingHandler{}
		c.retryStatusFetch(context.Background(), slog.New(h), l, id, kindStatus)

		rec, ok := h.find("Giving up on status fetch")
		if !ok {
			t.Fatal("no give-up record")
		}
		var got error
		rec.Attrs(func(a slog.Attr) bool {
			if e, ok := a.Value.Any().(error); ok {
				got = e
				return false
			}
			return true
		})
		// The Sentry bridge titles the event by the first error attr.
		if got == nil || got.Error() == "" {
			t.Fatal("give-up carries no error, so Sentry titles it \"None\"")
		}
	})

	t.Run("givingUpOnAnotherFailureIsAnError", func(t *testing.T) {
		l := newLimiter(statusFetchRetries)
		h := &recordingHandler{}
		c.retryStatusFetch(context.Background(), slog.New(h), l, id, kindStatus)

		rec, ok := h.find("Giving up on status fetch")
		if !ok {
			t.Fatal("no give-up record")
		}
		if rec.Level != slog.LevelError {
			t.Fatalf("level = %v, want ERROR", rec.Level)
		}
	})

	// The user's own network failing is theirs to fix: the give-up is only a
	// warning, so the Sentry bridge (which reports ERROR) leaves it out.
	t.Run("givingUpOnThisMachinesNetworkIsAWarning", func(t *testing.T) {
		unreachable := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "This recorder could not reach Twitch: no such host.", http.StatusFailedDependency)
		}))
		defer unreachable.Close()
		apiBaseURL = unreachable.URL
		defer func() { apiBaseURL = srv.URL }()

		l := newLimiter(statusFetchRetries)
		h := &recordingHandler{}
		c.retryStatusFetch(context.Background(), slog.New(h), l, id, kindStatus)

		rec, ok := h.find("Giving up on status fetch")
		if !ok {
			t.Fatal("no give-up record")
		}
		if rec.Level != slog.LevelWarn {
			t.Fatalf("level = %v, want WARN", rec.Level)
		}
	})

	// A streamer the subscription doesn't hold gets the same answer every time,
	// so the retry stops at the first refusal and gives up on nothing.
	t.Run("stopsAtNotSubscribed", func(t *testing.T) {
		var calls int
		var mu sync.Mutex
		refused := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			mu.Lock()
			calls++
			mu.Unlock()
			http.Error(w, "Streamer not in your subscriptions", http.StatusForbidden)
		}))
		defer refused.Close()
		apiBaseURL = refused.URL
		defer func() { apiBaseURL = srv.URL }()

		l := newLimiter(statusFetchRetries)
		h := &recordingHandler{}
		c.retryStatusFetch(context.Background(), slog.New(h), l, id, kindStatus)

		if calls != 1 {
			t.Fatalf("fetched %d times, want 1", calls)
		}
		if _, ok := h.find("Giving up on status fetch"); ok {
			t.Fatal("gave up on a streamer the subscription doesn't hold")
		}
		if l.fails != 0 {
			t.Fatalf("limiter counted %d failures, want 0", l.fails)
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

// recordingHandler keeps every record logged through it.
type recordingHandler struct {
	mu   sync.Mutex
	recs []slog.Record
}

func (h *recordingHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *recordingHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.recs = append(h.recs, r.Clone())
	return nil
}
func (h *recordingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *recordingHandler) WithGroup(string) slog.Handler      { return h }

func (h *recordingHandler) find(msg string) (slog.Record, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, r := range h.recs {
		if r.Message == msg {
			return r, true
		}
	}
	return slog.Record{}, false
}
