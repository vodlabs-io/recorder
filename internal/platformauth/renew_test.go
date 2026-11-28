package platformauth

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

// jwtExpiring returns an unsigned JWT whose exp is at.
func jwtExpiring(at time.Time) string {
	claims := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d}`, at.Unix())))
	return "e30." + claims + ".sig"
}

// swagRenewal fakes SWAG's token renewal, answering with status, and returns
// a client that sends every request to it.
func swagRenewal(t *testing.T, status int, fresh string) (*http.Client, *int) {
	t.Helper()
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != http.MethodPost || r.URL.Path != "/auth/tokens" ||
			r.Header.Get("Authorization") != "Bearer refresh" || r.ContentLength != 0 {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		w.WriteHeader(status)
		io.WriteString(w, `{"access_token":"`+fresh+`","refresh_token":"refresh"}`)
	}))
	t.Cleanup(srv.Close)
	target, _ := url.Parse(srv.URL)
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "api.swag.live" {
			return nil, fmt.Errorf("request to %s", r.URL.Host)
		}
		r.URL.Scheme, r.URL.Host = target.Scheme, target.Host
		return http.DefaultTransport.RoundTrip(r)
	})}
	return client, &calls
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func storeSwag(t *testing.T, access string) *Store {
	t.Helper()
	s := testStore(t)
	err := s.Set(Session{Platform: "swag", CapturedAt: 1000, Healthy: true, Cookies: []Cookie{
		{Name: "vl_access_token", Value: access, Domain: ".swag.live"},
		{Name: "vl_refresh_token", Value: "refresh", Domain: ".swag.live"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestFreshRenewsAnExpiredToken(t *testing.T) {
	fresh := jwtExpiring(time.Now().Add(time.Hour))
	client, calls := swagRenewal(t, http.StatusOK, fresh)
	s := storeSwag(t, jwtExpiring(time.Now().Add(-time.Minute)))

	sess, ok, err := s.Fresh(context.Background(), client, "swag")
	if !ok || err != nil {
		t.Fatalf("ok %v, err %v", ok, err)
	}
	if got := sess.cookie("vl_access_token"); got != fresh {
		t.Fatalf("returned token %q", got)
	}
	stored, _ := s.Get("swag")
	if stored.cookie("vl_access_token") != fresh || stored.CapturedAt != 1000 {
		t.Fatalf("stored %+v", stored)
	}
	// The stored token is now fresh, so asking again renews nothing.
	if _, _, err := s.Fresh(context.Background(), client, "swag"); err != nil || *calls != 1 {
		t.Fatalf("err %v after %d renewals", err, *calls)
	}
}

func TestFreshKeepsAValidToken(t *testing.T) {
	client, calls := swagRenewal(t, http.StatusOK, "unused")
	valid := jwtExpiring(time.Now().Add(30 * time.Minute))
	s := storeSwag(t, valid)

	sess, ok, err := s.Fresh(context.Background(), client, "swag")
	if !ok || err != nil || sess.cookie("vl_access_token") != valid || *calls != 0 {
		t.Fatalf("ok %v, err %v, %d renewals", ok, err, *calls)
	}
}

func TestFreshRefusedRenewalMarksTheLoginUnhealthy(t *testing.T) {
	client, _ := swagRenewal(t, http.StatusUnauthorized, "")
	expired := jwtExpiring(time.Now().Add(-time.Minute))
	s := storeSwag(t, expired)

	sess, ok, err := s.Fresh(context.Background(), client, "swag")
	if !ok || !errors.Is(err, ErrRenewalRefused) || sess.cookie("vl_access_token") != expired {
		t.Fatalf("ok %v, err %v", ok, err)
	}
	if stored, _ := s.Get("swag"); stored.Healthy {
		t.Fatal("login still healthy")
	}
}

func TestFreshLeavesPlatformsWithoutRenewal(t *testing.T) {
	s := testStore(t)
	s.Set(Session{Platform: "tiktok", Cookies: []Cookie{{Name: "sessionid", Value: "x"}}})
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		t.Fatalf("request to %s", r.URL)
		return nil, nil
	})}
	if sess, ok, err := s.Fresh(context.Background(), client, "tiktok"); !ok || err != nil || sess.cookie("sessionid") != "x" {
		t.Fatalf("ok %v, err %v", ok, err)
	}
	if _, ok, _ := s.Fresh(context.Background(), client, "swag"); ok {
		t.Fatal("found a login never stored")
	}
}

func TestNeedsRenewal(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		token string
		want  bool
	}{
		{"", true},
		{jwtExpiring(now.Add(-time.Second)), true},
		{jwtExpiring(now.Add(time.Minute)), true},
		{jwtExpiring(now.Add(10 * time.Minute)), false},
		{"opaque-session-token", false},
		{"e30.e30.sig", false},
	} {
		if got := needsRenewal(tc.token, now); got != tc.want {
			t.Errorf("needsRenewal(%q) = %v", tc.token, got)
		}
	}
}
