package platformauth

import (
	"strings"
	"testing"
	"time"
)

func TestSAPISIDHashMatchesGoogleScheme(t *testing.T) {
	// Fixed instant so the signature is reproducible. The digest is
	// sha1("1700000000 SECRET https://www.youtube.com") — the exact string
	// YouTube's own web client hashes — cross-checked against independent
	// Python and Node implementations rather than against this code.
	at := time.Unix(1700000000, 0)
	got := sapisidHash("SECRET", "https://www.youtube.com", at)
	const want = "SAPISIDHASH 1700000000_898814012fff61748414f947f7b4f443948a06f0"
	if got != want {
		t.Errorf("sapisidHash() = %q, want %q", got, want)
	}
}

func TestAuthHeadersPrefersFirstPresentCookie(t *testing.T) {
	s := Session{Platform: "youtube", Cookies: []Cookie{
		{Name: "SAPISID", Value: "legacy"},
		{Name: "__Secure-3PAPISID", Value: "preferred"},
	}}
	h := s.AuthHeaders(SchemeSAPISIDHASH, []string{"__Secure-3PAPISID", "SAPISID"}, "https://www.youtube.com", SignedRequest{})
	if h["X-Origin"] != "https://www.youtube.com" {
		t.Errorf("X-Origin = %q", h["X-Origin"])
	}
	// The header must be built from the preferred cookie, not merely from one
	// that happens to be earlier in the jar.
	ts := time.Now().Unix()
	var want string
	for _, cand := range []int64{ts, ts - 1} { // tolerate a second boundary
		want = sapisidHash("preferred", "https://www.youtube.com", time.Unix(cand, 0))
		if h["Authorization"] == want {
			return
		}
	}
	t.Errorf("Authorization = %q, want one built from the preferred cookie (%q)", h["Authorization"], want)
}

func TestAuthHeadersDeclinesWhatItCannotSign(t *testing.T) {
	full := Session{Cookies: []Cookie{{Name: "SAPISID", Value: "v"}}}
	cases := []struct {
		name    string
		session Session
		scheme  string
		cookies []string
		origin  string
	}{
		// An unknown scheme is ignored, not fatal: the cookies still go.
		{"unknown scheme", full, "hmac-v2", []string{"SAPISID"}, "https://www.youtube.com"},
		{"no origin to bind", full, SchemeSAPISIDHASH, []string{"SAPISID"}, ""},
		{"secret cookie absent", Session{Cookies: []Cookie{{Name: "LOGIN_INFO", Value: "x"}}},
			SchemeSAPISIDHASH, []string{"SAPISID"}, "https://www.youtube.com"},
		{"secret cookie empty", Session{Cookies: []Cookie{{Name: "SAPISID", Value: ""}}},
			SchemeSAPISIDHASH, []string{"SAPISID"}, "https://www.youtube.com"},
	}
	for _, tc := range cases {
		if h := tc.session.AuthHeaders(tc.scheme, tc.cookies, tc.origin, SignedRequest{}); h != nil {
			t.Errorf("%s: got headers %v, want nil", tc.name, h)
		}
	}
}

// TestOAuthCookieHeader covers Twitch's scheme: the cookie's value replayed as
// a bearer credential, which is the only thing that signs a GraphQL call in as
// the user (its API ignores the Cookie header).
func TestOAuthCookieHeader(t *testing.T) {
	s := Session{Platform: "twitch", Cookies: []Cookie{
		{Name: "login", Value: "someone"},
		{Name: "auth-token", Value: "thetoken"},
	}}
	h := s.AuthHeaders(SchemeOAuthCookie, []string{"auth-token"}, "", SignedRequest{})
	if got := h["Authorization"]; got != "OAuth thetoken" {
		t.Errorf("Authorization = %q, want %q", got, "OAuth thetoken")
	}
	// Unlike SAPISIDHASH this scheme binds to no origin, so it must not invent
	// the X-Origin that scheme sends.
	if _, ok := h["X-Origin"]; ok {
		t.Errorf("oauth-cookie should send no X-Origin, got %v", h)
	}
	// A jar with no auth-token in it cannot authenticate, and must not produce
	// a header carrying some other cookie's value.
	empty := Session{Platform: "twitch", Cookies: []Cookie{{Name: "login", Value: "someone"}}}
	if h := empty.AuthHeaders(SchemeOAuthCookie, []string{"auth-token"}, "", SignedRequest{}); h != nil {
		t.Errorf("got headers %v from a jar with no auth-token, want nil", h)
	}
}

func TestHasAuthSecret(t *testing.T) {
	s := Session{Cookies: []Cookie{{Name: "LOGIN_INFO", Value: "x"}, {Name: "SAPISID", Value: "v"}}}
	if !s.HasAuthSecret([]string{"__Secure-3PAPISID", "SAPISID"}) {
		t.Error("jar carries SAPISID but HasAuthSecret said no")
	}
	if s.HasAuthSecret([]string{"__Secure-3PAPISID"}) {
		t.Error("jar lacks __Secure-3PAPISID but HasAuthSecret said yes")
	}
}

// TestStatusCanAuthenticate covers the silent-failure mode the flag exists for:
// a capture that keeps the session cookie but drops the signing one stores
// clean and reports Active while authenticating nothing.
func TestStatusCanAuthenticate(t *testing.T) {
	signable := Session{Platform: "youtube", Healthy: true, Cookies: []Cookie{
		{Name: "LOGIN_INFO", Value: "x"},
		{Name: "__Secure-3PAPISID", Value: "secret"},
	}}
	if !signable.Status().CanAuthenticate {
		t.Error("a jar carrying the signing cookie reported that it cannot authenticate")
	}

	unsignable := Session{Platform: "youtube", Healthy: true, Cookies: []Cookie{
		{Name: "LOGIN_INFO", Value: "x"},
	}}
	if unsignable.Status().CanAuthenticate {
		t.Error("a jar with no signing cookie reported that it can authenticate")
	}

	// A platform that declares no scheme needs nothing beyond its jar, so the
	// flag must not read as a fault for every other platform.
	tiktok := Session{Platform: "tiktok", Healthy: true, Cookies: []Cookie{{Name: "sessionid", Value: "x"}}}
	if !tiktok.Status().CanAuthenticate {
		t.Error("a platform with no auth scheme was reported unable to authenticate")
	}

	// Twitch signs with the same cookie it calls the session cookie, so any jar
	// the daemon accepted as a login can also authenticate with it.
	twitch := Session{Platform: "twitch", Healthy: true, Cookies: []Cookie{{Name: "auth-token", Value: "x"}}}
	if !twitch.Status().CanAuthenticate {
		t.Error("a twitch jar carrying auth-token reported that it cannot authenticate")
	}
}

// The header is secret-derived; make sure the secret itself cannot leak into it.
func TestAuthHeaderDoesNotContainTheSecret(t *testing.T) {
	const secret = "AVeryDistinctiveSapisidValue"
	s := Session{Cookies: []Cookie{{Name: "SAPISID", Value: secret}}}
	h := s.AuthHeaders(SchemeSAPISIDHASH, []string{"SAPISID"}, "https://www.youtube.com", SignedRequest{})
	for k, v := range h {
		if strings.Contains(v, secret) {
			t.Errorf("header %s leaks the cookie value", k)
		}
	}
}

func TestTiLiveHeadersMatchWebClient(t *testing.T) {
	// Digests cross-checked against Python's hashlib, following the web
	// client's own signing code: md5(url + md5(body) + ts + userId + token[:6]).
	at := time.UnixMilli(1791533572915)
	post := tiliveHeaders("317147090", "abcdef0123456789", SignedRequest{
		URL:  "https://api.ti.live/vchat/app/state/get_state",
		Body: []byte(`{"idList":[273178158],"typeList":[1,2]}`),
	}, at)
	if post["CheckSum"] != "9abfc9ef8a5852a464084d5d82673863" {
		t.Errorf("POST CheckSum = %q", post["CheckSum"])
	}
	if post["timestamp"] != "1791533572915" || post["userId"] != "317147090" || post["token"] != "abcdef0123456789" {
		t.Errorf("unexpected headers %v", post)
	}
	get := tiliveHeaders("317147090", "abcdef0123456789", SignedRequest{
		URL: "https://api.ti.live/vchat/app/video_living/room_info/273178158",
	}, at)
	if get["CheckSum"] != "e04830ec8281fcb19121f07636328289" {
		t.Errorf("GET CheckSum = %q", get["CheckSum"])
	}
}

func TestTiLiveSchemeNeedsBothCookies(t *testing.T) {
	names := []string{"token", "userId"}
	req := SignedRequest{URL: "https://api.ti.live/vchat/app/video_living/room_info/1"}
	full := Session{Cookies: []Cookie{{Name: "userId", Value: "42"}, {Name: "token", Value: "secret"}}}
	h := full.AuthHeaders(SchemeTiLiveChecksum, names, "", req)
	if h["userId"] != "42" || h["token"] != "secret" || len(h["CheckSum"]) != 32 {
		t.Errorf("headers = %v", h)
	}
	for _, s := range []Session{
		{Cookies: []Cookie{{Name: "token", Value: "secret"}}},
		{Cookies: []Cookie{{Name: "userId", Value: "42"}}},
	} {
		if h := s.AuthHeaders(SchemeTiLiveChecksum, names, "", req); h != nil {
			t.Errorf("signed with a partial jar %v: %v", s.Cookies, h)
		}
	}
	if h := full.AuthHeaders(SchemeTiLiveChecksum, names[:1], "", req); h != nil {
		t.Errorf("signed without naming the user-id cookie: %v", h)
	}
}
