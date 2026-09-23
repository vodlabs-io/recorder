package vlapi

import "github.com/nats-io/nats.go/micro"

func (s *Service) handleShutdown(req micro.Request) {
	respond(req, OK("shutdown signaled"))
	select {
	case s.ShutdownCh <- struct{}{}:
	default:
	}
}

func (s *Service) handleForceShutdown(req micro.Request) {
	respond(req, OK("force shutdown signaled"))
	select {
	case s.ShutdownCh <- struct{}{}:
	default:
	}
	select {
	case s.ForceShutdownCh <- struct{}{}:
	default:
	}
}
