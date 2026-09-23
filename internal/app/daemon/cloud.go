package daemon

import (
	"context"

	"github.com/vodlabs-io/recorder/internal/streamer"
	"github.com/vodlabs-io/recorder/internal/vlapi"
)

// cloudIntervals adapts the cloud client's intervals response to the vlapi type,
// keeping vlapi decoupled from the cloud package. It backs the
// "cloud.intervals" RPC endpoint.
func (a *App) cloudIntervals(ctx context.Context) (vlapi.CloudIntervals, error) {
	resp, err := a.cloudClient.GetIntervals(ctx)
	if err != nil {
		return vlapi.CloudIntervals{}, err
	}
	out := vlapi.CloudIntervals{
		TargetSeconds: resp.TargetSeconds,
		Intervals:     make([]vlapi.CloudInterval, len(resp.Intervals)),
	}
	for i, pi := range resp.Intervals {
		out.Intervals[i] = vlapi.CloudInterval{
			Platform:         pi.Platform,
			EstimatedSeconds: pi.EstimatedSeconds,
			UpdatedAt:        pi.UpdatedAt,
		}
	}
	return out, nil
}

// cloudStreamerFuncs wires the synchronous streamer round-trips vlapi needs:
// naming a streamer and confirming it exists before an add is accepted, and
// re-checking a retired one on request.
func (a *App) cloudStreamerFuncs() vlapi.StreamerFuncs {
	return vlapi.StreamerFuncs{
		Resolve: func(ctx context.Context, id streamer.StreamerRef) (streamer.StreamerRef, error) {
			return a.cloudClient.ResolveStreamer(ctx, id)
		},
		Recheck: func(ctx context.Context, id streamer.StreamerRef) error {
			return a.cloudClient.RecheckStreamer(ctx, id)
		},
	}
}
