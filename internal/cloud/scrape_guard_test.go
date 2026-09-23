package cloud

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"testing"
)

func TestCheckScrapeTarget(t *testing.T) {
	tests := []struct {
		name    string
		method  string
		rawURL  string
		wantErr error
	}{
		{name: "platform host", method: "GET", rawURL: "https://www.tiktok.com/@x/live"},
		{name: "platform subdomain", method: "GET", rawURL: "https://webcast.tiktok.com/api"},
		{name: "media host", method: "GET", rawURL: "https://usher.ttvnw.net/api/channel/hls/x.m3u8"},
		{name: "empty method is a GET", rawURL: "https://gql.twitch.tv/gql"},
		{name: "post is allowed", method: "POST", rawURL: "https://gql.twitch.tv/gql"},

		{name: "another site", method: "GET", rawURL: "https://example.com/", wantErr: errHostNotAllowed},
		{name: "loopback by name", method: "GET", rawURL: "http://localhost:8080/", wantErr: errHostNotAllowed},
		{name: "loopback by address", method: "GET", rawURL: "http://127.0.0.1/", wantErr: errHostNotAllowed},
		{name: "cloud metadata", method: "GET", rawURL: "http://169.254.169.254/latest/meta-data/", wantErr: errHostNotAllowed},
		{name: "lan address", method: "GET", rawURL: "http://192.168.1.1/", wantErr: errHostNotAllowed},
		{
			name: "platform name as a prefix of another domain", method: "GET",
			rawURL: "https://tiktok.com.evil.test/", wantErr: errHostNotAllowed,
		},
		{
			name: "trailing dot does not escape the suffix match", method: "GET",
			rawURL: "https://evil.test./", wantErr: errHostNotAllowed,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u, err := url.Parse(tt.rawURL)
			if err != nil {
				t.Fatalf("parsing %q: %v", tt.rawURL, err)
			}
			err = checkScrapeTarget(tt.method, u)
			switch {
			case tt.wantErr == nil && err != nil:
				t.Fatalf("checkScrapeTarget(%q) = %v, want allowed", tt.rawURL, err)
			case tt.wantErr != nil && !errors.Is(err, tt.wantErr):
				t.Fatalf("checkScrapeTarget(%q) = %v, want %v", tt.rawURL, err, tt.wantErr)
			}
		})
	}
}

func TestCheckScrapeTargetRefusesOtherMethods(t *testing.T) {
	u, _ := url.Parse("https://www.tiktok.com/")
	for _, method := range []string{"DELETE", "PUT", "PATCH", "CONNECT"} {
		if err := checkScrapeTarget(method, u); err == nil {
			t.Fatalf("checkScrapeTarget(%s) allowed the request", method)
		}
	}
}

func TestCheckScrapeTargetRefusesOtherSchemes(t *testing.T) {
	for _, raw := range []string{"file:///etc/passwd", "ftp://www.tiktok.com/"} {
		u, _ := url.Parse(raw)
		if err := checkScrapeTarget("GET", u); err == nil {
			t.Fatalf("checkScrapeTarget(%q) allowed the request", raw)
		}
	}
}

func TestBlockedAddr(t *testing.T) {
	blocked := []string{
		"127.0.0.1", "::1", "0.0.0.0", "10.1.2.3", "172.16.0.1", "192.168.1.1",
		"169.254.169.254", "100.64.0.1", "198.18.0.1", "255.255.255.255",
		"224.0.0.1", "fd00::1", "fc00::1", "fe80::1", "::ffff:127.0.0.1",
		"::ffff:10.0.0.1",
	}
	for _, s := range blocked {
		if !blockedAddr(netip.MustParseAddr(s)) {
			t.Errorf("blockedAddr(%s) = false, want true", s)
		}
	}

	allowed := []string{"1.1.1.1", "93.184.216.34", "2606:4700:4700::1111", "100.128.0.1"}
	for _, s := range allowed {
		if blockedAddr(netip.MustParseAddr(s)) {
			t.Errorf("blockedAddr(%s) = true, want false", s)
		}
	}
}

// The dial guard is what stops a platform hostname that resolves to a local
// address, which the name check alone cannot catch.
func TestScrapeClientRefusesLoopbackAddress(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("should never be reached"))
	}))
	defer srv.Close()

	// srv.URL is http://127.0.0.1:<port>, i.e. exactly the case the dial guard
	// exists for. The name check is bypassed here on purpose.
	_, err := newScrapeClient().Get(srv.URL)
	if err == nil {
		t.Fatal("scrape client reached a loopback address")
	}
	if !strings.Contains(err.Error(), errPrivateAddress.Error()) {
		t.Fatalf("error = %v, want it to mention a non-public address", err)
	}
}

func TestScrapeClientRefusesRedirectOffPlatform(t *testing.T) {
	client := newScrapeClient()
	req, _ := http.NewRequest("GET", "https://www.tiktok.com/", nil)
	redirect, _ := http.NewRequest("GET", "https://example.com/", nil)

	if err := client.CheckRedirect(redirect, []*http.Request{req}); !errors.Is(err, errHostNotAllowed) {
		t.Fatalf("CheckRedirect = %v, want %v", err, errHostNotAllowed)
	}
	stillOn, _ := http.NewRequest("GET", "https://www.tiktok.com/login", nil)
	if err := client.CheckRedirect(stillOn, []*http.Request{req}); err != nil {
		t.Fatalf("CheckRedirect on a platform host = %v, want nil", err)
	}
}

// allowAnyScrapeTarget is the scrapeHandler.checkTarget the tests use: their
// server is on loopback, which the real guard refuses first.
func allowAnyScrapeTarget(string, *url.URL) error { return nil }
