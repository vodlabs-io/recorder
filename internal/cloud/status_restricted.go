package cloud

import (
	"context"
	"sync"
	"time"

	"github.com/vodlabs-io/recorder/internal/streamer"
	"github.com/vodlabs-io/recorder/internal/vlstate"
)

// withholdsByAddress names the platforms that hand a live room's stream URL to
// some addresses and not others, so that restricted says little about the room.
// A re-check from this machine usually gets the stream, and an unlucky one has
// a good chance on its next try. On other platforms restricted is the room's own
// state, a paid show for one, and asking again would only spend requests.
var withholdsByAddress = map[string]bool{
	"bigo": true,
}

// restrictedRecheckDelays is how long to wait before each further plain re-check
// of a room that stays restricted. The last one repeats until the room changes.
// The service announces only changes, so nothing else would ask again.
var restrictedRecheckDelays = []time.Duration{
	30 * time.Second,
	time.Minute,
	2 * time.Minute,
	5 * time.Minute,
}

// restrictedRechecks tracks the rooms waiting on a re-check. The zero value is
// ready to use.
type restrictedRechecks struct {
	mu      sync.Mutex
	entries map[streamer.StreamerRef]*restrictedRecheck
}

type restrictedRecheck struct {
	attempts int
	waiting  bool
}

// next records a further re-check of id and returns how long to wait for it,
// or false when one is already waiting.
func (r *restrictedRechecks) next(id streamer.StreamerRef) (time.Duration, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.entries == nil {
		r.entries = make(map[streamer.StreamerRef]*restrictedRecheck)
	}
	e := r.entries[id]
	if e == nil {
		e = &restrictedRecheck{}
		r.entries[id] = e
	}
	if e.waiting {
		return 0, false
	}
	e.waiting = true
	i := min(e.attempts, len(restrictedRecheckDelays)-1)
	e.attempts++
	return restrictedRecheckDelays[i], true
}

// fired marks id's wait as over, so its result may schedule the next one.
func (r *restrictedRechecks) fired(id streamer.StreamerRef) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if e := r.entries[id]; e != nil {
		e.waiting = false
	}
}

// forget ends id's re-checks, and with them its backoff.
func (r *restrictedRechecks) forget(id streamer.StreamerRef) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.entries, id)
}

// scheduleRestrictedRecheck follows a plain check's result. A room still
// restricted on a platform in withholdsByAddress is asked about again later; any
// other answer ends the re-checks.
func (c *Client) scheduleRestrictedRecheck(ctx context.Context, str streamer.StreamerState) {
	if str.Status != streamer.StatusRestricted || !withholdsByAddress[str.Platform] {
		c.restricted.forget(str.StreamerRef)
		return
	}
	delay, ok := c.restricted.next(str.StreamerRef)
	if !ok {
		return
	}
	time.AfterFunc(delay, func() { c.recheckRestricted(ctx, str.StreamerRef) })
}

// recheckRestricted queues a plain re-check of id unless the room no longer
// needs one: the doc is the current word on whether anyone wants it recorded and
// whether it has since changed.
func (c *Client) recheckRestricted(ctx context.Context, id streamer.StreamerRef) {
	c.restricted.fired(id)
	doc, err := vlstate.GetStreamerDoc(ctx, c.kv, id.StreamerKey)
	if err != nil || !sweepEligible(doc) || doc.Status != streamer.StatusRestricted {
		c.restricted.forget(id)
		return
	}
	select {
	case c.StatusReqPriCh <- id:
	case <-ctx.Done():
	}
}
