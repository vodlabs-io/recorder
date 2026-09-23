package cloud

import (
	"context"
	"encoding/json"
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
