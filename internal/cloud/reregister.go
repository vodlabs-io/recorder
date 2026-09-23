package cloud

import (
	"context"
	"net/http"
	"time"

	"github.com/vodlabs-io/recorder/internal/version"
)

// clientUnknownHeader marks a 401 the service sends when it has no record of
// this client, as opposed to any other authentication failure. See the api's
// client middleware, which sets it.
const clientUnknownHeader = "X-Client-Unknown"

// reregisterRetryMin is the pause before a re-registration is attempted again
// after it fails. It is short because the daemon has no identity meanwhile and
// every request it makes is refused; registerRetryMax bounds the growth.
const reregisterRetryMin = 5 * time.Second

// noteResponse watches a signed call's reply for the one answer the daemon
// cannot retry its way out of: the service saying it has never heard of this
// client. Every signed call passes its response here.
//
// Identities are handed out once and kept forever, so this means the service
// lost its record of ours — a restore from a backup taken before we registered
// is the realistic way. Nothing re-registers a daemon on its own, so without
// this it would keep signing requests as a client that no longer exists, and
// every one of them would be refused, for as long as it ran.
func (c *Client) noteResponse(ctx context.Context, resp *http.Response) {
	if resp == nil {
		return
	}
	if resp.StatusCode == http.StatusUpgradeRequired {
		c.noteUpgradeRequired(ctx)
		return
	}
	if resp.StatusCode != http.StatusUnauthorized {
		return
	}
	if resp.Header.Get(clientUnknownHeader) == "" {
		// Some other 401: a bad signature, a clock too far out. Re-registering
		// would throw away a good identity without fixing any of them.
		return
	}
	c.reregister(ctx)
}

// reregister replaces this daemon's identity with a new one and resubscribes
// with it. Concurrent callers collapse into the one attempt: every signed call
// in flight sees the same 401.
func (c *Client) reregister(ctx context.Context) {
	if !c.reregistering.CompareAndSwap(false, true) {
		return
	}

	log := c.logger.With("func", "reregister")
	log.Warn("The service does not know this client; registering again",
		"client_id", c.clientID)

	go func() {
		defer c.reregistering.Store(false)

		delay := reregisterRetryMin
		for attempt := 1; ; attempt++ {
			if ctx.Err() != nil {
				return
			}
			// register() overwrites the identity in memory and on disk, so a
			// failure leaves the old one in place rather than no identity.
			if err := c.register(ctx); err == nil {
				log.Info("Registered again after the service lost this client",
					"client_id", c.clientID, "attempts", attempt)
				c.publishRegistration(ctx, registrationState{Registered: true})
				// The new identity has no subscription and no NATS credentials
				// of its own. A resubscribe mints both.
				c.triggerResubscribe()
				return
			} else {
				log.Warn("Registering again failed",
					"attempt", attempt, "retry_in", delay.String(), "error", err)
				c.publishRegistration(ctx, registrationState{
					Error: err.Error(), Attempts: attempt,
				})
			}

			select {
			case <-ctx.Done():
				return
			case <-time.After(jitter(delay)):
			}
			delay = nextRegisterDelay(delay)
		}
	}()
}

// upgradeRequired remembers a 426 so the daemon says why its calls are being
// refused, even before the next heartbeat.
func (c *Client) noteUpgradeRequired(ctx context.Context) {
	if c.upgradeRequired.Swap(true) {
		return
	}
	c.logger.Warn("The service requires a newer version of this daemon; " +
		"it will keep running, but only the heartbeat is answered until it updates")
	if c.onVersionChecked == nil {
		return
	}
	// Carries the versions the last heartbeat reported, with the flag set.
	u := c.lastUpstream.Load()
	if u == nil {
		c.onVersionChecked(ctx, version.Upstream{UpdateRequired: true})
		return
	}
	upstream := *u
	upstream.UpdateRequired = true
	c.onVersionChecked(ctx, upstream)
}
