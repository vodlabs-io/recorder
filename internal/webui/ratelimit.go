package webui

import (
	"net/netip"
	"sync"
	"time"
)

const (
	failWindow   = 15 * time.Minute
	failsPerIP   = 10
	failsInTotal = 100
)

// limiter counts failed sign-ins per client IP and in total, in fixed
// windows. It stays small without a cap: it only grows on a recorded
// failure, and failsInTotal bounds those per window.
type limiter struct {
	mu     sync.Mutex
	now    func() time.Time
	perIP  map[netip.Addr]*window
	global window
}

type window struct {
	start time.Time
	count int
}

func newLimiter() *limiter {
	return &limiter{now: time.Now, perIP: map[netip.Addr]*window{}}
}

func (w *window) current(now time.Time) *window {
	if now.Sub(w.start) >= failWindow {
		w.start, w.count = now, 0
	}
	return w
}

// blocked reports how long ip must wait before trying again, 0 if it may.
func (l *limiter) blocked(ip netip.Addr) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if g := l.global.current(now); g.count >= failsInTotal {
		return g.start.Add(failWindow).Sub(now)
	}
	if w, ok := l.perIP[ip]; ok {
		if w = w.current(now); w.count >= failsPerIP {
			return w.start.Add(failWindow).Sub(now)
		}
	}
	return 0
}

func (l *limiter) fail(ip netip.Addr) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	for k, w := range l.perIP {
		if now.Sub(w.start) >= failWindow {
			delete(l.perIP, k)
		}
	}
	w, ok := l.perIP[ip]
	if !ok {
		w = &window{start: now}
		l.perIP[ip] = w
	}
	w.current(now).count++
	l.global.current(now).count++
}

func (l *limiter) size() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.perIP)
}
