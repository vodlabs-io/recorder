package agora

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// swagAPI fakes the SWAG endpoints the mint and watch call: the token
// endpoint answers with status (200 or 403), the stream state with state.
func swagAPI(t *testing.T, status int, state string) (*httptest.Server, *[]string) {
	t.Helper()
	var accounts []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/streams/sess1/token":
			accounts = append(accounts, r.Header.Get("X-Session-ID"))
			w.WriteHeader(status)
			if status == http.StatusOK {
				io.WriteString(w, `{"agora_token":"a","agora_token_session_id":"tok","agora_exp":1795478533,"nbf":1795478473,"exp":1795478533}`)
			}
		case r.URL.Path == "/realtime/state" && r.URL.Query().Get("channels") == "private-stream-rt@user1":
			if r.Header.Get("X-Client-ID") == "" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			io.WriteString(w, `{"channels":{"private-stream-rt@user1":{"state":{"session":`+state+`}}}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &accounts
}

const (
	swagFree     = `{"sessionId":"sess1","mode":"free"}`
	swagShow     = `{"sessionId":"sess1","mode":"show"}`
	swagNext     = `{"sessionId":"sess2","mode":"free"}`
	swagOffline  = `null`
	swagChannel  = "agora://app/sess1?mint=swag&room=sess1&user=user1"
	swagTestBase = "/"
)

func withSwag(t *testing.T, srv *httptest.Server) {
	t.Helper()
	old := swag.base
	swag.base = srv.URL + swagTestBase
	t.Cleanup(func() { swag.base = old })
}

func TestSwagMintTakesItsOwnAccount(t *testing.T) {
	srv, accounts := swagAPI(t, http.StatusOK, swagFree)
	withSwag(t, srv)
	ch, _ := Parse(swagChannel)
	a, err := mint(context.Background(), srv.Client(), ch)
	if err != nil {
		t.Fatal(err)
	}
	b, err := mint(context.Background(), srv.Client(), ch)
	if err != nil {
		t.Fatal(err)
	}
	if a.Token != "tok" || a.UID != 0 || a.Expires.Unix() != 1795478533 {
		t.Fatalf("minted %+v", a)
	}
	if len(a.Account) != 24 || a.Account == b.Account {
		t.Fatalf("accounts %q and %q: want two distinct 24-hex accounts", a.Account, b.Account)
	}
	if (*accounts)[0] != a.Account {
		t.Fatalf("token asked for %q, minted %q", (*accounts)[0], a.Account)
	}
	token, _, err := swag.renew(context.Background(), srv.Client(), a)
	if err != nil || token != "tok" || (*accounts)[2] != a.Account {
		t.Fatalf("renew: %q, %v; account %q", token, err, (*accounts)[2])
	}
}

func TestSwagMintRefusesAWithheldStream(t *testing.T) {
	for name, c := range map[string]struct{ state, reason string }{
		"paid show": {swagShow, "show"},
		"offline":   {swagOffline, "finished"},
		"restarted": {swagNext, "finished"},
	} {
		t.Run(name, func(t *testing.T) {
			srv, _ := swagAPI(t, http.StatusForbidden, c.state)
			withSwag(t, srv)
			ch, _ := Parse(swagChannel)
			_, err := mint(context.Background(), srv.Client(), ch)
			var refused *RefusedError
			if !errors.As(err, &refused) || refused.Reason != c.reason {
				t.Fatalf("err = %v, want refused for %q", err, c.reason)
			}
		})
	}
}

func TestSwagMintSignsInAndTrustsTheTokenEndpoint(t *testing.T) {
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/streams/sess1/token":
			auth = r.Header.Get("Authorization")
			if auth == "" {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			io.WriteString(w, `{"agora_token_session_id":"tok","agora_exp":1795478533}`)
		case "/realtime/state":
			io.WriteString(w, `{"channels":{"private-stream-rt@user1":{"state":{"session":`+swagShow+`}}}}`)
		}
	}))
	t.Cleanup(srv.Close)
	withSwag(t, srv)
	ch, _ := Parse(swagChannel)
	ch.login = &seatLogin{headers: map[string]string{"Authorization": "Bearer jwt"}}
	got, err := mint(context.Background(), srv.Client(), ch)
	if err != nil || got.Token != "tok" || auth != "Bearer jwt" {
		t.Fatalf("signed-in mint of a paid show: %+v, %v (auth %q)", got, err, auth)
	}
	// A paid show is the token endpoint's call, not the stream mode's.
	if reason := swag.withheld(context.Background(), srv.Client(), ch); reason != "" {
		t.Fatalf("withheld %q for a signed-in viewer", reason)
	}
}

func TestSwagSignedInRefusalNamesUnpaid(t *testing.T) {
	srv, _ := swagAPI(t, http.StatusForbidden, swagShow)
	withSwag(t, srv)
	ch, _ := Parse(swagChannel)
	ch.login = &seatLogin{headers: map[string]string{"Authorization": "Bearer jwt"}}
	_, err := mint(context.Background(), srv.Client(), ch)
	var refused *RefusedError
	if !errors.As(err, &refused) || refused.Reason != "not paid for" {
		t.Fatalf("err = %v", err)
	}
}

func TestSwagWatch(t *testing.T) {
	for name, c := range map[string]struct {
		state string
		want  func(error) bool
	}{
		"ended": {swagOffline, func(err error) bool { return errors.Is(err, ErrChannelEnded) }},
		"show": {swagShow, func(err error) bool {
			var r *RefusedError
			return errors.As(err, &r) && r.Reason == "show"
		}},
	} {
		t.Run(name, func(t *testing.T) {
			srv, _ := swagAPI(t, http.StatusOK, c.state)
			withSwag(t, srv)
			ch, _ := Parse(swagChannel)
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if err := swag.watch(ctx, srv.Client(), ch, 10*time.Millisecond); !c.want(err) {
				t.Fatalf("watch = %v", err)
			}
		})
	}
}

func TestRenewerOnlyForExpiringSwagTokens(t *testing.T) {
	ch, _ := Parse(swagChannel)
	if renewer(http.DefaultClient, ch) != nil {
		t.Fatal("renewer for a token with no expiry")
	}
	ch.Expires = time.Now().Add(time.Minute)
	if renewer(http.DefaultClient, ch) == nil {
		t.Fatal("no renewer for a SWAG token")
	}
	ch.Mint = "manyvids"
	if renewer(http.DefaultClient, ch) != nil {
		t.Fatal("renewer for ManyVids")
	}
}

func TestRenewDelay(t *testing.T) {
	if d := renewDelay(time.Now().Add(time.Minute)); d < 39*time.Second || d > 40*time.Second {
		t.Fatalf("delay %s for a minute-long token", d)
	}
	if d := renewDelay(time.Now()); d != time.Second {
		t.Fatalf("delay %s for an expired token", d)
	}
}

func TestChannelAccountRoundTrip(t *testing.T) {
	ch, err := Parse("agora://app/chan?token=t&account=abc&mint=swag&room=r&user=u")
	if err != nil || ch.Account != "abc" {
		t.Fatalf("parse: %+v, %v", ch, err)
	}
	if again, _ := Parse(ch.URL()); again.Account != "abc" || !strings.Contains(ch.URL(), "account=abc") {
		t.Fatalf("round trip lost the account: %s", ch.URL())
	}
}

func TestBroadcasterNamedByAccountLeaves(t *testing.T) {
	s := &session{logger: slog.New(slog.DiscardHandler)}
	add := notification{Type: "on_add_video_stream", Message: []byte(`{"uid":"gz8fYELRXJ4TicGZ","uint_id":1069681488,"pt":102}`)}
	if err := s.onNotification(add); err != nil || s.publisher.Load() != 1069681488 {
		t.Fatalf("add: %v, publisher %d", err, s.publisher.Load())
	}
	left := notification{Type: "on_user_offline", Message: []byte(`{"uid":"gz8fYELRXJ4TicGZ","uint_id":1069681488,"reason":0}`)}
	if err := s.onNotification(left); !errors.Is(err, ErrChannelEnded) {
		t.Fatalf("offline: %v", err)
	}
}

// TestSwagLive_Network records a live SWAG stream past its first token's
// expiry. SWAG_LIVE_ROOM is a free stream's session id, SWAG_LIVE_USER its
// streamer's id.
func TestSwagLive_Network(t *testing.T) {
	room, user := os.Getenv("SWAG_LIVE_ROOM"), os.Getenv("SWAG_LIVE_USER")
	if room == "" || user == "" {
		t.Skip("SWAG_LIVE_ROOM and SWAG_LIVE_USER not set")
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	u := Channel{AppID: "19c9ed8fd65f4ea9b5de096362af989e", Name: room, Mint: "swag", Room: room, User: user}.URL()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	r, err := Open(ctx, u, nil, logger)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	start := time.Now()
	var total, late int64
	buf := make([]byte, 64<<10)
	for time.Since(start) < 100*time.Second {
		n, err := r.Read(buf)
		total += int64(n)
		if time.Since(start) > 75*time.Second {
			late += int64(n)
		}
		if err != nil {
			t.Fatalf("after %s and %d bytes: %v", time.Since(start).Round(time.Second), total, err)
		}
	}
	t.Logf("%d bytes in %s, %d after the first token expired", total, time.Since(start).Round(time.Second), late)
	if late == 0 {
		t.Fatal("no media after the first token expired")
	}
}

func TestSwagRejectedLoginIsBadLogin(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		io.WriteString(w, `{"code":"AUTH_ERROR","message":"Signature verification failed"}`)
	}))
	t.Cleanup(srv.Close)
	withSwag(t, srv)
	ch, _ := Parse(swagChannel)
	ch.login = &seatLogin{headers: map[string]string{"Authorization": "Bearer stale"}}
	_, err := mint(context.Background(), srv.Client(), ch)
	if !errors.Is(err, ErrBadLogin) {
		t.Fatalf("err = %v", err)
	}
}

func TestSwagRejectedLoginFallsBackToSignedOut(t *testing.T) {
	var signed, unsigned int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			signed++
			w.WriteHeader(http.StatusInternalServerError)
			io.WriteString(w, `{"code":"AUTH_ERROR","message":"Signature has expired"}`)
			return
		}
		unsigned++
		io.WriteString(w, `{"agora_token_session_id":"tok","agora_exp":1795478533}`)
	}))
	t.Cleanup(srv.Close)
	withSwag(t, srv)
	ch, _ := Parse(swagChannel)
	ch.login = &seatLogin{headers: map[string]string{"Authorization": "Bearer stale"}}
	minted, err := mint(context.Background(), srv.Client(), ch)
	if err != nil {
		t.Fatal(err)
	}
	if minted.Token != "tok" || signed != 1 || unsigned != 1 {
		t.Fatalf("token %q after %d signed and %d unsigned requests", minted.Token, signed, unsigned)
	}
}

func TestOpenFailsWithTheMintErrorWhenTheChannelHasNoToken(t *testing.T) {
	srv, _ := swagAPI(t, http.StatusInternalServerError, swagFree)
	withSwag(t, srv)
	_, err := Open(context.Background(), swagChannel, srv.Client(), slog.New(slog.DiscardHandler))
	if err == nil || !strings.Contains(err.Error(), "swag token: status 500") {
		t.Fatalf("err = %v", err)
	}
}

func TestOpenFallsBackToTheChannelsOwnToken(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // fails the mint, and then the join it falls back to
	_, err := Open(ctx, swagChannel+"&token=shared", nil, slog.New(slog.DiscardHandler))
	if err == nil || strings.Contains(err.Error(), "swag token") {
		t.Fatalf("err = %v", err)
	}
}

func TestOpenSignedRejectsALoginNoRequestCanCarry(t *testing.T) {
	login := map[string]string{"Authorization": "Bearer a\nb"}
	_, err := OpenSigned(context.Background(), swagChannel, nil, login, slog.New(slog.DiscardHandler))
	if !errors.Is(err, ErrBadLogin) {
		t.Fatalf("err = %v", err)
	}
}
