package webui

import (
	"context"
	"io"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
)

const (
	maxBridges  = 64
	pingTimeout = 15 * time.Second
)

// pingInterval keeps a proxy with a 60 s idle timeout from dropping a quiet
// socket.
var pingInterval = 25 * time.Second

// bridges tracks open sockets by session, so a sign-out or a rotated token
// can close them.
type bridges struct {
	mu    sync.Mutex
	conns map[*websocket.Conn]sessionID
}

func newBridges() *bridges { return &bridges{conns: map[*websocket.Conn]sessionID{}} }

// add registers c, refusing it past maxBridges.
func (b *bridges) add(c *websocket.Conn, sid sessionID) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.conns) >= maxBridges {
		return false
	}
	b.conns[c] = sid
	return true
}

func (b *bridges) remove(c *websocket.Conn) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.conns, c)
}

func (b *bridges) reserve() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.conns) < maxBridges
}

func (b *bridges) closeWhere(match func(sessionID) bool, reason string) int {
	b.mu.Lock()
	var hit []*websocket.Conn
	for c, sid := range b.conns {
		if match(sid) {
			hit = append(hit, c)
		}
	}
	b.mu.Unlock()
	for _, c := range hit {
		// Close waits for the browser's reply; don't hold up the others.
		go c.Close(websocket.StatusPolicyViolation, reason)
	}
	return len(hit)
}

func (b *bridges) closeSession(sid sessionID) int {
	return b.closeWhere(func(s sessionID) bool { return s == sid }, "signed out")
}

func (b *bridges) closeAll() int {
	return b.closeWhere(func(sessionID) bool { return true }, "session ended")
}

// handleBridge upgrades to a WebSocket and relays NATS protocol bytes between
// it and an in-process connection to the daemon's NATS server.
func (s *Server) handleBridge(w http.ResponseWriter, r *http.Request) {
	if !s.bridges.reserve() {
		writeStatus(w, http.StatusServiceUnavailable)
		return
	}
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		OriginPatterns: s.cfg.AllowedOrigins,
	})
	if err != nil {
		// Accept has answered: 403 for a foreign Origin.
		s.clientLog(r).Debug("Web UI socket refused", "error", err)
		return
	}
	if !s.bridges.add(ws, sessionFrom(r.Context()).sid) {
		ws.Close(websocket.StatusTryAgainLater, "too many connections")
		return
	}
	defer s.bridges.remove(ws)

	nc, err := s.cfg.Dial()
	if err != nil {
		s.log.Warn("Web UI could not reach the daemon's API", "error", err)
		ws.Close(websocket.StatusInternalError, "daemon unavailable")
		return
	}
	s.relay(r.Context(), ws, nc)
}

// relay copies both ways until either side ends, then closes both.
func (s *Server) relay(ctx context.Context, ws *websocket.Conn, nc net.Conn) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	wc := websocket.NetConn(ctx, ws, websocket.MessageBinary)
	var once sync.Once
	end := func() {
		once.Do(func() {
			cancel()
			nc.Close()
			wc.Close()
		})
	}
	defer end()

	go func() {
		defer end()
		t := time.NewTicker(pingInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			pctx, pcancel := context.WithTimeout(ctx, pingTimeout)
			err := ws.Ping(pctx)
			pcancel()
			if err != nil {
				return
			}
		}
	}()
	go func() {
		defer end()
		_, _ = io.Copy(wc, nc)
	}()
	_, _ = io.Copy(nc, wc)
}
