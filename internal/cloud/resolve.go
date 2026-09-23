package cloud

import (
	"context"
	"fmt"

	"github.com/vodlabs-io/recorder/internal/streamer"
)

// RecheckStreamer asks the cloud to check a streamer again right now, clearing
// any retirement so it returns to the ordinary polling rotation.
//
// This is the only way out of StatusDefunct, and it is deliberately manual: a
// defunct account is not polled at all, so nothing else will ever ask again.
func (c *Client) RecheckStreamer(ctx context.Context, id streamer.StreamerRef) error {
	str, err := c.fetchStreamerRecheck(ctx, id)
	if err != nil {
		return err
	}
	c.handleStatusUpdate(ctx, str)
	return nil
}

// fetchStreamerRecheck performs the single-streamer status fetch with
// ?recheck=1, which also clears any retirement.
func (c *Client) fetchStreamerRecheck(ctx context.Context, id streamer.StreamerRef) (streamer.StreamerState, error) {
	empty := streamer.StreamerState{}
	if id.StreamerKey.Platform == "" || id.StreamerKey.Username == "" {
		return empty, fmt.Errorf("re-check needs a platform and username")
	}
	list, err := c.fetchStatus(ctx, statusOpts{recheck: true}, id)
	if err != nil {
		return empty, err
	}
	if len(list) == 0 {
		return empty, fmt.Errorf("no streamer found with ID: %s", id)
	}
	return list[0], nil
}
