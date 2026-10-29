package cloud

import (
	"context"
	"encoding/json"
	"time"
)

// RegistrationKey is the KV key holding whether this daemon has an identity yet.
// It rides the clients' existing "daemon.>" KV watch, so every UI receives it
// without a new subscription.
const RegistrationKey = "daemon.registration"

// Retry pacing for a failed registration. The first attempts are quick because
// the usual cause is a host that is not on the network yet and comes back
// within seconds; the ceiling keeps a daemon left running beside a genuinely
// unreachable API from polling it forever.
const (
	registerRetryMin = 5 * time.Second
	registerRetryMax = 5 * time.Minute
)

// registrationState is the published document.
type registrationState struct {
	// Registered is false only before this daemon has ever obtained an
	// identity. Once true it stays true: the credentials are on disk.
	Registered bool `json:"registered"`
	// Error is the last failure, for a UI to show rather than leaving the user
	// to read container logs.
	Error string `json:"error,omitempty"`
	// Attempts counts failures so far, so a UI can distinguish "just started"
	// from "has been failing for a while".
	Attempts  int       `json:"attempts,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}

// nextRegisterDelay backs off geometrically up to the ceiling.
func nextRegisterDelay(d time.Duration) time.Duration {
	if d *= 2; d > registerRetryMax {
		return registerRetryMax
	}
	return d
}

// publishRegistration writes the current registration state to the local KV.
// Best effort: this exists to explain a failure, and must never become one.
func (c *Client) publishRegistration(ctx context.Context, st registrationState) {
	if c.kv == nil {
		return
	}
	st.UpdatedAt = time.Now().UTC()
	b, err := json.Marshal(st)
	if err != nil {
		return
	}
	if _, err := c.kv.Put(ctx, RegistrationKey, b); err != nil {
		c.logger.Debug("Failed to publish registration state", "error", err)
	}
}

// retryRegistration re-attempts registration until it succeeds or ctx ends,
// then runs the cloud startup that Init had to skip. It reports whether the
// daemon ended up registered. The daemon stays up meanwhile so its UIs can show
// why it is not registered.
func (c *Client) retryRegistration(ctx context.Context) bool {
	log := c.logger.With("func", "retryRegistration")
	delay := registerRetryMin
	for attempt := 1; ; attempt++ {
		select {
		case <-ctx.Done():
			return false
		case <-time.After(jitter(delay)):
		}
		if c.isBlocked() {
			// The block's probe asks instead; register once it lifts.
			if !c.waitUnblocked(ctx) {
				return false
			}
			delay = registerRetryMin
		}

		if err := c.register(ctx); err != nil {
			log.Warn("Registration retry failed",
				"attempt", attempt, "retry_in", delay.String(), "error", err)
			c.publishRegistration(ctx, registrationState{
				Error: err.Error(), Attempts: attempt,
			})
			delay = nextRegisterDelay(delay)
			continue
		}

		log.Info("Registered after retrying", "attempts", attempt)
		c.publishRegistration(ctx, registrationState{Registered: true})
		c.startCloudWork(ctx)
		return true
	}
}
