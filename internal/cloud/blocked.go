package cloud

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/vodlabs-io/recorder/internal/version"
)

// clientBlockedCode is the error the service answers every call with, under
// 403, while it refuses this recorder or its address. The body also carries
// "until": unix seconds, 0 for no set end.
const clientBlockedCode = "client_blocked"

// BlockedMessage is how a blocked recorder describes itself.
const BlockedMessage = "This recorder is blocked from VodLabs cloud services"

// While blocked, the recorder asks again once an hour, or as soon as the
// block's end passes, but never sooner than blockedProbeMin apart.
const (
	blockedProbeInterval = time.Hour
	blockedProbeMin      = time.Minute
)

// wsRefusedProbeGap spaces the checks a refused NATS WebSocket upgrade starts,
// since reconnects are attempted every few seconds.
const wsRefusedProbeGap = 10 * time.Minute

// errBlocked is returned in place of any call to the service while blocked.
var errBlocked = errors.New(BlockedMessage)

// blockState is one stretch of being blocked. until is guarded by blockMu.
type blockState struct {
	until  time.Time     // zero: no set end
	lifted chan struct{} // closed when the block lifts
}

// readBlocked reports whether resp is the service's blocked reply, and when
// the block ends (zero for no set end). The body stays readable for the caller.
func readBlocked(resp *http.Response) (time.Time, bool) {
	if resp == nil || resp.StatusCode != http.StatusForbidden || resp.Body == nil {
		return time.Time{}, false
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	resp.Body = struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(raw), resp.Body), resp.Body}

	var body struct {
		Error string `json:"error"`
		Until int64  `json:"until"`
	}
	if json.Unmarshal(raw, &body) != nil || body.Error != clientBlockedCode {
		return time.Time{}, false
	}
	if body.Until > 0 {
		return time.Unix(body.Until, 0), true
	}
	return time.Time{}, true
}

// noteBlockedReply puts the client into the blocked state if resp is the
// blocked reply, reporting whether it was.
func (c *Client) noteBlockedReply(ctx context.Context, resp *http.Response) bool {
	until, ok := readBlocked(resp)
	if ok {
		c.noteBlocked(ctx, until)
	}
	return ok
}

// isBlocked reports whether the service is refusing this recorder.
func (c *Client) isBlocked() bool {
	c.blockMu.Lock()
	defer c.blockMu.Unlock()
	return c.block != nil
}

// blockedUntil returns when the current block ends, zero for no set end.
func (c *Client) blockedUntil() time.Time {
	c.blockMu.Lock()
	defer c.blockMu.Unlock()
	if c.block == nil {
		return time.Time{}
	}
	return c.block.until
}

// checkBlocked is the gate every call to the service passes first.
func (c *Client) checkBlocked() error {
	if c.isBlocked() {
		return errBlocked
	}
	return nil
}

// waitUnblocked returns once no block is in force, or false if ctx ends first.
func (c *Client) waitUnblocked(ctx context.Context) bool {
	c.blockMu.Lock()
	b := c.block
	c.blockMu.Unlock()
	if b == nil {
		return true
	}
	select {
	case <-b.lifted:
		return true
	case <-ctx.Done():
		return false
	}
}

// lifetime returns the context work outliving a single call runs under.
func (c *Client) lifetime(ctx context.Context) context.Context {
	if c.runCtx != nil {
		return c.runCtx
	}
	return ctx
}

// noteBlocked enters the blocked state, or updates when it ends. Entering it
// stops everything that talks to the service: subscribing, the heartbeat,
// status checks and the NATS connection. One probe keeps asking until the
// service answers again.
func (c *Client) noteBlocked(ctx context.Context, until time.Time) {
	c.blockMu.Lock()
	if b := c.block; b != nil {
		changed := !b.until.Equal(until)
		b.until = until
		c.blockMu.Unlock()
		if changed {
			c.logger.Warn(BlockedMessage, "until", formatUntil(until))
			c.reportBlocked(c.lifetime(ctx))
		}
		return
	}
	c.block = &blockState{until: until, lifted: make(chan struct{})}
	c.blockMu.Unlock()

	c.logger.Warn(BlockedMessage+"; cloud work stops until the block lifts",
		"until", formatUntil(until))
	c.setSubscribed(false)
	c.closeConnection()
	run := c.lifetime(ctx)
	c.reportBlocked(run)
	go c.probeWhileBlocked(run)
}

// liftBlock leaves the blocked state and restarts the cloud work it stopped.
func (c *Client) liftBlock(ctx context.Context) {
	c.blockMu.Lock()
	b := c.block
	c.block = nil
	c.blockMu.Unlock()
	if b == nil {
		return
	}
	close(b.lifted)
	c.logger.Info("No longer blocked from VodLabs cloud services")
	c.reportBlocked(ctx)
	if c.clientID == "" {
		return // the registration retry is waiting on lifted
	}
	c.triggerResubscribe()
	// Status changes during the block were never delivered.
	go func() {
		if c.cond != nil && c.waitSubscribeCtx(ctx) {
			c.InitStatusRetrying(ctx)
		}
	}()
}

// probeWhileBlocked asks the service again on blockedProbeInterval, or once
// the block's end passes, until a call succeeds.
func (c *Client) probeWhileBlocked(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(nextProbeWait(c.blockedUntil(), time.Now())):
		}
		if c.probeBlock(ctx) {
			return
		}
	}
}

// nextProbeWait is how long a blocked recorder waits before asking again.
func nextProbeWait(until, now time.Time) time.Duration {
	wait := blockedProbeInterval
	if !until.IsZero() {
		// Spread out, so recorders blocked together do not return together.
		wait = min(wait, until.Sub(now)+jitter(blockedProbeMin)/2)
	}
	return max(wait, blockedProbeMin)
}

// probeBlock asks the service once, lifting the block if it answers. It
// reports whether the block is gone.
func (c *Client) probeBlock(ctx context.Context) bool {
	if !c.isBlocked() {
		return true
	}
	if err := c.probe(ctx); err != nil {
		c.logger.Info("Still blocked, or the service could not be asked", "error", err)
		return false
	}
	c.liftBlock(ctx)
	return true
}

// probe makes the cheapest call this recorder can: the heartbeat once it has
// an identity, the registration challenge before.
func (c *Client) probe(ctx context.Context) error {
	if c.clientID == "" {
		_, err := c.fetchPoWChallenge(ctx)
		return err
	}
	return c.heartbeat(ctx)
}

// probeAfterRefusedUpgrade checks whether a refused NATS WebSocket upgrade
// means this recorder is blocked: that refusal carries no reason of its own.
func (c *Client) probeAfterRefusedUpgrade(ctx context.Context) {
	if c.isBlocked() {
		return
	}
	now := time.Now().Unix()
	last := c.lastUpgradeProbe.Load()
	if now-last < int64(wsRefusedProbeGap/time.Second) || !c.lastUpgradeProbe.CompareAndSwap(last, now) {
		return
	}
	go func() {
		if err := c.sendHeartbeat(ctx); err != nil {
			c.logger.Debug("Heartbeat after a refused NATS upgrade failed", "error", err)
		}
	}()
}

// reportBlocked passes the blocked state on with the last version info, the
// way a 426 is reported.
func (c *Client) reportBlocked(ctx context.Context) {
	if c.onVersionChecked == nil {
		return
	}
	var u version.Upstream
	if last := c.lastUpstream.Load(); last != nil {
		u = *last
	}
	c.blockMu.Lock()
	if b := c.block; b != nil {
		u.Blocked = true
		if !b.until.IsZero() {
			u.BlockedUntil = b.until.Unix()
		}
	}
	c.blockMu.Unlock()
	c.onVersionChecked(ctx, u)
}

func formatUntil(until time.Time) string {
	if until.IsZero() {
		return "permanent"
	}
	return until.UTC().Format(time.RFC3339)
}
