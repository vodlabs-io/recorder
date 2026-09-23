package cloud

import (
	"context"
	"testing"
	"time"

	"github.com/vodlabs-io/recorder/internal/vlconfig"
	"github.com/vodlabs-io/recorder/internal/vllog"
)

// A UI says how long the service has been unreachable, so repeated verdicts
// have to keep the moment the outage began rather than restarting the clock.
func TestServiceStateKeepsSinceAcrossRepeats(t *testing.T) {
	log, err := vllog.NewFileLogger(vlconfig.LoggerConfig{Mode: "off", Level: "error"}, "test")
	if err != nil {
		t.Fatalf("NewFileLogger: %v", err)
	}
	c := &Client{logger: log} // no KV: the published copy is what is under test

	ctx := context.Background()
	c.publishServiceState(ctx, false, "dial tcp: connection refused")
	first := c.serviceState.Load()
	if first == nil || first.Reachable {
		t.Fatal("an unreachable service was not recorded")
	}

	time.Sleep(2 * time.Millisecond)
	c.publishServiceState(ctx, false, "")
	second := c.serviceState.Load()
	if !second.Since.Equal(first.Since) {
		t.Fatalf("Since moved from %v to %v; the outage's start must hold", first.Since, second.Since)
	}
	if second.Error != first.Error {
		t.Fatalf("Error = %q, want the cause kept: %q", second.Error, first.Error)
	}

	c.publishServiceState(ctx, true, "")
	back := c.serviceState.Load()
	if !back.Reachable || back.Since.Equal(first.Since) {
		t.Fatalf("recovery = %+v, want reachable with its own Since", back)
	}
	if back.Error != "" {
		t.Fatalf("Error = %q, want it cleared once the service is back", back.Error)
	}
}
