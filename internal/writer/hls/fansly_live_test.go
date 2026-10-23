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
// TestFanslyRecord_Network records FANSLY_ACCOUNT (the account id of a channel
// streaming to anyone) for 40 s through a seat of its own, to FANSLY_OUT or a
// temporary directory.
func TestFanslyRecord_Network(t *testing.T) {
	id := os.Getenv("FANSLY_ACCOUNT")
	if id == "" {
		t.Skip("FANSLY_ACCOUNT not set")
	}
	out := os.Getenv("FANSLY_OUT")
	if out == "" {
		out = t.TempDir()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	err := Record(ctx, Config{
		URL:          "fansly://" + id,
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
	if len(segs) < 10 {
		t.Errorf("only %d segments", len(segs))
	}
}
