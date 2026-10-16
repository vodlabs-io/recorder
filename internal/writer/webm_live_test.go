//go:build live

package writer

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"testing"
	"time"
)

// TestWebMWriter_Network records AGORA_URL (a VP8 agora:// channel, such as
// agora://669058195ead4efaa9509c9a400de392/x?mint=manyvids&room=<id>&codec=vp8
// for a ManyVids room) through the webmwriter for 60 s, into WEBM_OUT or a
// temporary directory.
func TestWebMWriter_Network(t *testing.T) {
	u := os.Getenv("AGORA_URL")
	if u == "" {
		t.Skip("AGORA_URL not set")
	}
	out := os.Getenv("WEBM_OUT")
	if out == "" {
		out = t.TempDir()
	}
	w, _ := Get("webmwriter")
	part, err := w.PreparePart(out, "pt1_best")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	carry := NewCarryover()
	defer carry.Close()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	cfg := &Config{StreamURL: u, Retries: 2, Sanitize: true, Carryover: carry,
		OnConfigKey: func(key string) { t.Logf("config key %s", key) }}
	err = w.Record(ctx, part, cfg, &http.Client{}, logger, make(chan int64, 1<<16))
	t.Logf("Record: %v", err)
	fi, err := os.Stat(part.Path)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("wrote %d bytes to %s", fi.Size(), part.Path)
	if fi.Size() < 100_000 {
		t.Errorf("only %d bytes", fi.Size())
	}
}
