package hlsingest

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/vodlabs-io/recorder/internal/fc2live"
	"github.com/vodlabs-io/recorder/internal/nicolive"
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

// isSeatURL reports whether u names a broadcast recorded through a seat.
func isSeatURL(u string) bool {
	return nicolive.IsURL(u) || fc2live.IsURL(u)
}

// openSeat takes a seat on the broadcast u names.
func openSeat(ctx context.Context, u string, client *http.Client, userAgent string, logger *slog.Logger) (seat, error) {
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
func seatEnded(err error) bool {
	return errors.Is(err, nicolive.ErrEnded) || errors.Is(err, fc2live.ErrEnded)
}
