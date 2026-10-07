package netguard

import (
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
)

func TestBlockedAddr(t *testing.T) {
	blocked := []string{
		"127.0.0.1", "::1", "0.0.0.0", "10.1.2.3", "172.16.0.1", "192.168.1.1",
		"169.254.169.254", "100.64.0.1", "198.18.0.1", "255.255.255.255",
		"224.0.0.1", "fd00::1", "fc00::1", "fe80::1", "::ffff:127.0.0.1",
		"::ffff:10.0.0.1",
	}
	for _, s := range blocked {
		if !BlockedAddr(netip.MustParseAddr(s)) {
			t.Errorf("BlockedAddr(%s) = false, want true", s)
		}
	}

	allowed := []string{"1.1.1.1", "93.184.216.34", "2606:4700:4700::1111", "100.128.0.1"}
	for _, s := range allowed {
		if BlockedAddr(netip.MustParseAddr(s)) {
			t.Errorf("BlockedAddr(%s) = true, want false", s)
		}
	}
}

func TestBlockedHost(t *testing.T) {
	for host, want := range map[string]bool{
		"localhost": true, "LOCALHOST.": true, "dev.localhost": true,
		"127.0.0.1": true, "[::1]": true, "192.168.0.10": true,
		"example.com": false, "8.8.8.8": false, "localhost.example.com": false,
	} {
		if got := BlockedHost(host); got != want {
			t.Errorf("BlockedHost(%q) = %v, want %v", host, got, want)
		}
	}
}

// The dial guard is what stops a public-looking name that resolves to a local
// address, which no check on the URL can catch.
func TestDialControlRefusesLoopback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("should never be reached"))
	}))
	defer srv.Close()

	dialer := &net.Dialer{Control: DialControl}
	client := &http.Client{Transport: &http.Transport{DialContext: dialer.DialContext}}
	_, err := client.Get(srv.URL)
	if !errors.Is(err, ErrPrivateAddress) {
		t.Fatalf("error = %v, want %v", err, ErrPrivateAddress)
	}
}
