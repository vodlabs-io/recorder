package cloud

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"testing"

	"github.com/nats-io/nats.go"
)

func TestNATSErrorLevel(t *testing.T) {
	reset := &net.OpError{Op: "read", Net: "tcp", Err: errors.New("connection reset by peer")}
	cases := []struct {
		err  error
		want slog.Level
	}{
		{reset, slog.LevelWarn},
		{fmt.Errorf("ws: %w", io.EOF), slog.LevelWarn},
		{nats.ErrSlowConsumer, slog.LevelError},
		{nats.ErrAuthRevoked, slog.LevelError},
	}
	for _, c := range cases {
		if got := natsErrorLevel(c.err); got != c.want {
			t.Errorf("natsErrorLevel(%v) = %v, want %v", c.err, got, c.want)
		}
	}
}
