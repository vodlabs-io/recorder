package cloud

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"syscall"
	"time"

	"github.com/vodlabs-io/recorder/internal/vlconfig"
)

// errHostNotAllowed is returned for a scrape request aimed somewhere other than
// a supported platform.
var errHostNotAllowed = errors.New("host is not a supported platform")

// errPrivateAddress is returned when a scrape resolves to an address on this
// machine or this network.
var errPrivateAddress = errors.New("address is not a public internet address")

// scrapeMethods are the methods a scrape request may use. Checks read pages and
// call APIs; nothing about them needs to delete or overwrite anything, and a
// narrow set keeps it that way.
var scrapeMethods = map[string]bool{
	http.MethodGet:  true,
	http.MethodPost: true,
}

// checkScrapeTarget reports whether a scrape may be made for this URL.
//
// It bounds the request on the way out, before DNS: the scheme has to be
// http(s), the method has to be one checks use, and the host has to belong to a
// platform (see vlconfig.ScrapeHostAllowed). The address the host resolves to is
// checked separately, at dial time, because a name can point anywhere.
func checkScrapeTarget(method string, u *url.URL) error {
	if u == nil || (u.Scheme != "http" && u.Scheme != "https") {
		return errors.New("invalid URL")
	}
	if method == "" {
		method = http.MethodGet // http.NewRequest's own default
	}
	if !scrapeMethods[method] {
		return fmt.Errorf("method %s is not allowed for a check", method)
	}
	if !vlconfig.ScrapeHostAllowed(u.Hostname()) {
		return errHostNotAllowed
	}
	return nil
}

// blockedAddr reports whether an address is one a scrape must never reach: this
// machine, this network, or a range that is not public internet. It is the half
// of the guard a hostname cannot talk its way around, since it runs on the
// address actually dialled.
func blockedAddr(addr netip.Addr) bool {
	addr = addr.Unmap() // an IPv4-mapped IPv6 address is its IPv4 address
	switch {
	case !addr.IsValid(),
		addr.IsLoopback(),
		addr.IsUnspecified(),
		addr.IsPrivate(),
		addr.IsLinkLocalUnicast(),
		addr.IsLinkLocalMulticast(),
		addr.IsInterfaceLocalMulticast(),
		addr.IsMulticast():
		return true
	}
	if addr.Is4() {
		b := addr.As4()
		switch {
		case b[0] == 0: // 0.0.0.0/8, "this network"
			return true
		case b[0] == 100 && b[1]&0xc0 == 64: // 100.64.0.0/10, carrier-grade NAT
			return true
		case b[0] == 198 && b[1]&0xfe == 18: // 198.18.0.0/15, benchmarking
			return true
		case b[0] == 255 && b[1] == 255 && b[2] == 255 && b[3] == 255: // broadcast
			return true
		}
		return false
	}
	// IPv6 unique local addresses, fc00::/7, which Addr.IsPrivate only reports
	// for fd00::/8.
	return addr.As16()[0]&0xfe == 0xfc
}

// PublicDialControl refuses a connection to a non-public address. It runs after
// DNS and before the connection is made, on the address the kernel is about to
// use, so a name that resolves to 127.0.0.1 — or is re-resolved to it between
// the check and the dial — is refused here. Set it as a net.Dialer's Control
// for any client that fetches addresses others supply.
func PublicDialControl(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("unexpected dial address %q: %w", address, err)
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return fmt.Errorf("unexpected dial address %q: %w", address, err)
	}
	if blockedAddr(addr) {
		return fmt.Errorf("%w: %s", errPrivateAddress, addr)
	}
	return nil
}

// newScrapeClient builds the HTTP client scrapes are made with. It is separate
// from the client the daemon talks to the service with, which legitimately
// reaches a private address when a developer runs the service locally.
//
// Each request carries its own deadline, so the client sets no overall timeout.
//
// Redirects are re-checked rather than followed blindly, so a platform URL
// cannot hand the daemon on to somewhere the guard would have refused. (Go
// already drops Cookie and Authorization headers on a cross-host redirect, so
// the login cannot travel either way.)
func newScrapeClient() *http.Client {
	dialer := &net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 30 * time.Second,
		Control:   PublicDialControl,
	}
	return &http.Client{
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           dialer.DialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          10,
			IdleConnTimeout:       60 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: time.Second,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return errors.New("stopped after 10 redirects")
			}
			return checkScrapeTarget(req.Method, req.URL)
		},
	}
}
