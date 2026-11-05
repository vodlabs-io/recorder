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

// TestInstagramRecord_Network records INSTAGRAM_URL (an instagram:// stream URL
// from a signed-in check) for 40 s, to INSTAGRAM_OUT or a temporary directory.
func TestInstagramRecord_Network(t *testing.T) {
	u := os.Getenv("INSTAGRAM_URL")
	if u == "" {
		t.Skip("INSTAGRAM_URL not set")
	}
	out := os.Getenv("INSTAGRAM_OUT")
	if out == "" {
		out = t.TempDir()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	err := Record(ctx, Config{
		URL:          u,
		OutDir:       out,
		PollInterval: 2 * time.Second,
		Retries:      3,
		FileName:     "index.m3u8",
	}, &http.Client{Timeout: 30 * time.Second}, logger, make(chan int64, 4096))
	if err != nil && !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Record: %v", err)
	}
	segs, _ := filepath.Glob(filepath.Join(out, "*.m4s"))
	t.Logf("recorded %d segments to %s", len(segs), out)
	if len(segs) < 10 {
		t.Errorf("only %d segments", len(segs))
	}
}
