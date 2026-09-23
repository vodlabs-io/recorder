package cloud

import (
	"testing"
	"time"
)

// A resubscribe closes the cloud NATS connection, losing the reply of any scrape
// still being served. startScrapeHandler drains the handler before that close;
// these are the properties the drain has to hold.
func TestScrapeHandlerDrain(t *testing.T) {
	t.Run("waitsForAnInFlightScrape", func(t *testing.T) {
		h := &scrapeHandler{}
		if !h.begin() {
			t.Fatal("begin refused a scrape on a fresh handler")
		}

		drained := make(chan struct{})
		go func() {
			h.drain(2 * time.Second)
			close(drained)
		}()

		select {
		case <-drained:
			t.Fatal("drain returned while a scrape was still in flight; " +
				"the connection would close before its reply went out")
		case <-time.After(50 * time.Millisecond):
		}

		h.end()
		select {
		case <-drained:
		case <-time.After(2 * time.Second):
			t.Fatal("drain never returned after the in-flight scrape finished")
		}
	})

	t.Run("refusesScrapesOnceDraining", func(t *testing.T) {
		h := &scrapeHandler{}
		h.drain(time.Second) // nothing in flight: returns at once, draining stays set

		if h.begin() {
			t.Fatal("handler took a new scrape while draining; its reply cannot " +
				"survive the reconnect, so it must be refused instead")
		}
	})

	t.Run("isBoundedWhenAScrapeWedges", func(t *testing.T) {
		h := &scrapeHandler{}
		if !h.begin() {
			t.Fatal("begin refused a scrape on a fresh handler")
		}
		// Deliberately no end(): a scrape that never finishes must not hold the
		// resubscribe open, or a JWT refresh could miss its window.
		start := time.Now()
		h.drain(100 * time.Millisecond)
		if elapsed := time.Since(start); elapsed > time.Second {
			t.Fatalf("drain blocked %v on a wedged scrape; it must give up at the deadline", elapsed)
		}
	})

	t.Run("lastScrapeOutWakesDrain", func(t *testing.T) {
		h := &scrapeHandler{}
		for range 3 {
			if !h.begin() {
				t.Fatal("begin refused a concurrent scrape")
			}
		}

		drained := make(chan struct{})
		go func() {
			h.drain(2 * time.Second)
			close(drained)
		}()

		h.end()
		h.end()
		select {
		case <-drained:
			t.Fatal("drain returned with one scrape still in flight")
		case <-time.After(50 * time.Millisecond):
		}

		h.end()
		select {
		case <-drained:
		case <-time.After(2 * time.Second):
			t.Fatal("drain never returned after the last scrape finished")
		}
	})
}
