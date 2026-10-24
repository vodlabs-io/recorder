package webui

import (
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// proxies are the trusted reverse proxies, whose forwarding headers are
// believed. Any other client's are ignored: it could claim anything.
type proxies []netip.Prefix

func parseProxies(cidrs []string) (proxies, error) {
	var out proxies
	for _, c := range cidrs {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		if !strings.Contains(c, "/") {
			a, err := netip.ParseAddr(c)
			if err != nil {
				return nil, fmt.Errorf("web.trusted_proxies: %q is not an address or CIDR", c)
			}
			out = append(out, netip.PrefixFrom(a, a.BitLen()))
			continue
		}
		p, err := netip.ParsePrefix(c)
		if err != nil {
			return nil, fmt.Errorf("web.trusted_proxies: %q is not an address or CIDR", c)
		}
		out = append(out, p.Masked())
	}
	return out, nil
}

func (ps proxies) trusts(a netip.Addr) bool {
	a = a.Unmap()
	for _, p := range ps {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

func remoteAddr(r *http.Request) netip.Addr {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	a, _ := netip.ParseAddr(host)
	return a.Unmap()
}

// clientIP is the address rate limits and logs name: the peer, or behind a
// trusted proxy the rightmost X-Forwarded-For entry that isn't one, the only
// entry no client can have forged.
func (ps proxies) clientIP(r *http.Request) netip.Addr {
	peer := remoteAddr(r)
	if !ps.trusts(peer) {
		return peer
	}
	hops := strings.Split(strings.Join(r.Header.Values("X-Forwarded-For"), ","), ",")
	for i := len(hops) - 1; i >= 0; i-- {
		a, err := netip.ParseAddr(strings.TrimSpace(hops[i]))
		if err != nil {
			break
		}
		a = a.Unmap()
		if !ps.trusts(a) {
			return a
		}
	}
	return peer
}

// isHTTPS reports whether the browser reached us over TLS, directly or
// through a trusted proxy that says so.
func (ps proxies) isHTTPS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	if !ps.trusts(remoteAddr(r)) {
		return false
	}
	proto, _, _ := strings.Cut(r.Header.Get("X-Forwarded-Proto"), ",")
	return strings.EqualFold(strings.TrimSpace(proto), "https")
}
