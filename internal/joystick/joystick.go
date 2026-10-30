// Package joystick takes a seat on a Joystick.tv channel limited to an
// audience, for recording its stream as the signed-in user.
//
// A public Joystick stream is handed to the recorder as a direct HLS URL: the
// playback token is one a signed-out viewer mints itself. A stream limited to
// the streamer's followers, subscribers or verified viewers needs the viewer's
// own login, whose token the streaming server honours, so the backend names it
// joystick://<username>?pb=<channel base>&mode=<audience> and the recorder
// records it here: it reads the login token out of the jar the worker hands it,
// opens the channel's master playlist with that token, and polls it — the seat
// ends the attempt cleanly once the stream ends or turns the viewer away.
package joystick

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/vodlabs-io/recorder/internal/useragent"
)

// Scheme names a channel to record as the signed-in user.
const Scheme = "joystick"

// SiteHost is the host a login's cookies belong to, and the one whose jar the
// seat reads its playback token from.
const SiteHost = "joystick.tv"

// watchEvery is how often the master is re-read while the seat is held.
var watchEvery = 15 * time.Second

const (
	openTimeout    = 30 * time.Second
	requestTimeout = 15 * time.Second
	maxBody        = 1 << 20
)

var (
	// ErrEnded reports a stream that ended while a seat was held or taken.
	ErrEnded = errors.New("joystick stream ended")
	// ErrLocked reports a stream the viewer may not watch: one limited to an
	// audience the recorder's login is not part of.
	ErrLocked = errors.New("joystick stream is closed to this viewer")
)

// IsURL reports whether u names a Joystick channel seat.
func IsURL(u string) bool {
	return strings.HasPrefix(u, Scheme+"://")
}

// parsed is what a joystick:// URL names.
type parsed struct {
	username     string
	playbackBase string
	audience     string
}

// Parse decodes a joystick:// seat URL.
func Parse(raw string) (parsed, error) {
	if !IsURL(raw) {
		return parsed{}, fmt.Errorf("not a joystick URL: %q", raw)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return parsed{}, fmt.Errorf("parse joystick URL: %w", err)
	}
	pb := u.Query().Get("pb")
	if pb == "" {
		return parsed{}, fmt.Errorf("joystick URL %q names no playback base", raw)
	}
	return parsed{
		username:     strings.ToLower(u.Host),
		playbackBase: pb,
		audience:     u.Query().Get("mode"),
	}, nil
}

// Seat is a viewer watching a channel; its playlist stays usable until Done is
// closed.
type Seat struct {
	// MasterURL is the HLS master playlist carrying the viewer's token.
	MasterURL string

	p         parsed
	token     string
	http      *http.Client
	userAgent string
	cancel    context.CancelFunc
	done      chan struct{}

	mu  sync.Mutex
	err error
}

// Open reads the channel raw names and watches it until ctx ends or Close is
// called. client fetches the playlist; login, when not empty, is the jar the
// seat reads its playback token from (the viewer's Joystick session), and lets
// a stream limited to an audience the viewer belongs to be recorded.
func Open(ctx context.Context, raw string, client *http.Client, userAgent string, login map[string]string, logger *slog.Logger) (*Seat, error) {
	p, err := Parse(raw)
	if err != nil {
		return nil, err
	}
	if client == nil {
		client = http.DefaultClient
	}
	if userAgent == "" {
		userAgent = useragent.Fallback
	}
	token := tokenFromLogin(login)
	if token == "" {
		token = mintGuestToken()
	}
	s := &Seat{p: p, token: token, http: client, userAgent: userAgent, done: make(chan struct{})}

	openCtx, cancelOpen := context.WithTimeout(ctx, openTimeout)
	defer cancelOpen()
	if err := s.check(openCtx); err != nil {
		return nil, err
	}
	s.MasterURL = masterURL(p.playbackBase, token)
	holdCtx, cancel := context.WithCancel(ctx)
	s.cancel = cancel
	logger.Info("Watching a Joystick channel", "username", p.username, "audience", p.audience, "signed_in", tokenFromLogin(login) != "")
	go s.hold(holdCtx, logger)
	return s, nil
}

// Client returns base: the master playlist carries its own token and needs no
// cookies.
func (s *Seat) Client(base *http.Client) *http.Client { return base }

// Done is closed when the seat is lost or released.
func (s *Seat) Done() <-chan struct{} { return s.done }

// Err reports why the seat was lost: ErrEnded or ErrLocked, nil while held or
// after Close.
func (s *Seat) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

// Close gives the seat up.
func (s *Seat) Close() {
	s.cancel()
	<-s.done
}

// hold re-reads the master until ctx ends or the stream ends or turns the
// viewer away.
func (s *Seat) hold(ctx context.Context, logger *slog.Logger) {
	defer close(s.done)
	t := time.NewTicker(watchEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		cctx, cancel := context.WithTimeout(ctx, requestTimeout)
		err := s.check(cctx)
		cancel()
		if ctx.Err() != nil {
			return
		}
		if err == nil {
			continue
		}
		if !errors.Is(err, ErrEnded) && !errors.Is(err, ErrLocked) {
			// A failed read says nothing about the stream; the playlist itself
			// shows a stream that is gone.
			logger.Debug("Joystick master read failed", "username", s.p.username, "error", err)
			continue
		}
		logger.Info("Leaving the Joystick channel", "username", s.p.username, "reason", err)
		s.mu.Lock()
		s.err = err
		s.mu.Unlock()
		return
	}
}

// check fetches the master playlist: ErrEnded once it is gone, ErrLocked once
// the viewer is turned away, nil while it is served.
func (s *Seat) check(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, masterURL(s.p.playbackBase, s.token), nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", s.userAgent)
	req.Header.Set("Referer", "https://"+SiteHost+"/")
	resp, err := s.http.Do(req)
	if err != nil {
		return fmt.Errorf("joystick master: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxBody))
	switch resp.StatusCode {
	case http.StatusOK:
		return nil
	case http.StatusUnauthorized, http.StatusForbidden:
		return ErrLocked
	case http.StatusNotFound, http.StatusGone:
		return ErrEnded
	default:
		return fmt.Errorf("joystick master: status %d", resp.StatusCode)
	}
}

// masterURL builds the HLS master playlist URL for a channel base and token.
func masterURL(playbackBase, token string) string {
	return strings.TrimRight(playbackBase, "/") + "/index.m3u8?token=" + token
}

// tokenFromLogin reads the Joystick session token out of the jar the worker
// hands the seat. Joystick keeps the token in a JSON "user" cookie; the seat
// takes its "token" field.
func tokenFromLogin(login map[string]string) string {
	if login == nil {
		return ""
	}
	raw := login["Cookie"]
	if raw == "" {
		return ""
	}
	for _, c := range strings.Split(raw, ";") {
		c = strings.TrimSpace(c)
		name, val, ok := strings.Cut(c, "=")
		if !ok || name != "user" {
			continue
		}
		decoded, err := url.QueryUnescape(val)
		if err != nil {
			decoded = val
		}
		var j struct {
			Token string `json:"token"`
		}
		if json.Unmarshal([]byte(decoded), &j) == nil && j.Token != "" {
			return j.Token
		}
	}
	return ""
}
