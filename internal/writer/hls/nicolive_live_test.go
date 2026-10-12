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

// TestNicoliveRecord_Network records NICONICO_LV (a program on air) for 40 s
// through a seat of its own, to NICONICO_OUT or a temporary directory.
func TestNicoliveRecord_Network(t *testing.T) {
	lv := os.Getenv("NICONICO_LV")
	if lv == "" {
		t.Skip("NICONICO_LV not set")
	}
	out := os.Getenv("NICONICO_OUT")
	if out == "" {
		out = t.TempDir()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	err := Record(ctx, Config{
		URL:          "nicolive://" + lv,
		OutDir:       out,
		PollInterval: time.Second,
		Retries:      3,
		FileName:     "index.m3u8",
	}, &http.Client{Timeout: 30 * time.Second}, logger, make(chan int64, 1024))
	if err != nil && !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Record: %v", err)
	}
	segs, _ := filepath.Glob(filepath.Join(out, "*.m4s"))
	t.Logf("recorded %d segments to %s", len(segs), out)
	if len(segs) < 5 {
		t.Errorf("only %d segments", len(segs))
	}
}
