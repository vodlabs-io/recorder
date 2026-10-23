//go:build live

package hls

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// TestCameraPriveRecord_Network records CAMERAPRIVE_ROOM (the slug of a
// performer in free chat) through guest seats of its own for
// CAMERAPRIVE_SECONDS (default 220, past the first seat's three minutes), to
// CAMERAPRIVE_OUT or a temporary directory.
func TestCameraPriveRecord_Network(t *testing.T) {
	slug := os.Getenv("CAMERAPRIVE_ROOM")
	if slug == "" {
		t.Skip("CAMERAPRIVE_ROOM not set")
	}
	secs, _ := strconv.Atoi(os.Getenv("CAMERAPRIVE_SECONDS"))
	if secs <= 0 {
		secs = 220
	}
	out := os.Getenv("CAMERAPRIVE_OUT")
	if out == "" {
		out = t.TempDir()
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(secs)*time.Second)
	defer cancel()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	err := Record(ctx, Config{
		URL:          "cameraprive://" + slug,
		OutDir:       out,
		PollInterval: 2 * time.Second,
		Retries:      3,
		FileName:     "index.m3u8",
	}, &http.Client{Timeout: 30 * time.Second}, logger, make(chan int64, 4096))
	if err != nil && !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Record: %v", err)
	}
	segs, _ := filepath.Glob(filepath.Join(out, "*.m4s"))
	if len(segs) == 0 {
		segs, _ = filepath.Glob(filepath.Join(out, "*.ts"))
	}
	t.Logf("recorded %d segments to %s", len(segs), out)
	if want := secs / 3; len(segs) < want {
		t.Errorf("only %d segments, want at least %d", len(segs), want)
	}
}
