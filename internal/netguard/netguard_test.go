package netguard

import (
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
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

// A proxy the user runs on loopback, as a sandbox's egress proxy does, is
// reached, while a direct dial to loopback, the proxy's included, is refused.
func TestTransportReachesLoopbackProxy(t *testing.T) {
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("through the proxy"))
	}))
	defer target.Close()

	// A CONNECT proxy on 127.0.0.1 that tunnels every request to target.
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			http.Error(w, "CONNECT only", http.StatusMethodNotAllowed)
			return
		}
		up, err := net.Dial("tcp", target.Listener.Addr().String())
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer up.Close()
		conn, _, err := http.NewResponseController(w).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		io.WriteString(conn, "HTTP/1.1 200 Connection Established\r\n\r\n")
		go io.Copy(up, conn)
		io.Copy(conn, up)
	}))
	defer proxy.Close()
	proxyURL, _ := url.Parse(proxy.URL)

	client := func(proxied bool) *http.Client {
		tr := &http.Transport{
			DialContext:     (&net.Dialer{ControlContext: ControlContext}).DialContext,
			TLSClientConfig: target.Client().Transport.(*http.Transport).TLSClientConfig,
		}
		if proxied {
			tr.Proxy = http.ProxyURL(proxyURL)
		}
		return &http.Client{Transport: Transport(tr)}
	}

	// httptest's certificate is valid for example.com.
	resp, err := client(true).Get("https://example.com/")
	if err != nil {
		t.Fatalf("through the proxy: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "through the proxy" {
		t.Fatalf("body = %q", body)
	}

	for _, raw := range []string{target.URL, proxy.URL} {
		if _, err := client(false).Get(raw); !errors.Is(err, ErrPrivateAddress) {
			t.Errorf("direct %s: error = %v, want %v", raw, err, ErrPrivateAddress)
		}
	}
	// The proxy would resolve a private host itself, so it is never asked.
	if _, err := client(true).Get("http://10.0.0.1/"); !errors.Is(err, ErrPrivateAddress) {
		t.Errorf("proxied 10.0.0.1: error = %v, want %v", err, ErrPrivateAddress)
	}
}
