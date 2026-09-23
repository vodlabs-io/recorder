package cloud

import (
	"context"
	"testing"
	"time"
)

// TestDrainTriggers covers the burst-coalescing helper that keeps the
// subscription loop under the cloud's subscribe rate limit: it waits out a
// window, swallowing any resubscribe or reload triggers that arrive so they
// fold into the single subscription that follows.
func TestDrainTriggers(t *testing.T) {
	t.Run("waits the full window and reports settled", func(t *testing.T) {
		c := testClient(t)
		const d = 60 * time.Millisecond
		start := time.Now()
		if ok := c.drainTriggers(context.Background(), d); !ok {
			t.Fatal("drainTriggers = false, want true (window elapsed)")
		}
		if elapsed := time.Since(start); elapsed < d {
			t.Errorf("returned after %v, want >= %v", elapsed, d)
		}
	})

	t.Run("coalesces triggers that arrive during the window", func(t *testing.T) {
		c := testClient(t)
		// Fire a burst while drainTriggers is waiting; every trigger must be
		// consumed so the channel is empty once it returns.
		go func() {
			for i := 0; i < 5; i++ {
				c.triggerResubscribe()
				time.Sleep(5 * time.Millisecond)
			}
		}()
		if ok := c.drainTriggers(context.Background(), 60*time.Millisecond); !ok {
			t.Fatal("drainTriggers = false, want true")
		}
		if resubscribed(c) {
			t.Error("a trigger survived drainTriggers; burst not fully coalesced")
		}
	})

	t.Run("coalesces reloads too", func(t *testing.T) {
		c := testClient(t)
		// A reload asks for the file to be re-read, which the subscribe that
		// follows the window does anyway.
		go func() {
			for i := 0; i < 5; i++ {
				c.TriggerReload()
				time.Sleep(5 * time.Millisecond)
			}
		}()
		if ok := c.drainTriggers(context.Background(), 60*time.Millisecond); !ok {
			t.Fatal("drainTriggers = false, want true")
		}
		if reloadQueued(c) {
			t.Error("a reload survived drainTriggers; it would re-read a file the subscribe just read")
		}
	})

	t.Run("returns early when the context is cancelled", func(t *testing.T) {
		c := testClient(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		start := time.Now()
		if ok := c.drainTriggers(ctx, time.Hour); ok {
			t.Fatal("drainTriggers = true, want false (context cancelled)")
		}
		if elapsed := time.Since(start); elapsed > time.Second {
			t.Errorf("blocked for %v after cancel, want prompt return", elapsed)
		}
	})
}
