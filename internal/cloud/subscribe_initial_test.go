package cloud

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestInitialSubDoneAfterFailedCycle pins the promise daemon startup depends
// on: cloud.Init blocks in waitInitialSub until the first subscription cycle
// concludes, and a cycle that FAILS concludes just as surely as one that
// succeeds. Otherwise a refused subscribe would block startup before any API
// endpoint is registered.
func TestInitialSubDoneAfterFailedCycle(t *testing.T) {
	c, path := diffClient(t, "")
	// Break the streamers file so the cycle fails at its first step, standing
	// in for every later error return without needing the network.
	if err := os.WriteFile(path, []byte("not = = valid toml"), 0644); err != nil {
		t.Fatalf("write streamers.toml: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.handleSubscription(ctx)

	done := make(chan struct{})
	go func() {
		c.waitInitialSub()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("waitInitialSub still blocked after a failed subscription cycle")
	}
}
