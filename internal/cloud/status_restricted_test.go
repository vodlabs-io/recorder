package cloud

import (
	"context"
	"testing"
	"time"

	"github.com/vodlabs-io/recorder/internal/streamer"
	"github.com/vodlabs-io/recorder/internal/vlconfig"
	"github.com/vodlabs-io/recorder/internal/vllog"
	"github.com/vodlabs-io/recorder/internal/vlstate"
)

func restrictedRoom(platform, user string, status streamer.LiveStatus) streamer.StreamerState {
	return streamer.StreamerState{
		StreamerRef: streamer.StreamerRef{StreamerKey: streamer.StreamerKey{Platform: platform, Username: user}},
		Status:      status,
	}
}

func newRecheckClient(t *testing.T) *Client {
	t.Helper()
	logger, err := vllog.NewFileLogger(vlconfig.LoggerConfig{Mode: "off", Level: "error"}, "test")
	if err != nil {
		t.Fatalf("logger: %v", err)
	}
	old := restrictedRecheckDelays
	restrictedRecheckDelays = []time.Duration{20 * time.Millisecond}
	t.Cleanup(func() { restrictedRecheckDelays = old })
	_, kv := newTestKV(t)
	return &Client{
		kv:             kv,
		store:          vlstate.NewStore(kv),
		logger:         logger,
		StatusReqPriCh: make(chan streamer.StreamerRef, 4),
		StreamerCh:     make(chan streamer.StreamerState, 8),
	}
}

func expectRecheck(t *testing.T, c *Client, want streamer.StreamerRef) {
	t.Helper()
	select {
	case got := <-c.StatusReqPriCh:
		if got != want {
			t.Fatalf("re-checked %v, want %v", got, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no re-check was queued")
	}
}

func expectNoRecheck(t *testing.T, c *Client) {
	t.Helper()
	select {
	case got := <-c.StatusReqPriCh:
		t.Fatalf("unexpected re-check of %v", got)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestRestrictedRoomIsRecheckedUntilItChanges(t *testing.T) {
	ctx := context.Background()
	c := newRecheckClient(t)
	room := restrictedRoom("bigo", "alice", streamer.StatusRestricted)

	// Each result that stays restricted leads to one more re-check.
	for range 2 {
		c.handleFetchResult(ctx, room, false)
		expectRecheck(t, c, room.StreamerRef)
	}

	// Any other answer ends them.
	c.handleFetchResult(ctx, restrictedRoom("bigo", "alice", streamer.StatusLive), false)
	expectNoRecheck(t, c)
}

func TestRestrictedRecheckStopsOnADocThatMovedOn(t *testing.T) {
	ctx := context.Background()
	c := newRecheckClient(t)
	room := restrictedRoom("bigo", "alice", streamer.StatusRestricted)

	c.handleFetchResult(ctx, room, false)
	// The service announces the room offline while the re-check waits.
	c.dispatchStreamer(ctx, restrictedRoom("bigo", "alice", streamer.StatusOffline))
	expectNoRecheck(t, c)
}

func TestRestrictedRecheckOnlyOnAddressDependentPlatforms(t *testing.T) {
	c := newRecheckClient(t)
	c.handleFetchResult(context.Background(), restrictedRoom("stripchat", "bob", streamer.StatusRestricted), false)
	expectNoRecheck(t, c)
}

func TestRestrictedRechecksBackOffAndDoNotStack(t *testing.T) {
	old := restrictedRecheckDelays
	restrictedRecheckDelays = []time.Duration{time.Second, time.Minute}
	t.Cleanup(func() { restrictedRecheckDelays = old })

	var r restrictedRechecks
	id := restrictedRoom("bigo", "alice", streamer.StatusRestricted).StreamerRef
	if d, ok := r.next(id); !ok || d != time.Second {
		t.Fatalf("first = %v, %v", d, ok)
	}
	if _, ok := r.next(id); ok {
		t.Fatal("a second re-check was scheduled while one waits")
	}
	for range 2 {
		r.fired(id)
		if d, ok := r.next(id); !ok || d != time.Minute {
			t.Fatalf("later = %v, %v; want the last delay repeated", d, ok)
		}
	}
	r.forget(id)
	if d, ok := r.next(id); !ok || d != time.Second {
		t.Fatalf("after forget = %v, %v; want the backoff restarted", d, ok)
	}
}
