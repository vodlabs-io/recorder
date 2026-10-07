package cloud

import (
	"context"
	"errors"

	"github.com/vodlabs-io/recorder/internal/streamer"
	"github.com/vodlabs-io/recorder/internal/vlconfig"
)

// LocalSource checks the streamers of local platforms (vlconfig.IsLocal),
// which this machine resolves itself. Their identities and addresses are never
// sent to the service: they are left out of every subscribe, status request
// and lag report, and their checks go to the LocalSource instead, through the
// same per-platform lanes and dispatch as any other.
type LocalSource interface {
	// Check reports what the streamer is broadcasting now. fresh asks for a
	// new reading rather than a recent one, as a re-sign does.
	Check(ctx context.Context, key streamer.StreamerKey, fresh bool) (streamer.StreamerState, error)
}

// errNoLocalSource is a check of a local streamer on a client given none.
var errNoLocalSource = errors.New("no local source for this platform")

// isLocal reports whether id belongs to a local platform.
func isLocal(id streamer.StreamerRef) bool {
	return id.StreamerKey.Valid() && vlconfig.IsLocal(id.Platform)
}

// splitLocal separates the streamers this machine resolves itself from those
// the service is asked about.
func splitLocal(ids streamer.StreamerRefs) (local, remote streamer.StreamerRefs) {
	for _, id := range ids {
		if isLocal(id) {
			if !local.Contains(id) {
				local = append(local, id)
			}
		} else {
			remote = append(remote, id)
		}
	}
	return local, remote
}

// checkLocal runs a local streamer's check.
func (c *Client) checkLocal(ctx context.Context, id streamer.StreamerRef, fresh bool) (streamer.StreamerState, error) {
	if c.local == nil {
		return streamer.StreamerState{}, errNoLocalSource
	}
	return c.local.Check(ctx, id.StreamerKey, fresh)
}
