//go:build live

package asiancammodels

import (
	"context"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"
)

// TestOpen_Network records a live room. ACM_URL is its asiancammodels:// URL;
// ACM_SECONDS bounds the recording (default 30) and ACM_OUT keeps the FLV.
//
//	ACM_URL='asiancammodels://<room>?chatroom=<id>&stream=<hash>' go test -tags live -run TestOpen_Network -v ./internal/asiancammodels/
func TestOpen_Network(t *testing.T) {
	u := os.Getenv("ACM_URL")
	if u == "" {
		t.Skip("ACM_URL not set")
	}
	secs := 30
	if s := os.Getenv("ACM_SECONDS"); s != "" {
		if d, err := time.ParseDuration(s + "s"); err == nil {
			secs = int(d.Seconds())
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(secs)*time.Second)
	defer cancel()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	stream, err := Open(ctx, u, nil, logger)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	out := io.Discard
	if p := os.Getenv("ACM_OUT"); p != "" {
		f, err := os.Create(p)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		out = f
	}
	n, err := io.Copy(out, stream)
	t.Logf("read %d bytes, ended with %v", n, err)
	if n < 100_000 {
		t.Fatalf("only %d bytes", n)
	}
}
