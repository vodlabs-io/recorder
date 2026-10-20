package cloud

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"time"

	"github.com/vodlabs-io/recorder/internal/version"
	"github.com/vodlabs-io/recorder/internal/vlconfig"
)

const (
	heartbeatEndpoint = "/api/v1/heartbeat"
	heartbeatInterval = 6 * time.Hour
	// A failed heartbeat is retried from heartbeatRetryMin, doubling up to
	// heartbeatRetryMax, so the daemon isn't left without upstream version info
	// until the next interval.
	heartbeatRetryMin = 30 * time.Second
	heartbeatRetryMax = 30 * time.Minute
)

// heartbeatResponse is the heartbeat endpoint payload: version info plus the
// current supported-platform list.
type heartbeatResponse struct {
	version.Upstream
	SupportedPlatforms []vlconfig.Platform `json:"supported_platforms"`
}

// handleHeartbeat sends a heartbeat now and every heartbeatInterval after,
// retrying failures with backoff.
func (c *Client) handleHeartbeat(ctx context.Context) {
	log := c.logger.With("func", "handleHeartbeat")
	log.Info("Started heartbeat")
	defer log.Info("Stopped Heartbeat")

	retry := heartbeatRetryMin
	timer := time.NewTimer(0)
	defer timer.Stop()

	for {
		select {
		case <-timer.C:
			next := heartbeatInterval
			if err := c.sendHeartbeat(ctx); err != nil {
				log.Error("Failed to send heartbeat", "error", err, "retry_in", retry)
				next = retry
				retry = min(retry*2, heartbeatRetryMax)
			} else {
				retry = heartbeatRetryMin
			}
			timer.Reset(next)
		case <-ctx.Done():
			return
		}
	}
}

func (c *Client) sendHeartbeat(ctx context.Context) error {
	log := c.logger.With("func", "sendHeartbeat")

	timestamp := time.Now().Unix()
	reqURL := apiBaseURL + heartbeatEndpoint

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("X-Timestamp", fmt.Sprintf("%d", timestamp))
	c.setClientHeaders(req)
	req.Header.Set("X-Client-ID", c.clientID)

	if err := c.signRequest(req, nil); err != nil {
		return fmt.Errorf("failed to sign request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("heartbeat request failed: %w", err)
	}
	defer resp.Body.Close()
	c.noteResponse(ctx, resp)

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("heartbeat failed: %s - %s", resp.Status, string(body))
	}

	var result heartbeatResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return fmt.Errorf("failed to parse response: %w", err)
	}
	c.applyPlatforms(result.SupportedPlatforms)
	c.lastUpstream.Store(&result.Upstream)
	if c.onVersionChecked != nil {
		c.onVersionChecked(ctx, result.Upstream)
	}
	log.Info("Heartbeat sent")

	return nil
}

// applyPlatforms updates the supported-platform list from a heartbeat response.
// It ignores an empty/unchanged list and persists the new list to the cache.
//
// Only a change to WHICH platforms are supported resubscribes. The rest of a
// platform's metadata (label, rate limits) is read where it is used.
func (c *Client) applyPlatforms(p []vlconfig.Platform) {
	before := vlconfig.PlatformNames()
	if len(p) == 0 || !vlconfig.SetPlatforms(p) {
		return
	}
	if err := vlconfig.SavePlatformsCache(p); err != nil {
		c.logger.Warn("Failed to save platforms cache", "error", err)
	}
	after := vlconfig.PlatformNames()
	c.logger.Info("Supported platforms updated", "platforms", after)
	if !slices.Equal(before, after) {
		c.triggerResubscribe()
	}
}
