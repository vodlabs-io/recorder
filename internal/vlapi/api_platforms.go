package vlapi

import (
	"github.com/nats-io/nats.go/micro"
	"github.com/vodlabs-io/recorder/internal/vlconfig"
)

// handlePlatforms returns the daemon's current supported-platform list, which is
// seeded from the embedded default and refreshed at runtime from the cloud
// heartbeat. Clients populate their platform picker from this so the list stays
// in sync with what streamer.add will accept.
func (s *Service) handlePlatforms(req micro.Request) {
	respond(req, OK(vlconfig.Platforms()))
}
