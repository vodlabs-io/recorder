//go:build live

package fmp4

import (
	"context"
	"io"
	"net/http"
	"os"
	"strconv"
	"testing"
	"time"
)

// TestLiveRemux reads FMP4_URL (a progressive fragmented MP4) for
// FMP4_SECONDS and writes it as FLV to FMP4_OUT.
func TestLiveRemux(t *testing.T) {
	u, out := os.Getenv("FMP4_URL"), os.Getenv("FMP4_OUT")
	if u == "" || out == "" {
		t.Skip("FMP4_URL and FMP4_OUT not set")
	}
	secs, _ := strconv.Atoi(os.Getenv("FMP4_SECONDS"))
	if secs <= 0 {
		secs = 30
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(secs)*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	stream := Sniff(resp.Body)
	defer stream.Close()
	f, err := os.Create(out)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	n, err := io.Copy(f, stream)
	t.Logf("wrote %d bytes of FLV (%v)", n, err)
	if n == 0 {
		t.Fatal("no FLV written")
	}
}
