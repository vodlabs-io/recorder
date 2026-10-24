// Package webui serves the Web UI: the desktop app's React UI built for the
// browser, a sign-in page, and a WebSocket that bridges the browser to the
// daemon's in-process NATS API. Every route but the sign-in ones needs a
// session cookie, which a correct sign-in token buys.
package webui

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"
)

// Config configures a Server.
type Config struct {
	Host string
	Port int
	// Dir is the config folder, which keeps the session secret and the
	// signed-out session ids.
	Dir string
	// ResolveToken returns the sign-in token in force. It is polled, so a
	// rotated token takes over without a restart.
	ResolveToken   func() (string, error)
	AllowedOrigins []string
	TrustedProxies []string
	// Dist is the built Web UI; nil serves a page saying this build has none.
	Dist fs.FS
	// Dial opens a connection to the daemon's NATS server.
	Dial func() (net.Conn, error)
	// Files serves recordings for playback and download; nil disables it.
	Files FileOpener
	Log   *slog.Logger
}

// pollInterval is how often the token and session secret are re-read.
var pollInterval = 2 * time.Second

// Server is a running Web UI listener.
type Server struct {
	cfg     Config
	log     *slog.Logger
	proxies proxies
	limit   *limiter
	revoked *revocations
	bridges *bridges
	now     func() time.Time
	srv     *http.Server
	ln      net.Listener
	stop    chan struct{}
	stopped sync.Once
	done    sync.WaitGroup

	mu     sync.RWMutex
	token  string
	secret []byte
	codec  codec
}

// Start binds the listener and serves in the background.
func Start(cfg Config) (*Server, error) {
	s, err := newServer(cfg)
	if err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp", net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port)))
	if err != nil {
		return nil, fmt.Errorf("listen on %s:%d: %w", cfg.Host, cfg.Port, err)
	}
	s.ln = ln
	s.done.Add(2)
	go func() {
		defer s.done.Done()
		if err := s.srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.log.Error("Web UI listener stopped", "error", err)
		}
	}()
	go func() {
		defer s.done.Done()
		s.watch()
	}()
	return s, nil
}

func newServer(cfg Config) (*Server, error) {
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.DiscardHandler)
	}
	ps, err := parseProxies(cfg.TrustedProxies)
	if err != nil {
		return nil, err
	}
	s := &Server{
		cfg:     cfg,
		log:     cfg.Log,
		proxies: ps,
		limit:   newLimiter(),
		revoked: loadRevocations(cfg.Dir),
		bridges: newBridges(),
		now:     time.Now,
		stop:    make(chan struct{}),
	}
	if _, err := s.reload(); err != nil {
		return nil, err
	}
	s.srv = &http.Server{
		Handler:           s.handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    16 << 10,
		ErrorLog:          slog.NewLogLogger(cfg.Log.Handler(), slog.LevelDebug),
	}
	return s, nil
}

// Addr is the bound listener address.
func (s *Server) Addr() string { return s.ln.Addr().String() }

// Close stops the listener and closes every bridged socket.
func (s *Server) Close() {
	s.stopped.Do(func() {
		close(s.stop)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		s.bridges.closeAll()
		_ = s.srv.Shutdown(ctx)
		s.done.Wait()
	})
}

// reload re-reads the token and the session secret, reporting whether either
// changed since the last read.
func (s *Server) reload() (bool, error) {
	token, err := s.cfg.ResolveToken()
	if err != nil {
		return false, err
	}
	secret, err := readSecret(s.cfg.Dir)
	if err != nil {
		return false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	changed := s.codec.key != nil && (token != s.token || string(secret) != string(s.secret))
	s.token, s.secret, s.codec = token, secret, newCodec(secret, token)
	return changed, nil
}

// watch polls for a rotated token or session secret. Either invalidates every
// session, so it also closes every open socket.
func (s *Server) watch() {
	t := time.NewTicker(pollInterval)
	defer t.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-t.C:
		}
		changed, err := s.reload()
		if err != nil {
			s.log.Warn("Web UI could not re-read its token", "error", err)
			continue
		}
		if changed {
			s.revoked.clear()
			n := s.bridges.closeAll()
			s.log.Info("Web UI token or session secret changed; signed every browser out", "closed_sockets", n)
		}
	}
}

func (s *Server) current() (string, codec) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.token, s.codec
}

func (s *Server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("GET /signin", s.serveSignin)
	mux.HandleFunc("GET /signin.js", s.serveSigninAsset)
	mux.HandleFunc("GET /signin.css", s.serveSigninAsset)
	mux.HandleFunc("POST /api/session", s.handleSignIn)
	mux.HandleFunc("GET /api/session", s.handleSessionStatus)
	mux.HandleFunc("POST /api/signout", s.requireSession(s.handleSignOut))
	mux.HandleFunc("GET /nats", s.requireSession(s.handleBridge))
	mux.HandleFunc("GET /api/vods/file", s.requireSession(s.handleVODFile))
	mux.HandleFunc("HEAD /api/vods/file", s.requireSession(s.handleVODFile))
	mux.HandleFunc("GET /{$}", s.requirePage(s.serveIndex))
	mux.HandleFunc("GET /assets/", s.requirePage(s.serveAsset))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})

	cop := http.NewCrossOriginProtection()
	for _, o := range s.cfg.AllowedOrigins {
		if err := cop.AddTrustedOrigin(o); err != nil {
			s.log.Warn("Ignoring invalid web.allowed_origins entry", "origin", o, "error", err)
		}
	}
	return securityHeaders(cop.Handler(mux))
}

// clientLog is the logger for one request, naming the client.
func (s *Server) clientLog(r *http.Request) *slog.Logger {
	return s.log.With("client_ip", s.proxies.clientIP(r).String())
}

func writeStatus(w http.ResponseWriter, code int) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_, _ = w.Write([]byte(http.StatusText(code) + "\n"))
}

// isNotExist maps a missing file to 404.
func isNotExist(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, os.ErrNotExist)
}
