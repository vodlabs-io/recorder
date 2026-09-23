package cloud

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
)

// connect connects to NATS using the current user JWT
func (c *Client) connect() error {
	log := c.logger.With("func", "connect")
	headers := make(http.Header)
	headers.Set("X-Client-ID", c.clientID)
	headers.Set("X-Subscription-ID", c.subscriptionID)

	// A revoked-authentication error means this connection's JWT is dead, so
	// retrying it every ReconnectWait would never recover. Resubscribe once per
	// connection instead, which mints a fresh one.
	var revokedOnce sync.Once
	onRevoked := func() {
		revokedOnce.Do(func() {
			log.Warn("Authentication was revoked; resubscribing for a fresh token")
			c.triggerResubscribe()
		})
	}

	opts := []nats.Option{
		nats.Name(c.clientID),
		nats.UserJWTAndSeed(c.userJWT, c.nkeySeed),
		nats.WebSocketConnectionHeaders(headers),
		nats.MaxReconnects(-1),
		nats.ReconnectWait(5 * time.Second),
		nats.DisconnectErrHandler(func(_ *nats.Conn, err error) {
			cause := ""
			if err != nil {
				log.Error("NATS disconnected", "error", err)
				cause = err.Error()
			}
			// Nothing is checked while this is down, so say so: an outage is
			// otherwise indistinguishable from every streamer being offline.
			c.publishServiceState(context.Background(), false, cause)
		}),
		nats.ReconnectHandler(func(_ *nats.Conn) {
			log.Info("NATS reconnected")
			c.publishServiceState(context.Background(), true, "")
			c.resumeLanes() // the path is back; stop holding off
		}),
		nats.ErrorHandler(func(_ *nats.Conn, _ *nats.Subscription, err error) {
			log.Error("NATS error", "error", err)
			if errors.Is(err, nats.ErrAuthRevoked) {
				onRevoked()
			}
		}),
	}

	nc, err := nats.Connect(natsEndpoint, opts...)
	if err != nil {
		return fmt.Errorf("NATS connection failed: %w", err)
	}

	// Written under the lock: readers take the current connection concurrently
	// with reconnect cycles.
	c.mu.Lock()
	c.nc = nc
	c.mu.Unlock()
	c.publishServiceState(context.Background(), true, "")
	log.Info("Connected to NATS",
		"endpoint", natsEndpoint,
		"account", c.accountID,
		"subscription_id", c.subscriptionID)

	return nil
}

// closeConnection closes the NATS connection
func (c *Client) closeConnection() {
	log := c.logger.With("func", "closeConnection")

	if c.nc != nil {
		c.nc.Close()
		log.Info("NATS connection closed")
	}
}
