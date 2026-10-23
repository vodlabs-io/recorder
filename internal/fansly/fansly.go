// Package fansly takes a seat on a Fansly channel, for recording its stream.
//
// Fansly streams over Amazon IVS from private channels: the channel's master
// playlist wants a playback token signed for the viewer, good for ten seconds,
// which the channel read hands to a viewer allowed to watch. A broadcaster can
// limit a stream to followers or subscribers at any point, so a stream is named
// by a fansly://<account id> URL, and whoever records it reads the channel
// itself — as its user, with their own login where the recorder holds one —
// takes the signed playlist at once, and re-reads the channel while it records:
// the seat ends once the stream is off the air or turns its viewer away.
package fansly

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/vodlabs-io/recorder/internal/useragent"
)

// Scheme names a channel to record.
const Scheme = "fansly"

// APIHost is the host the seat reads the channel from, and the one a login's
// headers may go to.
const APIHost = "apiv3.fansly.com"

// apiOrigin is the API; a variable so tests can point it at a fake.
var apiOrigin = "https://" + APIHost

// watchEvery is how often the channel is re-read while the seat is held.
var watchEvery = 15 * time.Second

const (
	siteOrigin = "https://fansly.com"
	// openTimeout bounds taking a seat.
	openTimeout = 30 * time.Second
	// requestTimeout bounds one API call.
	requestTimeout = 15 * time.Second
	// streamLive is the status of a stream on the air.
	streamLive = 2
	maxBody    = 1 << 20
)

var (
	// ErrEnded reports a stream that ended, or was not on, while a seat was
	// held or taken.
	ErrEnded = errors.New("fansly stream ended")
	// ErrLocked reports a stream its viewer may not watch: one limited to
	// followers or subscribers.
	ErrLocked = errors.New("fansly stream is closed to this viewer")
)

var accountPattern = regexp.MustCompile(`^[0-9]{1,20}$`)

// IsURL reports whether u names a Fansly channel.
func IsURL(u string) bool {
	return strings.HasPrefix(u, Scheme+"://")
}

// Parse returns the account id a fansly:// URL names.
func Parse(raw string) (string, error) {
	if !IsURL(raw) {
		return "", fmt.Errorf("not a fansly URL: %q", raw)
	}
	id := strings.TrimSuffix(strings.TrimPrefix(raw, Scheme+"://"), "/")
	if !accountPattern.MatchString(id) {
		return "", fmt.Errorf("fansly URL %q names no account", raw)
	}
	return id, nil
}

// Seat is a viewer watching a channel. Its playlist stays usable until Done is
// closed.
type Seat struct {
	// MasterURL is the HLS master playlist, signed for the seat's viewer.
	MasterURL string

	id        string
	api       *http.Client
	userAgent string
	login     map[string]string
	cancel    context.CancelFunc
	done      chan struct{}

	mu  sync.Mutex
	err error
}

// Open reads the channel raw names and watches it until ctx ends or Close is
// called. client makes the API calls; login, when not empty, holds the headers
// that sign them in as the recorder's user, and goes to the API alone.
func Open(ctx context.Context, raw string, client *http.Client, userAgent string, login map[string]string, logger *slog.Logger) (*Seat, error) {
	id, err := Parse(raw)
	if err != nil {
		return nil, err
	}
	if client == nil {
		client = http.DefaultClient
	}
	if userAgent == "" {
		userAgent = useragent.Fallback
	}
	s := &Seat{id: id, api: client, userAgent: userAgent, login: login, done: make(chan struct{})}

	openCtx, cancelOpen := context.WithTimeout(ctx, openTimeout)
	defer cancelOpen()
	playback, err := s.check(openCtx)
	if err != nil {
		return nil, err
	}
	s.MasterURL = playback
	holdCtx, cancel := context.WithCancel(ctx)
	s.cancel = cancel
	logger.Info("Watching a Fansly channel", "account", id, "signed_in", len(login) > 0)
	go s.hold(holdCtx, logger)
	return s, nil
}

// Client returns base: the signed playlist needs no cookies.
func (s *Seat) Client(base *http.Client) *http.Client { return base }

// Done is closed when the seat is lost or released.
func (s *Seat) Done() <-chan struct{} { return s.done }

// Err reports why the seat was lost: ErrEnded or ErrLocked when the stream
// ended or turned its viewer away, nil while it is held or after Close.
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

// hold re-reads the channel until ctx ends or the stream ends or turns its
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
		_, err := s.check(cctx)
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
			logger.Debug("Fansly channel read failed", "account", s.id, "error", err)
			continue
		}
		logger.Info("Leaving the Fansly channel", "account", s.id, "reason", err)
		s.mu.Lock()
		s.err = err
		s.mu.Unlock()
		return
	}
}

// check reads the channel: ErrEnded once its stream is off the air, ErrLocked
// while the viewer may not watch it, and otherwise the master playlist signed
// for the viewer.
func (s *Seat) check(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		apiOrigin+"/api/v1/streaming/channel/"+s.id+"?ngsw-bypass=true", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", s.userAgent)
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Origin", siteOrigin)
	req.Header.Set("Referer", siteOrigin+"/")
	for k, v := range s.login {
		req.Header.Set(k, v)
	}
	resp, err := s.api.Do(req)
	if err != nil {
		return "", fmt.Errorf("fansly channel: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return "", fmt.Errorf("fansly channel: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("fansly channel: status %d", resp.StatusCode)
	}
	var r struct {
		Success  bool `json:"success"`
		Response *struct {
			AccountID string `json:"accountId"`
			Stream    *struct {
				Status      int    `json:"status"`
				Access      bool   `json:"access"`
				PlaybackURL string `json:"playbackUrl"`
			} `json:"stream"`
		} `json:"response"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return "", fmt.Errorf("decode fansly channel: %w", err)
	}
	switch {
	case !r.Success:
		return "", fmt.Errorf("fansly channel refused: %.200s", body)
	case r.Response == nil || r.Response.Stream == nil:
		return "", ErrEnded
	case r.Response.AccountID != s.id:
		return "", fmt.Errorf("fansly channel answered for account %q", r.Response.AccountID)
	case r.Response.Stream.Status != streamLive:
		return "", ErrEnded
	case !r.Response.Stream.Access:
		return "", ErrLocked
	}
	playback := r.Response.Stream.PlaybackURL
	u, err := url.Parse(playback)
	if err != nil || u.Scheme != "https" || !onIVS(u.Hostname()) || u.Query().Get("token") == "" {
		return "", fmt.Errorf("fansly channel named no signed playlist: %.200s", playback)
	}
	return playback, nil
}

// onIVS reports whether host is Amazon IVS's playback edge.
func onIVS(host string) bool {
	return strings.HasSuffix(strings.ToLower(host), ".live-video.net")
}
