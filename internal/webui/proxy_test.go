package webui

import (
	"crypto/tls"
	"net/http/httptest"
	"testing"
)

func TestClientIPAndHTTPS(t *testing.T) {
	ps, err := parseProxies([]string{"10.0.0.0/8", "192.168.1.1"})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, remote, xff, proto string
		tls                      bool
		ip                       string
		https                    bool
	}{
		{"direct", "192.0.2.5:1234", "", "", false, "192.0.2.5", false},
		{"direct TLS", "192.0.2.5:1234", "", "", true, "192.0.2.5", true},
		{"untrusted forwarding headers", "192.0.2.5:1234", "198.51.100.1", "https", false, "192.0.2.5", false},
		{"trusted proxy", "10.1.2.3:1234", "198.51.100.1", "https", false, "198.51.100.1", true},
		{"spoofed leftmost entry", "10.1.2.3:1234", "1.1.1.1, 198.51.100.1", "http", false, "198.51.100.1", false},
		{"proxy chain", "10.1.2.3:1234", "198.51.100.1, 192.168.1.1", "https", false, "198.51.100.1", true},
		{"single address", "192.168.1.1:80", "203.0.113.9", "https", false, "203.0.113.9", true},
	}
	for _, tc := range cases {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = tc.remote
		if tc.xff != "" {
			r.Header.Set("X-Forwarded-For", tc.xff)
		}
		if tc.proto != "" {
			r.Header.Set("X-Forwarded-Proto", tc.proto)
		}
		if tc.tls {
			r.TLS = &tls.ConnectionState{}
		}
		if got := ps.clientIP(r).String(); got != tc.ip {
			t.Errorf("%s: clientIP = %s, want %s", tc.name, got, tc.ip)
		}
		if got := ps.isHTTPS(r); got != tc.https {
			t.Errorf("%s: isHTTPS = %v, want %v", tc.name, got, tc.https)
		}
	}
}

func TestParseProxiesRejectsJunk(t *testing.T) {
	if _, err := parseProxies([]string{"not-a-cidr"}); err == nil {
		t.Fatal("accepted junk")
	}
}
