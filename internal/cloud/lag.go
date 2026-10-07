package cloud

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/vodlabs-io/recorder/internal/streamer"
	"github.com/vodlabs-io/recorder/internal/vlconfig"
)

func (c *Client) handleLagReports(ctx context.Context) {
	log := c.logger.With("func", "handleLagReports")
	log.Info("Started lag reporter")
	defer log.Info("Stopped lag reporter")

	for {
		select {
		case <-ctx.Done():
			return
		case strKey := <-c.LagReportCh:
			log := log.With("streamer", strKey)
			if !strKey.Valid() {
				log.Warn("Received invalid streamer key")
				continue
			}
			if vlconfig.IsLocal(strKey.Platform) {
				// Nothing to tell the cloud: check it again here, afresh.
				select {
				case c.StreamReqCh <- streamer.StreamerRef{StreamerKey: strKey}:
				default:
				}
				continue
			}
			if err := c.reportLag(ctx, strKey); err != nil {
				log.Error("Failed to report lag", "error", err)
			} else {
				log.Info("Reported lag")
			}
		}
	}
}

func (c *Client) reportLag(ctx context.Context, strKey streamer.StreamerKey) error {
	timestamp := time.Now().Unix()
	reqURL := fmt.Sprintf("%s%s/%s/%s/lagging",
		apiBaseURL,
		streamersRoute,
		strKey.Platform,
		strKey.Username)

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, reqURL, nil)
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
		return fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()
	c.noteResponse(ctx, resp)

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status code: %s", resp.Status)
	}

	return nil
}
