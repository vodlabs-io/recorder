package flv

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// counter is a refresh func handing out url-1, url-2, … and counting calls.
type counter struct{ n atomic.Int32 }

func (c *counter) refresh(context.Context) (string, error) {
	return fmt.Sprintf("url-%d", c.n.Add(1)), nil
}

func quiet() *slog.Logger { return slog.New(slog.DiscardHandler) }

func TestRenewerWithoutTTLKeepsURL(t *testing.T) {
	var c counter
	r := NewURLRenewer(t.Context(), "url-0", 0, c.refresh, quiet())
	defer r.Close()
	for range 3 {
		if got := r.Next(t.Context()); got != "url-0" {
			t.Fatalf("Next = %q", got)
		}
	}
	if c.n.Load() != 0 {
		t.Errorf("refreshed %d times without a TTL", c.n.Load())
	}
}

// TestRenewerNeverReusesAURL pins that each connection gets a URL no other
// connection has used, fetching one on the spot when none was fetched ahead.
func TestRenewerNeverReusesAURL(t *testing.T) {
	var c counter
	r := NewURLRenewer(t.Context(), "url-0", time.Hour, c.refresh, quiet())
	defer r.Close()
	for i := range 3 {
		if got, want := r.Next(t.Context()), fmt.Sprintf("url-%d", i); got != want {
			t.Fatalf("connection %d: Next = %q, want %q", i, got, want)
		}
	}
}

// TestRenewerFetchesAhead pins that the next URL is fetched in the background
// before the grant runs out, so a reconnect does not wait on a re-resolve.
func TestRenewerFetchesAhead(t *testing.T) {
	var c counter
	r := NewURLRenewer(t.Context(), "url-0", 50*time.Millisecond, c.refresh, quiet())
	defer r.Close()
	r.Next(t.Context())
	deadline := time.Now().Add(time.Second)
	for c.n.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if c.n.Load() != 1 {
		t.Fatalf("no background fetch within the TTL")
	}
	if got := r.Next(t.Context()); got != "url-1" {
		t.Errorf("Next = %q, want the URL fetched ahead", got)
	}
	if c.n.Load() != 1 {
		t.Errorf("refreshed %d times, want the reconnect to use the fetched URL", c.n.Load())
	}
}

// TestRenewerRejectsTheSpentURL pins that a re-resolve answering with the URL
// in use does not count as a fresh one.
func TestRenewerRejectsTheSpentURL(t *testing.T) {
	var calls atomic.Int32
	same := func(context.Context) (string, error) { calls.Add(1); return "url-0", nil }
	r := NewURLRenewer(t.Context(), "url-0", time.Hour, same, quiet())
	defer r.Close()
	r.Next(t.Context())
	if got := r.Next(t.Context()); got != "url-0" {
		t.Errorf("Next = %q", got)
	}
	if calls.Load() < 1 {
		t.Error("no re-resolve was attempted")
	}
}

func TestRenewerFallsBackOnFailure(t *testing.T) {
	fail := func(context.Context) (string, error) { return "", errors.New("offline") }
	r := NewURLRenewer(t.Context(), "url-0", time.Hour, fail, quiet())
	defer r.Close()
	r.Next(t.Context())
	if got := r.Next(t.Context()); got != "url-0" {
		t.Errorf("Next = %q, want the current URL when no fresh one can be had", got)
	}
}

// TestRenewerCloseStopsFetching pins that nothing calls the session's refresh
// func once Close has returned.
func TestRenewerCloseStopsFetching(t *testing.T) {
	var c counter
	r := NewURLRenewer(t.Context(), "url-0", 20*time.Millisecond, c.refresh, quiet())
	r.Next(t.Context())
	r.Close()
	n := c.n.Load()
	time.Sleep(60 * time.Millisecond)
	if c.n.Load() != n {
		t.Errorf("refreshed after Close")
	}
}

// TestWriterRenewsSingleUseURLs records through an Origin whose URLs each
// serve one connection, closed after a slice of the stream — Douyu's shape. A
// writer that reconnected on a spent URL would be refused.
func TestWriterRenewsSingleUseURLs(t *testing.T) {
	slices := [][]byte{
		append(append(append(fileHeader(), vCfg(0, 0x41)...), aCfg(0)...), gop(0, 1000)...),
		append(append(append(fileHeader(), vCfg(1000, 0x41)...), aCfg(1000)...), gop(1000, 2000)...),
		append(append(append(fileHeader(), vCfg(2000, 0x41)...), aCfg(2000)...), gop(2000, 3000)...),
	}

	var mu sync.Mutex
	used := map[string]int{}
	served := make(chan struct{}, len(slices))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok := r.URL.Query().Get("t")
		mu.Lock()
		used[tok]++
		n, again := len(used)-1, used[tok] > 1
		mu.Unlock()
		if again {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		if n >= len(slices) {
			// The broadcast goes quiet until the test ends the part.
			<-r.Context().Done()
			return
		}
		w.Write(slices[n])
		served <- struct{}{}
	}))
	defer srv.Close()

	var minted atomic.Int32
	refresh := func(context.Context) (string, error) {
		return fmt.Sprintf("%s/?t=%d", srv.URL, minted.Add(1)), nil
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() {
		for range slices {
			<-served
		}
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	cfg := Config{
		URL: srv.URL + "/?t=0", OutDir: t.TempDir(), Sanitize: true,
		Retries: 2, ReconnectDelay: time.Millisecond,
		URLTTL: time.Hour, RefreshURL: refresh,
	}
	if err := recordPart(t, ctx, cfg, "pt1.ts"); err != nil {
		t.Fatalf("Record: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	for tok, n := range used {
		if n > 1 {
			t.Errorf("URL t=%s was requested %d times", tok, n)
		}
	}
	if len(used) < len(slices) {
		t.Errorf("served %d slices, want %d", len(used), len(slices))
	}
}
