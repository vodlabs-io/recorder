package cloud

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/vodlabs-io/recorder/internal/streamer"
	"github.com/vodlabs-io/recorder/internal/vlconfig"
	"github.com/vodlabs-io/recorder/internal/vllog"
)

func drainTestClient(t *testing.T, depth int) *Client {
	t.Helper()
	log, err := vllog.NewFileLogger(vlconfig.LoggerConfig{Mode: "off", Level: "error"}, "test")
	if err != nil {
		t.Fatalf("NewFileLogger: %v", err)
	}
	c := &Client{
		logger:          log,
		statusReqCh:     make(chan streamer.StreamerRef, depth),
		StatusReqPriCh:  make(chan streamer.StreamerRef, depth),
		authStatusReqCh: make(chan streamer.StreamerRef, depth),
		StreamReqCh:     make(chan streamer.StreamerRef, depth),
	}
	c.cond = sync.NewCond(&c.mu)
	// Nothing here gets as far as serving a request, but a fetch left running
	// past the test that started it reads state the next one rewrites, so the
	// drain is registered rather than reasoned about.
	t.Cleanup(c.waitFetches)
	return c
}

// A resubscribe drops the subscribed flag for its whole round trip, while
// producers keep filling the daemon-wide status queues with blocking sends. The
// router is their only drain, so it must keep routing while unsubscribed or a
// burst deeper than the buffer blocks its producer.
func TestStatusRequestsDrainWhileUnsubscribed(t *testing.T) {
	// Deliberately shallower than the burst below: the property is that the
	// router keeps draining, not that the burst happens to fit.
	const depth = 4
	c := drainTestClient(t, depth)
	c.isSubscribed = false // mid-resubscribe: nothing may reach the cloud yet

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.rateLimitStatusRequests(ctx)

	sent := make(chan struct{})
	go func() {
		defer close(sent)
		for i := range depth * 5 {
			c.statusReqCh <- streamer.StreamerRef{StreamerKey: streamer.StreamerKey{
				Platform: "tiktok",
				Username: string(rune('a' + i%26)),
			}}
		}
	}()

	select {
	case <-sent:
	case <-time.After(5 * time.Second):
		t.Fatal("a burst of status requests blocked its producer while the daemon " +
			"was unsubscribed; the resubscribe that would release it can never " +
			"finish, so the daemon stays wedged")
	}
}

// Nothing may reach the cloud until the subscription is live again. The lane
// waits before taking a request, so a waiting request stays queued.
func TestStatusLaneHoldsRequestsUntilSubscribed(t *testing.T) {
	c := drainTestClient(t, 4)
	c.isSubscribed = false

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	l := c.limiterFor(ctx, "tiktok")
	id := streamer.StreamerRef{StreamerKey: streamer.StreamerKey{Platform: "tiktok", Username: "alice"}}
	l.highPri <- id

	// serve() would need a real httpClient, so the observable is the lane: an
	// unsubscribed daemon must leave the request sitting in it.
	time.Sleep(300 * time.Millisecond)
	if len(l.highPri) != 1 {
		t.Fatal("a status check left its lane while the daemon was unsubscribed; " +
			"it is on its way to a cloud connection that is not open")
	}

	// And a shutdown must release the lane servers rather than leave them
	// sleeping on a flag no resubscribe is left to set.
	cancel()
	deadline := time.After(5 * time.Second)
	for {
		if len(l.highPri) == 1 {
			select {
			case <-deadline:
				return // still queued, never served: correct
			case <-time.After(20 * time.Millisecond):
			}
			continue
		}
		t.Fatal("a cancelled lane server served a queued request on its way out")
	}
}
