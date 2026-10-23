package peeks

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestParse(t *testing.T) {
	if id, err := Parse("peeks://6265400"); err != nil || id != "6265400" {
		t.Errorf("Parse = %q, %v", id, err)
	}
	for _, bad := range []string{"peeks://", "peeks://abc", "fc2live://1", "https://www.peeks.com/streams/stream-view?id=1"} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("Parse(%q) accepted", bad)
		}
	}
	if !IsURL("peeks://1") || IsURL("https://x.m3u8") {
		t.Error("IsURL")
	}
}

const (
	freeLookup = `{"response":"success","stream":{"stream_id":6265400,"live":"Y","audience":"PUBLIC","tip_viewer_min_amount":0}}`
	paidLookup = `{"response":"success","stream":{"stream_id":6265400,"live":"Y","audience":"PUBLIC","tip_viewer_min_amount":60}}`
	endLookup  = `{"response":"success","stream":{"stream_id":6265400,"live":"N","audience":"PUBLIC","tip_viewer_min_amount":0}}`
	paidJoin   = `{"response":"error","invalidFields":"","message":"Access to stream requires payment"}`
)

// grant is a join reply granting a playlist whose token expires at exp.
func grant(exp int64) string {
	return `{"response":"success","url":{"stream_hls":"//wowza-g2.cdn.peeks.com/WToMlqvMLtS2ljV9-Q1nkA==,` +
		strconv.FormatInt(exp, 10) + `/liveapp/amlst:x/playlist.m3u8"}}`
}

// joinReply is a grant good for four hours, as the site's are.
func joinReply() string { return grant(time.Now().Add(4 * time.Hour).Unix()) }

// fakeSite plays the site: the page with its CSRF token, then the lookups
// lookup returns and the join's reply. It counts the leaves.
func fakeSite(t *testing.T, lookup *atomic.Value, join string, leaves *atomic.Int32) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			http.SetCookie(w, &http.Cookie{Name: "PHPSESSID", Value: "s"})
			io.WriteString(w, `<head><meta name="csrf-token" content="tok=="></head>`)
		case "/api/streams/stream-lookup":
			io.WriteString(w, lookup.Load().(string))
		case "/api/streams/stream-join":
			b, _ := io.ReadAll(r.Body)
			if r.Header.Get("X-CSRF-Token") != "tok==" || !strings.Contains(string(b), `"stream_id":6265400`) {
				t.Errorf("join: csrf %q body %s", r.Header.Get("X-CSRF-Token"), b)
			}
			if c, err := r.Cookie("PHPSESSID"); err != nil || c.Value != "s" {
				t.Errorf("join without the session cookie: %v", err)
			}
			io.WriteString(w, join)
		case "/api/streams/leave-stream":
			leaves.Add(1)
			io.WriteString(w, `{"response":"success"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	old, oldWatch := siteOrigin, watchEvery
	siteOrigin, watchEvery = srv.URL, 20*time.Millisecond
	t.Cleanup(func() { siteOrigin, watchEvery = old, oldWatch })
}

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestSeatEndsWhenTheStreamTurnsPaid(t *testing.T) {
	var lookup atomic.Value
	var leaves atomic.Int32
	lookup.Store(freeLookup)
	exp := time.Now().Add(4 * time.Hour).Unix()
	fakeSite(t, &lookup, grant(exp), &leaves)

	s, err := Open(context.Background(), "peeks://6265400", nil, "ua", discard())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if want := "https:" + strings.TrimSuffix(strings.SplitN(grant(exp), `"stream_hls":"`, 2)[1], `"}}`); s.MasterURL != want {
		t.Errorf("MasterURL = %q, want %q", s.MasterURL, want)
	}
	if !s.expires.Equal(time.Unix(exp, 0)) {
		t.Errorf("expires = %v", s.expires)
	}
	select {
	case <-s.Done():
		t.Fatal("seat lost while the stream is free")
	case <-time.After(100 * time.Millisecond):
	}
	lookup.Store(paidLookup)
	select {
	case <-s.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("seat held through a paywall")
	}
	if !errors.Is(s.Err(), ErrPaid) || leaves.Load() != 1 {
		t.Errorf("Err = %v, leaves %d", s.Err(), leaves.Load())
	}
	s.Close()
}

func TestSeatEndsWithTheStream(t *testing.T) {
	var lookup atomic.Value
	var leaves atomic.Int32
	lookup.Store(freeLookup)
	fakeSite(t, &lookup, joinReply(), &leaves)
	s, err := Open(context.Background(), "peeks://6265400", nil, "ua", discard())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	lookup.Store(endLookup)
	<-s.Done()
	if !errors.Is(s.Err(), ErrEnded) {
		t.Errorf("Err = %v", s.Err())
	}
}

func TestOpenRefused(t *testing.T) {
	for name, tc := range map[string]struct {
		lookup, join string
		want         error
	}{
		"paid lookup": {paidLookup, joinReply(), ErrPaid},
		"paid join":   {freeLookup, paidJoin, ErrPaid},
		"ended":       {endLookup, joinReply(), ErrEnded},
		"unknown":     {`{"response":"error","message":"Stream not found"}`, joinReply(), ErrEnded},
	} {
		var lookup atomic.Value
		var leaves atomic.Int32
		lookup.Store(tc.lookup)
		fakeSite(t, &lookup, tc.join, &leaves)
		if _, err := Open(context.Background(), "peeks://6265400", nil, "ua", discard()); !errors.Is(err, tc.want) {
			t.Errorf("%s: Open = %v, want %v", name, err, tc.want)
		}
	}
}

func TestSeatGivesWayBeforeItsTokenExpires(t *testing.T) {
	var lookup atomic.Value
	var leaves atomic.Int32
	lookup.Store(freeLookup)
	soon := time.Now().Add(renewAhead - time.Minute).Unix()
	fakeSite(t, &lookup, grant(soon), &leaves)
	s, err := Open(context.Background(), "peeks://6265400", nil, "ua", discard())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	<-s.Done()
	if !errors.Is(s.Err(), errExpiring) {
		t.Errorf("Err = %v", s.Err())
	}
}
