package cloud

import (
	"context"
	"log/slog"
	"sync"

	"github.com/nats-io/nats.go"

	"github.com/vodlabs-io/recorder/internal/browserfetch"
	"github.com/vodlabs-io/recorder/internal/streamer"
)

// parkedChecks holds checks the cloud could not run for want of a browser
// context until a desktop app provides one. Retrying cannot help, and on a
// CLI-only install a parked check may correctly never run. A set, so a streamer
// parked twice is asked for once.
type parkedChecks struct {
	mu  sync.Mutex
	ids map[string]streamer.StreamerRef
}

func (p *parkedChecks) park(id streamer.StreamerRef) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ids == nil {
		p.ids = make(map[string]streamer.StreamerRef)
	}
	p.ids[id.String()] = id
}

// take empties the set and returns what was in it, so a browser context that
// appears twice does not ask for the same checks twice.
func (p *parkedChecks) take() []streamer.StreamerRef {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]streamer.StreamerRef, 0, len(p.ids))
	for _, id := range p.ids {
		out = append(out, id)
	}
	p.ids = nil
	return out
}

// WatchBrowserContext re-asks for parked checks whenever a desktop app announces
// a browser context, which it does once per connection. The subscription lasts
// as long as ctx. Does nothing when the daemon has no local NATS, which is every
// headless build — there, a parked check simply stays parked.
func (c *Client) WatchBrowserContext(ctx context.Context) {
	log := c.logger.With("func", "WatchBrowserContext")
	if c.localNC == nil {
		return
	}
	sub, err := c.localNC.Subscribe(browserfetch.ReadySubject, func(*nats.Msg) {
		c.releaseParkedChecks(ctx, log)
	})
	if err != nil {
		log.Warn("Failed to watch for a browser context", "error", err)
		return
	}
	go func() {
		<-ctx.Done()
		_ = sub.Unsubscribe()
	}()
}

func (c *Client) releaseParkedChecks(ctx context.Context, log *slog.Logger) {
	ids := c.parked.take()
	if len(ids) == 0 {
		return
	}
	log.Info("Browser context available; re-checking what it was needed for", "count", len(ids))
	for _, id := range ids {
		select {
		case c.StatusReqPriCh <- id:
		case <-ctx.Done():
			return
		default:
			// Re-park what does not fit rather than dropping it: the queue
			// draining is what makes room, and the next announcement asks again.
			c.parked.park(id)
			log.Warn("Status queue full; leaving the check parked", "id", id)
		}
	}
}
