package joystick

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestParse(t *testing.T) {
	p, err := Parse("joystick://tmiah?pb=https%3A%2F%2Fe1.use1.live.joystick.tv%2Flive%2Ftmiah&mode=follower")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if p.username != "tmiah" {
		t.Errorf("username = %q", p.username)
	}
	if p.playbackBase != "https://e1.use1.live.joystick.tv/live/tmiah" {
		t.Errorf("playbackBase = %q", p.playbackBase)
	}
	if p.audience != "follower" {
		t.Errorf("audience = %q", p.audience)
	}
	if _, err := Parse("joystick://x"); err == nil {
		t.Error("Parse accepted a seat URL with no playback base")
	}
	if IsURL("https://joystick.tv/u/x") {
		t.Error("IsURL matched a profile URL")
	}
}

func TestTokenFromLogin(t *testing.T) {
	// The JWT lives in the "user" cookie's JSON "token" field.
	jwt := "eyJ0eXAiOiJKV1QiLCJhbGciOiJIUzI1NiJ9.payload.sig"
	cookie := `user=%7B%22token%22%3A%22` + jwt + `%22%2C%22tokenCount%22%3A0%7D; other=1`
	got := tokenFromLogin(map[string]string{"Cookie": cookie})
	if got != jwt {
		t.Errorf("tokenFromLogin = %q, want %q", got, jwt)
	}
	if tokenFromLogin(nil) != "" || tokenFromLogin(map[string]string{"Cookie": "foo=bar"}) != "" {
		t.Error("tokenFromLogin should be empty without a user cookie")
	}
}

func TestMintGuestToken(t *testing.T) {
	a, b := mintGuestToken(), mintGuestToken()
	if !strings.HasPrefix(a, "gtx_") {
		t.Errorf("token %q missing gtx_ prefix", a)
	}
	if a == b {
		t.Error("mintGuestToken not random")
	}
}

func TestOpenPublicAndEnd(t *testing.T) {
	var status int = http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("token") == "" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, "#EXTM3U\n")
	}))
	defer srv.Close()

	raw := "joystick://faeaurora?pb=" + srv.URL + "/live/faeaurora"
	seat, err := Open(context.Background(), raw, srv.Client(), "ua", nil, quiet())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer seat.Close()
	if !strings.Contains(seat.MasterURL, "/index.m3u8?token=gtx_") {
		t.Errorf("MasterURL = %q; want a guest token", seat.MasterURL)
	}
}

func TestOpenLockedIsClean(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	raw := "joystick://tmiah?pb=" + srv.URL + "/live/tmiah&mode=follower"
	_, err := Open(context.Background(), raw, srv.Client(), "ua", nil, quiet())
	if !errors.Is(err, ErrLocked) {
		t.Errorf("Open on a 403 channel = %v, want ErrLocked", err)
	}
}
