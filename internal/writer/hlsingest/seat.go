package hlsingest

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/vodlabs-io/recorder/internal/cameraprive"
	"github.com/vodlabs-io/recorder/internal/fansly"
	"github.com/vodlabs-io/recorder/internal/fc2live"
	"github.com/vodlabs-io/recorder/internal/instagram"
	"github.com/vodlabs-io/recorder/internal/nicolive"
	"github.com/vodlabs-io/recorder/internal/peeks"
)

// seat is a viewer's place on a platform that grants its HLS per viewer and
// only while that viewer stays connected.
type seat interface {
	master() string
	Client(base *http.Client) *http.Client
	Done() <-chan struct{}
	Err() error
	Close()
}

type nicoliveSeat struct{ *nicolive.Seat }

func (s nicoliveSeat) master() string { return s.MasterURL }

type fc2liveSeat struct{ *fc2live.Seat }

func (s fc2liveSeat) master() string { return s.MasterURL }

type peeksSeat struct{ *peeks.Seat }

func (s peeksSeat) master() string { return s.MasterURL }

type fanslySeat struct{ *fansly.Seat }

func (s fanslySeat) master() string { return s.MasterURL }

type camerapriveSeat struct{ *cameraprive.Seat }

func (s camerapriveSeat) master() string { return s.MasterURL }

type instagramSeat struct{ *instagram.Seat }

func (s instagramSeat) master() string { return s.MasterURL }

// retained implements retainedSeat: an Instagram broadcast keeps every segment
// until it ends.
func (s instagramSeat) retained() bool { return true }

// retainedSeat is a seat on an Origin that keeps every segment of the Broadcast,
// so a fetch that fails can be tried again at leisure.
type retainedSeat interface{ retained() bool }

// isSeatURL reports whether u names a broadcast recorded through a seat.
func isSeatURL(u string) bool {
	return nicolive.IsURL(u) || fc2live.IsURL(u) || peeks.IsURL(u) || fansly.IsURL(u) ||
		cameraprive.IsURL(u) || instagram.IsURL(u)
}

// openSeat takes a seat on the broadcast u names. login signs a Fansly seat's
// channel reads, or a CameraPrive seat's room reads, in as the recorder's user.
func openSeat(ctx context.Context, u string, client *http.Client, userAgent string, login map[string]string, logger *slog.Logger) (seat, error) {
	if instagram.IsURL(u) {
		s, err := instagram.Open(ctx, u, client, userAgent, logger)
		if err != nil {
			return nil, err
		}
		return instagramSeat{s}, nil
	}
	if cameraprive.IsURL(u) {
		s, err := cameraprive.Open(ctx, u, client, userAgent, login, logger)
		if err != nil {
			return nil, err
		}
		return camerapriveSeat{s}, nil
	}
	if fansly.IsURL(u) {
		s, err := fansly.Open(ctx, u, client, userAgent, login, logger)
		if err != nil {
			return nil, err
		}
		return fanslySeat{s}, nil
	}
	if peeks.IsURL(u) {
		s, err := peeks.Open(ctx, u, client, userAgent, logger)
		if err != nil {
			return nil, err
		}
		return peeksSeat{s}, nil
	}
	if fc2live.IsURL(u) {
		s, err := fc2live.Open(ctx, u, client, userAgent, logger)
		if err != nil {
			return nil, err
		}
		return fc2liveSeat{s}, nil
	}
	s, err := nicolive.Open(ctx, u, client, userAgent, logger)
	if err != nil {
		return nil, err
	}
	return nicoliveSeat{s}, nil
}

// seatEnded reports whether err is the broadcast's end rather than a failure.
// A Peeks broadcast that asks for payment, a Fansly stream closed to the
// viewer, or a CameraPrive performer's paid show or a room that turns every
// seat away, ends the attempt as cleanly.
func seatEnded(err error) bool {
	return errors.Is(err, nicolive.ErrEnded) || errors.Is(err, fc2live.ErrEnded) ||
		errors.Is(err, peeks.ErrEnded) || errors.Is(err, peeks.ErrPaid) ||
		errors.Is(err, fansly.ErrEnded) || errors.Is(err, fansly.ErrLocked) ||
		errors.Is(err, cameraprive.ErrEnded) || errors.Is(err, cameraprive.ErrPaid) ||
		errors.Is(err, cameraprive.ErrLocked) || errors.Is(err, instagram.ErrEnded)
}
