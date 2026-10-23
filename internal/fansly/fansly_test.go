package fansly

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestParse(t *testing.T) {
	if id, err := Parse("fansly://645676306893123584"); err != nil || id != "645676306893123584" {
		t.Errorf("Parse = %q, %v", id, err)
	}
	for _, bad := range []string{"fansly://", "fansly://abc", "peeks://1", "https://fansly.com/live/x"} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("Parse(%q) accepted", bad)
		}
	}
	if !IsURL("fansly://1") || IsURL("https://x.m3u8") {
		t.Error("IsURL")
	}
}

const (
	playback = "https://8e69c661e048.us-east-1.playback.live-video.net/api/video/v1/us-east-1.862541535858.channel.FuEbk7fjyv65.m3u8?token=eyJ.eyJ.sig"
	open     = `{"success":true,"response":{"accountId":"645676306893123584","stream":{"status":2,"access":true,"playbackUrl":"` + playback + `"}}}`
	locked   = `{"success":true,"response":{"accountId":"645676306893123584","stream":{"status":2,"access":false}}}`
	ended    = `{"success":true,"response":{"accountId":"645676306893123584","stream":{"status":1,"access":false}}}`
	refused  = `{"success": false,"error": {"code": 1,"details": "invalid login"}}`
)

// fakeAPI answers the channel read with whatever channel holds, and records
// the Authorization header of the last read.
func fakeAPI(t *testing.T, channel *atomic.Value, auth *atomic.Value) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/streaming/channel/645676306893123584" {
			http.NotFound(w, r)
			return
		}
		auth.Store(r.Header.Get("Authorization"))
		io.WriteString(w, channel.Load().(string))
	}))
	t.Cleanup(srv.Close)
	old, oldWatch := apiOrigin, watchEvery
	apiOrigin, watchEvery = srv.URL, 20*time.Millisecond
	t.Cleanup(func() { apiOrigin, watchEvery = old, oldWatch })
}

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func waitDone(t *testing.T, s *Seat) {
	t.Helper()
	select {
	case <-s.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("seat still held")
	}
}

func TestSeatEndsWhenTheStreamIsLocked(t *testing.T) {
	var channel, auth atomic.Value
	channel.Store(open)
	fakeAPI(t, &channel, &auth)

	s, err := Open(context.Background(), "fansly://645676306893123584", nil, "ua", map[string]string{"Authorization": "tok"}, discard())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if s.MasterURL != playback || auth.Load() != "tok" {
		t.Fatalf("master %q, auth %v", s.MasterURL, auth.Load())
	}
	time.Sleep(60 * time.Millisecond)
	if s.Err() != nil {
		t.Fatalf("seat lost while the stream is open: %v", s.Err())
	}
	channel.Store(locked)
	waitDone(t, s)
	if !errors.Is(s.Err(), ErrLocked) {
		t.Errorf("Err = %v", s.Err())
	}
}

func TestSeatEndsWithTheStream(t *testing.T) {
	var channel, auth atomic.Value
	channel.Store(open)
	fakeAPI(t, &channel, &auth)

	s, err := Open(context.Background(), "fansly://645676306893123584", nil, "ua", nil, discard())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if auth.Load() != "" {
		t.Errorf("anonymous read sent Authorization %v", auth.Load())
	}
	// A read the API refuses says nothing about the stream.
	channel.Store(refused)
	time.Sleep(60 * time.Millisecond)
	if s.Err() != nil {
		t.Fatalf("seat lost on a refused read: %v", s.Err())
	}
	channel.Store(ended)
	waitDone(t, s)
	if !errors.Is(s.Err(), ErrEnded) {
		t.Errorf("Err = %v", s.Err())
	}
}

func TestOpenRefusesAStreamItCannotWatch(t *testing.T) {
	var channel, auth atomic.Value
	fakeAPI(t, &channel, &auth)
	for body, want := range map[string]error{locked: ErrLocked, ended: ErrEnded, `{"success":true,"response":null}`: ErrEnded} {
		channel.Store(body)
		if _, err := Open(context.Background(), "fansly://645676306893123584", nil, "ua", nil, discard()); !errors.Is(err, want) {
			t.Errorf("%s: err = %v, want %v", body, err, want)
		}
	}
	channel.Store(`{"success":true,"response":{"accountId":"645676306893123584","stream":{"status":2,"access":true,"playbackUrl":"https://evil.test/x.m3u8?token=a"}}}`)
	if _, err := Open(context.Background(), "fansly://645676306893123584", nil, "ua", nil, discard()); err == nil {
		t.Error("a playlist off IVS was taken")
	}
}
