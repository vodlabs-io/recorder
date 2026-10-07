// Package netguard keeps a client that fetches addresses others supply on the
// public internet: never this machine, this network, or a range that is not
// public. The check runs at dial time, on the address actually dialled, so a
// hostname cannot talk its way around it.
package netguard

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"syscall"
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
