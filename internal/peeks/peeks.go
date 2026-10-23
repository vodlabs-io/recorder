// Package peeks takes a seat on a Peeks Social broadcast, for recording it.
//
// Peeks grants its HLS to a viewer who joins the stream (stream-join): MPEG-TS
// from a Wowza origin behind CDN77, under a path token good for four hours.
// The broadcaster may start charging for the stream at any point, and the CDN
// keeps serving a granted playlist after that, so a broadcast is named by a
// peeks://<stream id> URL, and whoever records it joins the stream itself and
// re-reads it while it records: the seat ends once the stream is over or asks
// for payment, and gives way ahead of its token's expiry so the next attempt
// joins again.
package peeks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/vodlabs-io/recorder/internal/useragent"
)

// Scheme names a broadcast to record.
const Scheme = "peeks"

// siteOrigin is the site; a variable so tests can point it at a fake.
var siteOrigin = "https://www.peeks.com"

// watchEvery is how often the stream is re-read while the seat is held.
var watchEvery = 10 * time.Second

const (
	// openTimeout bounds taking a seat, from the stream read to the grant.
	openTimeout = 30 * time.Second
	// requestTimeout bounds one API call.
	requestTimeout = 15 * time.Second
	// renewAhead is how long before its token expires a seat gives way.
	renewAhead = 5 * time.Minute
	maxBody    = 1 << 20
)

var (
	// ErrEnded reports a broadcast that ended, or was not on, while a seat was
	// held or taken.
	ErrEnded = errors.New("peeks broadcast ended")
	// ErrPaid reports a broadcast that asks a viewer for payment.
	ErrPaid = errors.New("peeks broadcast asks for payment")
	// errExpiring reports a seat whose token is about to expire.
	errExpiring = errors.New("peeks stream token expiring")
)

var (
	streamPattern = regexp.MustCompile(`^[0-9]{1,20}$`)
	csrfPattern   = regexp.MustCompile(`<meta name="csrf-token" content="([^"]+)"`)
	// tokenExpiry reads the expiry a stream URL's path token carries:
	// /<token>,<unix seconds>/.
	tokenExpiry = regexp.MustCompile(`^/[^/]*,([0-9]{9,11})/`)
)

// IsURL reports whether u names a Peeks broadcast.
func IsURL(u string) bool {
	return strings.HasPrefix(u, Scheme+"://")
}

// Parse returns the stream id a peeks:// URL names.
func Parse(raw string) (string, error) {
	if !IsURL(raw) {
		return "", fmt.Errorf("not a peeks URL: %q", raw)
	}
	id := strings.TrimSuffix(strings.TrimPrefix(raw, Scheme+"://"), "/")
	if !streamPattern.MatchString(id) {
		return "", fmt.Errorf("peeks URL %q names no stream", raw)
	}
	return id, nil
}

// Seat is a viewer joined to a stream. Its playlist stays usable until Done
// is closed.
type Seat struct {
	// MasterURL is the HLS master playlist the seat was granted.
	MasterURL string

	id        string
	api       *http.Client
	userAgent string
	csrf      string
	expires   time.Time
	cancel    context.CancelFunc
	done      chan struct{}

	mu  sync.Mutex
	err error
}

// Open joins the stream raw names, as an anonymous viewer, and watches it
// until ctx ends or Close is called. client makes the API calls.
func Open(ctx context.Context, raw string, client *http.Client, userAgent string, logger *slog.Logger) (*Seat, error) {
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
	// The join needs the session cookie the site hands out with its pages,
	// kept apart from the shared client's.
	jar, _ := cookiejar.New(nil)
	api := *client
	api.Jar = jar
	s := &Seat{id: id, api: &api, userAgent: userAgent, done: make(chan struct{})}

	openCtx, cancelOpen := context.WithTimeout(ctx, openTimeout)
	defer cancelOpen()
	if err := s.check(openCtx); err != nil {
		return nil, err
	}
	if err := s.session(openCtx); err != nil {
		return nil, err
	}
	if err := s.join(openCtx); err != nil {
		return nil, err
	}
	holdCtx, cancel := context.WithCancel(ctx)
	s.cancel = cancel
	logger.Info("Joined a Peeks stream", "stream", id, "token_expires", s.expires.Format(time.RFC3339))
	go s.hold(holdCtx, logger)
	return s, nil
}

// Client returns base: the granted playlist needs no cookies.
func (s *Seat) Client(base *http.Client) *http.Client { return base }

// Done is closed when the seat is lost or released.
func (s *Seat) Done() <-chan struct{} { return s.done }

// Err reports why the seat was lost: ErrEnded or ErrPaid when the broadcast
// ended or asked for payment, nil while it is held or after Close.
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

// hold re-reads the stream until ctx ends, the stream ends or turns paid, or
// the token is about to expire, then leaves it.
func (s *Seat) hold(ctx context.Context, logger *slog.Logger) {
	defer close(s.done)
	defer s.leave()

	t := time.NewTicker(watchEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		var err error
		if !s.expires.IsZero() && time.Until(s.expires) < renewAhead {
			err = errExpiring
		} else {
			cctx, cancel := context.WithTimeout(ctx, requestTimeout)
			err = s.check(cctx)
			cancel()
			if ctx.Err() != nil {
				return
			}
			if err != nil && !errors.Is(err, ErrEnded) && !errors.Is(err, ErrPaid) {
				// A failed read says nothing about the stream; the playlist
				// itself shows a broadcast that is gone.
				logger.Debug("Peeks stream read failed", "stream", s.id, "error", err)
				continue
			}
		}
		if err == nil {
			continue
		}
		logger.Info("Leaving the Peeks stream", "stream", s.id, "reason", err)
		s.mu.Lock()
		s.err = err
		s.mu.Unlock()
		return
	}
}

// check reads the stream: ErrEnded once it is over, ErrPaid while it asks
// for payment or plays only to followers or subscribers.
func (s *Seat) check(ctx context.Context) error {
	var r struct {
		Response string `json:"response"`
		Message  string `json:"message"`
		Stream   *struct {
			Live     string      `json:"live"`
			Audience string      `json:"audience"`
			MinTip   json.Number `json:"tip_viewer_min_amount"`
		} `json:"stream"`
	}
	body, err := s.do(ctx, http.MethodGet, "/api/streams/stream-lookup?"+url.Values{"id": {s.id}}.Encode(), nil)
	if err != nil {
		return fmt.Errorf("peeks stream lookup: %w", err)
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return fmt.Errorf("decode peeks stream lookup: %w", err)
	}
	switch {
	case r.Response == "error" && strings.EqualFold(r.Message, "Stream not found"):
		return ErrEnded
	case r.Response != "success" || r.Stream == nil:
		return fmt.Errorf("peeks stream lookup: %s %s", r.Response, r.Message)
	case r.Stream.Live != "Y":
		return ErrEnded
	case paid(r.Stream.MinTip) || !strings.EqualFold(r.Stream.Audience, "PUBLIC"):
		return ErrPaid
	}
	return nil
}

func paid(n json.Number) bool {
	f, err := strconv.ParseFloat(n.String(), 64)
	return err == nil && f > 0
}

// session reads a page, for the session cookie and the CSRF token the API's
// POSTs need.
func (s *Seat) session(ctx context.Context) error {
	body, err := s.do(ctx, http.MethodGet, "/", nil)
	if err != nil {
		return fmt.Errorf("peeks session: %w", err)
	}
	m := csrfPattern.FindSubmatch(body)
	if m == nil {
		return errors.New("peeks page carries no CSRF token")
	}
	s.csrf = string(m[1])
	return nil
}

// join joins the stream and takes the HLS master it is granted.
func (s *Seat) join(ctx context.Context) error {
	id, _ := strconv.ParseInt(s.id, 10, 64)
	payload, _ := json.Marshal(map[string]any{"stream_id": id, "isHls": true})
	body, err := s.do(ctx, http.MethodPost, "/api/streams/stream-join", payload)
	if err != nil {
		return fmt.Errorf("peeks stream join: %w", err)
	}
	var r struct {
		Response string `json:"response"`
		Message  string `json:"message"`
		URL      struct {
			HLS string `json:"stream_hls"`
		} `json:"url"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return fmt.Errorf("decode peeks stream join: %w", err)
	}
	if r.Response != "success" {
		if strings.Contains(strings.ToLower(r.Message), "payment") {
			return ErrPaid
		}
		return fmt.Errorf("peeks stream join refused: %s", r.Message)
	}
	master := r.URL.HLS
	if strings.HasPrefix(master, "//") {
		master = "https:" + master
	}
	u, err := url.Parse(master)
	if err != nil || u.Scheme != "https" || !onPeeks(u.Hostname()) {
		return fmt.Errorf("peeks stream join named no playlist: %.200s", body)
	}
	s.MasterURL = master
	if m := tokenExpiry.FindStringSubmatch(u.EscapedPath()); m != nil {
		if sec, err := strconv.ParseInt(m[1], 10, 64); err == nil {
			s.expires = time.Unix(sec, 0)
		}
	}
	return nil
}

// leave tells the site the viewer left, as its player does.
func (s *Seat) leave() {
	if s.csrf == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	id, _ := strconv.ParseInt(s.id, 10, 64)
	payload, _ := json.Marshal(map[string]any{"id": id, "likes": 0})
	_, _ = s.do(ctx, http.MethodPost, "/api/streams/leave-stream", payload)
}

func (s *Seat) do(ctx context.Context, method, path string, payload []byte) ([]byte, error) {
	var rd io.Reader
	if payload != nil {
		rd = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, siteOrigin+path, rd)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", s.userAgent)
	req.Header.Set("Referer", siteOrigin+"/streams/stream-view?id="+s.id)
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/plain, */*")
		req.Header.Set("Origin", siteOrigin)
		req.Header.Set("X-Requested-With", "XMLHttpRequest")
		req.Header.Set("X-CSRF-Token", s.csrf)
	}
	resp, err := s.api.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	return body, nil
}

func onPeeks(host string) bool {
	host = strings.ToLower(host)
	return host == "peeks.com" || strings.HasSuffix(host, ".peeks.com")
}
