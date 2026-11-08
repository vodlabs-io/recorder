package session

import (
	"errors"
	"fmt"
	"log/slog"
	"testing"

	"github.com/vodlabs-io/recorder/internal/streamer"
	"github.com/vodlabs-io/recorder/internal/vlerror"
)

func TestSessionFailureLevel(t *testing.T) {
	cases := []struct {
		err    error
		status streamer.LiveStatus
		want   slog.Level
	}{
		{errors.New("stream is restricted"), streamer.StatusRestricted, slog.LevelWarn},
		{errors.New("streamer account is unavailable"), streamer.StatusDefunct, slog.LevelWarn},
		{errors.New("status is unknown"), streamer.StatusUnknown, slog.LevelError},
		{fmt.Errorf("part: %w", vlerror.ErrWriteFailed), streamer.StatusRestricted, slog.LevelError},
	}
	for _, c := range cases {
		if got := sessionFailureLevel(c.err, c.status); got != c.want {
			t.Errorf("sessionFailureLevel(%v, %s) = %v, want %v", c.err, c.status, got, c.want)
		}
	}
}
