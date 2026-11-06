// Package netguard keeps a client that fetches addresses others supply on the
// public internet: never this machine, this network, or a range that is not
// public. The check runs at dial time, on the address actually dialled, so a
// hostname cannot talk its way around it.
package netguard

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"syscall"
	"time"
)

// ErrPrivateAddress is a connection refused for going somewhere not public.
var ErrPrivateAddress = errors.New("address is not a public internet address")

// BlockedAddr reports whether an address is one a guarded client must never
// reach.
func BlockedAddr(addr netip.Addr) bool {
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

// BlockedHost reports whether a URL's host can only be this machine or a
// private network on its face: "localhost" or a blocked literal address. It
// lets a caller refuse such a URL with a clear error before dialling; a name
// that resolves somewhere private is caught by DialControl.
func BlockedHost(host string) bool {
	host = strings.TrimSuffix(strings.ToLower(strings.Trim(host, "[]")), ".")
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	addr, err := netip.ParseAddr(host)
	return err == nil && BlockedAddr(addr)
}

// DialControl refuses a connection to a non-public address. It runs after DNS
// and before the connection is made, on the address the kernel is about to
// use, so a name that resolves to 127.0.0.1, or is re-resolved to it between
// a check and the dial, is refused here. Set it as a net.Dialer's Control for
// any client that fetches addresses others supply.
func DialControl(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("unexpected dial address %q: %w", address, err)
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return fmt.Errorf("unexpected dial address %q: %w", address, err)
	}
	if BlockedAddr(addr) {
		return fmt.Errorf("%w: %s", ErrPrivateAddress, addr)
	}
	return nil
}

// ControlContext is DialControl for a net.Dialer's ControlContext, except that
// it lets through the dial to the proxy Transport sends a request through.
func ControlContext(ctx context.Context, network, address string, c syscall.RawConn) error {
	if p, _ := ctx.Value(proxyKey{}).(proxyChoice); p.url != nil {
		return nil
	}
	return DialControl(network, address, c)
}

// Transport returns a RoundTripper that sends each request through t, via the
// proxy t.Proxy picks for it (such as http.ProxyFromEnvironment). That proxy is
// the user's own configuration, so ControlContext lets t dial it even on this
// machine or network; every direct dial stays guarded. t.Proxy is replaced, so
// send requests through the returned RoundTripper only.
//
// The proxy resolves a proxied request's host itself, out of the dial guard's
// sight. So a host that is BlockedHost on its face, or that resolves to a
// blocked address on this machine, is refused before it. A host this machine
// cannot resolve is left to the proxy: a sandbox behind one often has no DNS.
func Transport(t *http.Transport) http.RoundTripper {
	g := &transport{t: t, pick: t.Proxy, names: map[string]nameCheck{}}
	t.Proxy = func(req *http.Request) (*url.URL, error) {
		p, _ := req.Context().Value(proxyKey{}).(proxyChoice)
		return p.url, p.err
	}
	return g
}

// proxyKey keys a request's proxyChoice in its context, where t.Proxy and the
// dial it leads to both read it.
type proxyKey struct{}

type proxyChoice struct {
	url *url.URL
	err error
}

type transport struct {
	t    *http.Transport
	pick func(*http.Request) (*url.URL, error)

	mu    sync.Mutex
	names map[string]nameCheck // proxied hosts' lookups, by lower-case name
}

// nameCheck is a proxied host's lookup: the blocked address it resolved to, if
// any, until the lookup expires.
type nameCheck struct {
	blocked netip.Addr
	until   time.Time
}

// lookupNetIP resolves a proxied request's host. Tests replace it.
var lookupNetIP = net.DefaultResolver.LookupNetIP

const (
	proxiedLookupTimeout = 2 * time.Second
	proxiedLookupTTL     = time.Minute
	proxiedLookupMax     = 1024 // cached names before the cache is cleared
)

func (g *transport) RoundTrip(req *http.Request) (*http.Response, error) {
	var p proxyChoice
	if g.pick != nil {
		p.url, p.err = g.pick(req)
	}
	if p.err == nil && p.url != nil {
		p.err = g.checkProxied(req.Context(), req.URL.Hostname())
	}
	return g.t.RoundTrip(req.WithContext(context.WithValue(req.Context(), proxyKey{}, p)))
}

// checkProxied refuses a proxied request's host if it is BlockedHost, or if any
// address it resolves to here is blocked. A failed lookup lets it through.
func (g *transport) checkProxied(ctx context.Context, host string) error {
	if BlockedHost(host) {
		return fmt.Errorf("%w: %s", ErrPrivateAddress, host)
	}
	if _, err := netip.ParseAddr(strings.Trim(host, "[]")); err == nil {
		return nil // a public literal address
	}
	name := strings.TrimSuffix(strings.ToLower(host), ".")

	now := time.Now()
	g.mu.Lock()
	c, ok := g.names[name]
	g.mu.Unlock()
	if !ok || now.After(c.until) {
		c = nameCheck{until: now.Add(proxiedLookupTTL)}
		lctx, cancel := context.WithTimeout(ctx, proxiedLookupTimeout)
		addrs, err := lookupNetIP(lctx, "ip", name)
		cancel()
		if ctx.Err() != nil {
			return nil // the request is over; its transport reports why
		}
		if err == nil {
			for _, a := range addrs {
				if BlockedAddr(a) {
					c.blocked = a
					break
				}
			}
		}
		g.mu.Lock()
		if len(g.names) >= proxiedLookupMax {
			clear(g.names)
		}
		g.names[name] = c
		g.mu.Unlock()
	}
	if c.blocked.IsValid() {
		return fmt.Errorf("%w: %s resolves to %s", ErrPrivateAddress, host, c.blocked)
	}
	return nil
}

func (g *transport) CloseIdleConnections() { g.t.CloseIdleConnections() }
