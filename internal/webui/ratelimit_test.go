package webui

import (
	"net/netip"
	"testing"
	"time"
)

func TestLimiter(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	l := newLimiter()
	l.now = func() time.Time { return now }
	a := netip.MustParseAddr("192.0.2.1")
	b := netip.MustParseAddr("192.0.2.2")

	for range failsPerIP {
		if l.blocked(a) > 0 {
			t.Fatal("blocked too early")
		}
		l.fail(a)
	}
	if wait := l.blocked(a); wait <= 0 || wait > failWindow {
		t.Fatalf("blocked(a) = %v after %d failures", wait, failsPerIP)
	}
	if l.blocked(b) > 0 {
		t.Fatal("another IP blocked")
	}
	now = now.Add(failWindow)
	if l.blocked(a) > 0 {
		t.Fatal("still blocked after the window")
	}
}

func TestLimiterGlobalAndBounded(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	l := newLimiter()
	l.now = func() time.Time { return now }
	for i := range failsInTotal {
		ip := netip.AddrFrom4([4]byte{10, 0, byte(i >> 8), byte(i)})
		if l.blocked(ip) > 0 {
			t.Fatalf("blocked after %d failures in total", i)
		}
		l.fail(ip)
	}
	if l.blocked(netip.MustParseAddr("192.0.2.9")) <= 0 {
		t.Fatal("global limit not enforced")
	}
	// Failures are only recorded while not blocked, so the map can't grow
	// past one window's worth, and old windows are pruned.
	now = now.Add(failWindow)
	l.fail(netip.MustParseAddr("192.0.2.9"))
	if n := l.size(); n != 1 {
		t.Errorf("limiter holds %d IPs after the window, want 1", n)
	}
}
