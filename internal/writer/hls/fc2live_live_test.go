//go:build live

package hls

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestFC2LiveRecord_Network records FC2LIVE_CHANNEL (a free channel on air)
// for 40 s through a seat of its own, to FC2LIVE_OUT or a temporary
// directory.
func TestFC2LiveRecord_Network(t *testing.T) {
	id := os.Getenv("FC2LIVE_CHANNEL")
	if id == "" {
		t.Skip("FC2LIVE_CHANNEL not set")
	}
	out := os.Getenv("FC2LIVE_OUT")
	if out == "" {
		out = t.TempDir()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	err := Record(ctx, Config{
		URL:          "fc2live://" + id,
		OutDir:       out,
		PollInterval: time.Second,
		Retries:      3,
		FileName:     "index.m3u8",
	}, &http.Client{Timeout: 30 * time.Second}, logger, make(chan int64, 1024))
	if err != nil && !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Record: %v", err)
	}
	segs, _ := filepath.Glob(filepath.Join(out, "*.ts"))
	t.Logf("recorded %d segments to %s", len(segs), out)
	if len(segs) < 5 {
		t.Errorf("only %d segments", len(segs))
	}
}
