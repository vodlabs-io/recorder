package vlapi

import (
	"context"
	"time"

	"github.com/nats-io/nats.go/micro"
	"github.com/vodlabs-io/recorder/internal/streamer"
)

// CloudInterval is the estimated polling interval for one platform.
// EstimatedSeconds and UpdatedAt are nil when no estimate is available.
type CloudInterval struct {
	Platform         string `json:"platform"`
	EstimatedSeconds *int   `json:"estimated_seconds"`
	UpdatedAt        *int64 `json:"updated_at"`
}

// CloudIntervals is the daemon's view of the cloud /intervals endpoint.
type CloudIntervals struct {
	TargetSeconds int             `json:"target_seconds"`
	Intervals     []CloudInterval `json:"intervals"`
}

// intervalsFunc fetches the current cloud polling-interval estimates. The daemon
// supplies this (wired to the cloud client) so vlapi stays decoupled from the
// cloud package.
type intervalsFunc func(ctx context.Context) (CloudIntervals, error)

// StreamerFuncs are the cloud round-trips the streamer endpoints need to make
// synchronously, injected so vlapi does not depend on the cloud client. Both
// are nil when no cloud client is wired; handlers then skip the round-trip.
type StreamerFuncs struct {
	// Resolve names a streamer and confirms it exists, returning the canonical
	// platform/username to store. Every error refuses the add; see
	// rejectionCode.
	Resolve func(ctx context.Context, id streamer.StreamerRef) (streamer.StreamerRef, error)
	// Recheck asks the cloud to check a streamer again now and to clear any
	// retirement.
	Recheck func(ctx context.Context, id streamer.StreamerRef) error
	// ResolvePage checks, on this machine, a page no platform claims, and
	// returns the streamer it names while something plays there. The cloud
	// never sees the page. Nil means such pages cannot be recorded.
	ResolvePage func(ctx context.Context, rawURL string) (streamer.StreamerRef, error)
	// ForgetPage drops what ResolvePage kept about a removed streamer.
	ForgetPage func(key streamer.StreamerKey)
}

// handleCloudIntervals returns the estimated per-platform polling intervals,
// served through the cloud client's short-lived cache.
func (s *Service) handleCloudIntervals(req micro.Request) {
	if s.intervalsFn == nil {
		respond(req, fail(errInternal, "intervals provider unavailable"))
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	intervals, err := s.intervalsFn(ctx)
	if err != nil {
		respond(req, fail(errInternal, err.Error()))
		return
	}
	respond(req, OK(intervals))
}
