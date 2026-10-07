package cookiemint

import (
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeOrigin answers the mint and records the Cookie header every other
// request carried.
type fakeOrigin struct {
	mints   atomic.Int32
	cookies map[string]string // host → last Cookie header
}

func (f *fakeOrigin) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Path == "/token" {
		f.mints.Add(1)
		if req.Header.Get("Cookie") != "VT=jwt" {
			return reply(http.StatusUnauthorized, "Jwt is missing"), nil
		}
		return reply(http.StatusOK, `{"token":"abc","expireAt":1791410552,"ttlSeconds":10,"username":"u1"}`), nil
	}
	f.cookies[req.URL.Hostname()] = req.Header.Get("Cookie")
	return reply(http.StatusOK, "#EXTM3U"), nil
}

func reply(code int, body string) *http.Response {
	return &http.Response{StatusCode: code, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}
}

func testMint() *Mint {
	return &Mint{
		URL:     "https://gateway.example.me/token",
		Headers: map[string]string{"Cookie": "VT=jwt"},
		Cookies: map[string]string{"ttu": "username", "tt": "token", "tte": "expireAt"},
		TTL:     10,
	}
}

func get(t *testing.T, c *http.Client, rawURL string) {
	t.Helper()
	resp, err := c.Get(rawURL)
	if err != nil {
		t.Fatalf("GET %s: %v", rawURL, err)
	}
	resp.Body.Close()
}

func TestClientAttachesMintedCookies(t *testing.T) {
	origin := &fakeOrigin{cookies: map[string]string{}}
	c := Client(&http.Client{Transport: origin}, testMint())

	get(t, c, "https://cinema-de41.example.me/v2/a/master.m3u8")
	get(t, c, "https://cinema-de41.example.me/v2/b/live.m3u8")
	if got := origin.cookies["cinema-de41.example.me"]; got != "tt=abc; tte=1791410552; ttu=u1" {
		t.Errorf("Cookie = %q", got)
	}
	if n := origin.mints.Load(); n != 1 {
		t.Errorf("minted %d times, want once while the set is fresh", n)
	}
}

// TestClientKeepsCookiesOnTheMintsSite pins that a minted set is never sent
// to a host outside the mint URL's own site.
func TestClientKeepsCookiesOnTheMintsSite(t *testing.T) {
	origin := &fakeOrigin{cookies: map[string]string{}}
	c := Client(&http.Client{Transport: origin}, testMint())
	get(t, c, "https://cdn.elsewhere.test/seg.ts")
	get(t, c, "https://notexample.me/x")
	if got := origin.cookies["cdn.elsewhere.test"] + origin.cookies["notexample.me"]; got != "" {
		t.Errorf("sent %q off-site", got)
	}
	if n := origin.mints.Load(); n != 0 {
		t.Errorf("minted %d times for off-site requests", n)
	}
}

func TestClientRemintsAsTheSetAges(t *testing.T) {
	origin := &fakeOrigin{cookies: map[string]string{}}
	c := Client(&http.Client{Transport: origin}, testMint())
	get(t, c, "https://cinema.example.me/a.m3u8")
	m := c.Transport.(*transport).minter
	m.mu.Lock()
	m.at = time.Now().Add(-7 * time.Second) // past 60% of the ten-second TTL
	m.mu.Unlock()
	get(t, c, "https://cinema.example.me/a.m3u8")
	if n := origin.mints.Load(); n != 2 {
		t.Errorf("minted %d times, want a fresh set for the aged one", n)
	}
}

func TestClientFailsTheRequestWhenMintingFails(t *testing.T) {
	origin := &fakeOrigin{cookies: map[string]string{}}
	m := testMint()
	m.Headers = nil // the mint refuses a caller without its JWT
	c := Client(&http.Client{Transport: origin}, m)
	if _, err := c.Get("https://cinema.example.me/a.m3u8"); err == nil || !strings.Contains(err.Error(), "mint status 401") {
		t.Fatalf("err = %v, want the mint's refusal", err)
	}
}

func TestClientMergesExistingCookies(t *testing.T) {
	origin := &fakeOrigin{cookies: map[string]string{}}
	c := Client(&http.Client{Transport: origin}, testMint())
	req, _ := http.NewRequest(http.MethodGet, "https://cinema.example.me/a.m3u8", nil)
	req.Header.Set("Cookie", "a=b")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got := origin.cookies["cinema.example.me"]; got != "a=b; tt=abc; tte=1791410552; ttu=u1" {
		t.Errorf("Cookie = %q", got)
	}
}

func TestClientIgnoresUnusableMints(t *testing.T) {
	base := &http.Client{}
	for _, m := range []*Mint{
		nil,
		{URL: "http://gateway.example.me/token", Cookies: map[string]string{"a": "b"}, TTL: 10},
		{URL: "https://gateway.example.me/token", TTL: 10},
		{URL: "https://gateway.example.me/token", Cookies: map[string]string{"a": "b"}},
	} {
		if Client(base, m) != base {
			t.Errorf("Client(%+v) wrapped the client", m)
		}
	}
}

func TestRenderRejectsMissingFields(t *testing.T) {
	if _, err := render([]byte(`{"token":"abc"}`), map[string]string{"tt": "token", "tte": "expireAt"}); err == nil {
		t.Error("a reply missing a field rendered")
	}
}
