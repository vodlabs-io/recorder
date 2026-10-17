package cloud

import (
	"context"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"

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

// With no streamers left to subscribe to the daemon closes its own connection;
// that is not an outage, but a dropped connection still is.
func TestDisconnectReportsOutageOnlyForLostConnection(t *testing.T) {
	log, err := vllog.NewFileLogger(vlconfig.LoggerConfig{Mode: "off", Level: "error"}, "test")
	if err != nil {
		t.Fatalf("NewFileLogger: %v", err)
	}
	c := &Client{logger: log}

	srv, err := server.NewServer(&server.Options{Host: "127.0.0.1", Port: -1, NoLog: true, NoSigs: true})
	if err != nil {
		t.Fatalf("nats server: %v", err)
	}
	go srv.Start()
	if !srv.ReadyForConnections(5 * time.Second) {
		t.Fatal("nats server not ready")
	}
	t.Cleanup(srv.Shutdown)

	dial := func() (*nats.Conn, <-chan struct{}) {
		done := make(chan struct{})
		nc, err := nats.Connect(srv.ClientURL(),
			nats.DisconnectErrHandler(func(nc *nats.Conn, err error) {
				c.onDisconnect(nc, err)
				close(done)
			}))
		if err != nil {
			t.Fatalf("nats connect: %v", err)
		}
		return nc, done
	}
	wait := func(done <-chan struct{}) {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("disconnect handler never ran")
		}
	}

	nc, done := dial()
	nc.Close()
	wait(done)
	if st := c.serviceState.Load(); st != nil {
		t.Fatalf("closing the connection ourselves published %+v", st)
	}

	_, done = dial()
	srv.Shutdown()
	wait(done)
	if st := c.serviceState.Load(); st == nil || st.Reachable {
		t.Fatalf("a dropped connection published %+v, want unreachable", st)
	}
}

// A refused subscribe is published beside reachability, survives a later
// reachability verdict, and clears once a subscribe succeeds.
func TestSubscribeStatePublished(t *testing.T) {
	log, err := vllog.NewFileLogger(vlconfig.LoggerConfig{Mode: "off", Level: "error"}, "test")
	if err != nil {
		t.Fatalf("NewFileLogger: %v", err)
	}
	c := &Client{logger: log}
	ctx := context.Background()

	retry := time.Now().Add(10 * time.Minute)
	c.publishSubscribeState(ctx, &subscribeRefused{status: 429, reason: "429 Too Many Requests"}, retry)
	st := c.serviceState.Load()
	if st == nil || st.SubscribeError == "" || !st.RateLimited || !st.SubscribeRetryAt.Equal(retry.UTC()) {
		t.Fatalf("after a 429: %+v", st)
	}
	if !st.Reachable {
		t.Fatal("a refused subscribe marked the service unreachable")
	}

	c.publishServiceState(ctx, true, "")
	if st := c.serviceState.Load(); st.SubscribeError == "" {
		t.Fatal("a reachability verdict dropped the subscribe failure")
	}

	c.publishSubscribeState(ctx, nil, time.Time{})
	if st := c.serviceState.Load(); st.SubscribeError != "" || st.RateLimited || !st.SubscribeRetryAt.IsZero() {
		t.Fatalf("after a good subscribe: %+v", st)
	}
}
