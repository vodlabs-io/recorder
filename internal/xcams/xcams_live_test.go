//go:build live

package xcams_test

import (
	"context"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/vodlabs-io/recorder/internal/xcams"
)

// TestOpen_Network records a live room. XCAMS_URL is its xcams:// URL;
// XCAMS_SECONDS bounds the recording (default 30) and XCAMS_OUT keeps the FLV.
//
//	XCAMS_URL='xcams://<account>?nick=<nick>' go test -tags live -run TestOpen_Network -v ./internal/xcams/
func TestOpen_Network(t *testing.T) {
	u := os.Getenv("XCAMS_URL")
	if u == "" {
		t.Skip("XCAMS_URL not set")
	}
	secs := 30
	if s := os.Getenv("XCAMS_SECONDS"); s != "" {
		if d, err := time.ParseDuration(s + "s"); err == nil {
			secs = int(d.Seconds())
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(secs)*time.Second)
	defer cancel()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	stream, err := xcams.Open(ctx, u, logger)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	out := io.Discard
	if p := os.Getenv("XCAMS_OUT"); p != "" {
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
