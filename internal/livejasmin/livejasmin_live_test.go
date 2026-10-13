//go:build live

package livejasmin

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/vodlabs-io/recorder/internal/useragent"
)

// TestLiveRecord records a live room through the site:
//
//	LIVEJASMIN_ROOM=<name> LIVEJASMIN_SECONDS=260 LIVEJASMIN_OUT=/tmp/lj.flv \
//	  go test -tags live -run TestLiveRecord -v ./internal/livejasmin/
//
// Recording past two minutes crosses a change of seat.
func TestLiveRecord(t *testing.T) {
	room := os.Getenv("LIVEJASMIN_ROOM")
	if room == "" {
		t.Skip("LIVEJASMIN_ROOM not set")
	}
	secs, _ := strconv.Atoi(os.Getenv("LIVEJASMIN_SECONDS"))
	if secs <= 0 {
		secs = 30
	}
	out := io.Discard
	if p := os.Getenv("LIVEJASMIN_OUT"); p != "" {
		f, err := os.Create(p)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		out = f
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(secs)*time.Second)
	defer cancel()
	stream, err := Open(ctx, Room{Name: room}.URL(), nil, useragent.Fallback, logger)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	n, err := io.Copy(out, stream)
	t.Logf("recorded %d bytes, ended with %v", n, err)
	if n < 1<<20 {
		t.Fatalf("recorded only %d bytes", n)
	}
}
