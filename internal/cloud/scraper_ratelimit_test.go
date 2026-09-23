package cloud

import (
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/vodlabs-io/recorder/internal/vlconfig"
	"github.com/vodlabs-io/recorder/internal/vllog"
)

// A refusal (e.g. from switching every streamer back on at once) must leave the
// handler's lock usable: a double unlock is a fatal error that takes the whole
// daemon down.
func TestScrapeHandlerRateLimitRefusal(t *testing.T) {
	newHandler := func(t *testing.T) *scrapeHandler {
		t.Helper()
		log, err := vllog.NewFileLogger(vlconfig.LoggerConfig{Mode: "off", Level: "info"}, "test")
		if err != nil {
			t.Fatalf("NewFileLogger: %v", err)
		}
		return &scrapeHandler{
			logger:        log,
			checkTarget:   allowAnyScrapeTarget,
			maxConcurrent: 2,
			maxPerMinute:  3,
			lastMinute:    time.Now(),
		}
	}

	// An unbound Msg cannot deliver a reply, which respondWithError logs and
	// carries on from — the refusal path itself is what is under test.
	msg := &nats.Msg{}

	t.Run("refusesOverTheMinuteBudget", func(t *testing.T) {
		h := newHandler(t)
		h.maxConcurrent = 100 // leave only the per-minute budget binding
		for i := range h.maxPerMinute {
			if err := h.checkRateLimit(msg, true); err != nil {
				t.Fatalf("request %d within budget was refused: %v", i, err)
			}
		}
		if err := h.checkRateLimit(msg, true); err == nil {
			t.Fatal("a request over the per-minute budget was admitted")
		}
		// The handler survived the refusal and still holds a usable lock.
		h.mu.Lock()
		got := h.currentRequests
		h.mu.Unlock()
		if got != h.maxPerMinute {
			t.Fatalf("refused request took a concurrency slot: currentRequests=%d, want %d",
				got, h.maxPerMinute)
		}
	})

	t.Run("refusesOverTheConcurrencyLimit", func(t *testing.T) {
		h := newHandler(t)
		h.maxPerMinute = 100 // leave only the concurrency limit binding
		for i := range h.maxConcurrent {
			if err := h.checkRateLimit(msg, true); err != nil {
				t.Fatalf("request %d within the limit was refused: %v", i, err)
			}
		}
		if err := h.checkRateLimit(msg, true); err == nil {
			t.Fatal("a request over the concurrency limit was admitted")
		}
		h.mu.Lock()
		got := h.currentRequests
		h.mu.Unlock()
		if got != h.maxConcurrent {
			t.Fatalf("refused request took a concurrency slot: currentRequests=%d, want %d",
				got, h.maxConcurrent)
		}
	})

	// A refusal races every other scrape in flight, so the guard runs under a
	// concurrent burst too.
	t.Run("survivesAConcurrentBurst", func(t *testing.T) {
		h := newHandler(t)
		done := make(chan struct{})
		for range 50 {
			go func() {
				defer func() { done <- struct{}{} }()
				_ = h.checkRateLimit(msg, true)
			}()
		}
		for range 50 {
			<-done
		}
	})
}
