package cloud

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"
)

// ServiceStateKey is the KV key holding whether the service is reachable. It
// rides the clients' existing "daemon.>" KV watch, so every UI receives it
// without a new subscription.
const ServiceStateKey = "daemon.service"

// serviceState is the published document.
type serviceState struct {
	// Reachable is false while the daemon's connection to the service is down.
	// Nothing is checked meanwhile, so no recording can start — the ones already
	// running carry on.
	Reachable bool `json:"reachable"`
	// Error is the last connection failure, for a UI to show rather than
	// leaving the user to find the daemon's log.
	Error string `json:"error,omitempty"`
	// Since is when the current state began.
	Since     time.Time `json:"since"`
	UpdatedAt time.Time `json:"updated_at"`
	// SubscribeError is set while the last subscribe failed: the service may
	// still be reachable, but streamers added since the last good subscribe
	// are not being checked. SubscribeRetryAt is when the daemon retries on
	// its own; RateLimited marks a refusal for subscribing too often.
	SubscribeError   string    `json:"subscribe_error,omitempty"`
	SubscribeRetryAt time.Time `json:"subscribe_retry_at,omitzero"`
	RateLimited      bool      `json:"rate_limited,omitempty"`
}

// publishServiceState records whether the service can be reached.
//
// Without it an outage is invisible: streamers keep showing whatever status
// they last had, nothing starts recording, and the app looks broken rather than
// disconnected. Repeated calls with the same verdict keep the original Since,
// so a UI can say how long it has been down.
//
// Best effort: this exists to explain a failure, and must never become one.
func (c *Client) publishServiceState(ctx context.Context, reachable bool, cause string) {
	now := time.Now().UTC()
	since := now
	if prev := c.serviceState.Load(); prev != nil && prev.Reachable == reachable {
		since = prev.Since
		if cause == "" {
			cause = prev.Error
		}
	}

	st := serviceState{Reachable: reachable, Error: cause, Since: since, UpdatedAt: now}
	if prev := c.serviceState.Load(); prev != nil {
		st.SubscribeError, st.SubscribeRetryAt, st.RateLimited = prev.SubscribeError, prev.SubscribeRetryAt, prev.RateLimited
	}
	c.storeServiceState(ctx, st)
}

// publishSubscribeState records the outcome of a subscribe: err nil clears a
// previous failure, otherwise retryAt is when the daemon tries again.
func (c *Client) publishSubscribeState(ctx context.Context, err error, retryAt time.Time) {
	st := serviceState{Reachable: true, Since: time.Now().UTC()}
	if prev := c.serviceState.Load(); prev != nil {
		st = *prev
	}
	if err == nil {
		if st.SubscribeError == "" {
			return
		}
		st.SubscribeError, st.SubscribeRetryAt, st.RateLimited = "", time.Time{}, false
	} else {
		var refused *subscribeRefused
		st.SubscribeError = err.Error()
		st.SubscribeRetryAt = retryAt.UTC()
		st.RateLimited = errors.As(err, &refused) && refused.status == http.StatusTooManyRequests
	}
	st.UpdatedAt = time.Now().UTC()
	c.storeServiceState(ctx, st)
}

// storeServiceState keeps st as the current state and publishes it.
func (c *Client) storeServiceState(ctx context.Context, st serviceState) {
	c.serviceState.Store(&st)

	// The verdict is kept either way; only its publication needs the store.
	if c.kv == nil {
		return
	}

	b, err := json.Marshal(st)
	if err != nil {
		return
	}
	if _, err := c.kv.Put(ctx, ServiceStateKey, b); err != nil {
		c.logger.Debug("Failed to publish service state", "error", err)
	}
}
