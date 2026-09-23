package cloud

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

const (
	intervalsEndpoint = "/api/v1/intervals"
	intervalsCacheTTL = 60 * time.Second
)

// platformInterval is the estimated polling interval for one platform.
// EstimatedSeconds and UpdatedAt are nil when no estimate is available yet.
type platformInterval struct {
	Platform         string `json:"platform"`
	EstimatedSeconds *int   `json:"estimated_seconds"`
	UpdatedAt        *int64 `json:"updated_at"`
}

// intervalsResponse is the payload of GET /api/v1/intervals.
type intervalsResponse struct {
	TargetSeconds int                `json:"target_seconds"`
	Intervals     []platformInterval `json:"intervals"`
}

// intervalsCache holds the last intervals response for intervalsCacheTTL so that
// repeated UI/CLI requests do not exhaust the cloud API rate limit.
type intervalsCache struct {
	mu      sync.Mutex
	data    *intervalsResponse
	fetched time.Time
}

// GetIntervals returns the estimated per-platform polling intervals, served from
// a short-lived cache. On a fetch error it falls back to the last good response
// if one exists, otherwise it returns the error.
func (c *Client) GetIntervals(ctx context.Context) (*intervalsResponse, error) {
	return c.getIntervals(ctx, c.fetchIntervals)
}

// getIntervals implements the cache policy with an injectable fetch so the
// caching behaviour can be tested without a live cloud endpoint.
func (c *Client) getIntervals(
	ctx context.Context,
	fetch func(context.Context) (*intervalsResponse, error),
) (*intervalsResponse, error) {
	c.intervals.mu.Lock()
	defer c.intervals.mu.Unlock()

	if c.intervals.data != nil && time.Since(c.intervals.fetched) < intervalsCacheTTL {
		return c.intervals.data, nil
	}

	resp, err := fetch(ctx)
	if err != nil {
		if c.intervals.data != nil {
			c.logger.Warn("Using cached intervals after fetch failure", "error", err)
			return c.intervals.data, nil
		}
		return nil, err
	}

	c.intervals.data = resp
	c.intervals.fetched = time.Now()
	return resp, nil
}

// fetchIntervals performs a signed GET against the intervals endpoint.
func (c *Client) fetchIntervals(ctx context.Context) (*intervalsResponse, error) {
	timestamp := time.Now().Unix()
	reqURL := apiBaseURL + intervalsEndpoint

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("X-Timestamp", fmt.Sprintf("%d", timestamp))
	req.Header.Set("X-Client-Version", c.clientVersion)
	req.Header.Set("X-Client-ID", c.clientID)

	if err := c.signRequest(req, nil); err != nil {
		return nil, fmt.Errorf("failed to sign request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("intervals request failed: %w", err)
	}
	defer resp.Body.Close()
	c.noteResponse(ctx, resp)

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("intervals failed: %s - %s", resp.Status, string(body))
	}

	var result intervalsResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}
	return &result, nil
}
