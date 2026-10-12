// Package nicolive takes a seat on a Niconico Live program, for recording it.
//
// Niconico grants its HLS per seat: a viewer's watch socket (named by the
// program's watch page) is answered with a master playlist and the cookies its
// requests must carry, CloudFront's scoped by path and a key cookie that stops
// working shortly after the seat is given up. So a program is named by a
// nicolive://lv<number> URL, and whoever records it holds a seat of its own
// for as long as it reads the playlist.
package nicolive

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/vodlabs-io/recorder/internal/useragent"
)

// Scheme names a program to record.
const Scheme = "nicolive"

const (
	watchURL   = "https://live.nicovideo.jp/watch/"
	siteOrigin = "https://live.nicovideo.jp"
	// openTimeout bounds taking a seat, from the watch page to the stream.
	openTimeout = 30 * time.Second
	// maxPage bounds the watch page, about 50 KB.
	maxPage = 2 << 20
	// defaultKeepInterval is used until the socket names its own.
	defaultKeepInterval = 30 * time.Second
)

// ErrEnded reports a program that ended while a seat was held.
var ErrEnded = errors.New("niconico program ended")

var programPattern = regexp.MustCompile(`^lv[1-9][0-9]{0,11}$`)

var embeddedData = regexp.MustCompile(`id="embedded-data" data-props="([^"]*)"`)

// IsURL reports whether u names a Niconico program.
func IsURL(u string) bool {
	return strings.HasPrefix(u, Scheme+"://")
}

// Parse returns the program id a nicolive:// URL names.
func Parse(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != Scheme {
		return "", fmt.Errorf("not a nicolive URL: %q", raw)
	}
	if !programPattern.MatchString(u.Host) {
		return "", fmt.Errorf("nicolive URL %q names no program", raw)
	}
	return u.Host, nil
}

// Seat is a held seat on a program. Its playlist and cookies stay usable
// until Done is closed.
type Seat struct {
	// MasterURL is the HLS master playlist the seat was granted.
	MasterURL string

	conn    *websocket.Conn
	cookies []*http.Cookie
	cancel  context.CancelFunc
	done    chan struct{}

	mu  sync.Mutex
	err error
}

// Open takes a seat on the program raw names, as an anonymous viewer, and
// holds it until ctx ends or Close is called. client fetches the watch page and
// dials the socket.
func Open(ctx context.Context, raw string, client *http.Client, userAgent string, logger *slog.Logger) (*Seat, error) {
	lv, err := Parse(raw)
	if err != nil {
		return nil, err
	}
	if client == nil {
		client = http.DefaultClient
	}
	if userAgent == "" {
		userAgent = useragent.Fallback
	}
	// The socket's lifetime is bounded by context; a client timeout would cut
	// it.
	dialClient := *client
	dialClient.Timeout = 0
	dialClient.Jar = nil

	openCtx, cancelOpen := context.WithTimeout(ctx, openTimeout)
	defer cancelOpen()
	wsURL, err := seatURL(openCtx, &dialClient, lv, userAgent)
	if err != nil {
		return nil, err
	}
	conn, _, err := websocket.Dial(openCtx, wsURL, &websocket.DialOptions{
		HTTPClient: &dialClient,
		HTTPHeader: http.Header{"Origin": {siteOrigin}, "User-Agent": {userAgent}},
	})
	if err != nil {
		return nil, fmt.Errorf("dial niconico watch socket: %w", err)
	}
	conn.SetReadLimit(1 << 20)
	start := `{"type":"startWatching","data":{"stream":{"quality":"abr","protocol":"hls","latency":"high","chasePlay":false},"room":{"protocol":"webSocket","commentable":false},"reconnect":false}}`
	if err := conn.Write(openCtx, websocket.MessageText, []byte(start)); err != nil {
		conn.CloseNow()
		return nil, fmt.Errorf("start watching: %w", err)
	}

	keep := defaultKeepInterval
	var stream streamGrant
	for stream.URI == "" {
		m, err := readMessage(openCtx, conn)
		if err != nil {
			conn.CloseNow()
			return nil, err
		}
		switch m.Type {
		case "seat":
			var d struct {
				KeepIntervalSec int `json:"keepIntervalSec"`
			}
			if json.Unmarshal(m.Data, &d) == nil && d.KeepIntervalSec > 0 {
				keep = time.Duration(d.KeepIntervalSec) * time.Second
			}
		case "stream":
			if err := json.Unmarshal(m.Data, &stream); err != nil {
				conn.CloseNow()
				return nil, fmt.Errorf("decode stream grant: %w", err)
			}
			if err := checkStreamURL(stream.URI); err != nil {
				conn.CloseNow()
				return nil, err
			}
		case "ping":
			if err := pong(openCtx, conn); err != nil {
				conn.CloseNow()
				return nil, err
			}
		}
	}

	holdCtx, cancel := context.WithCancel(ctx)
	s := &Seat{
		MasterURL: stream.URI,
		conn:      conn,
		cookies:   stream.httpCookies(),
		cancel:    cancel,
		done:      make(chan struct{}),
	}
	logger.Info("Took a Niconico seat", "program", lv, "quality", stream.Quality,
		"available", strings.Join(stream.AvailableQualities, ","), "keep_interval", keep)
	go s.hold(holdCtx, keep, logger)
	return s, nil
}

// Client returns base with a cookie jar of its own holding the seat's cookies,
// each scoped to its path as the socket granted it.
func (s *Seat) Client(base *http.Client) *http.Client {
	if base == nil {
		base = http.DefaultClient
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		return base
	}
	if u, err := url.Parse(s.MasterURL); err == nil {
		jar.SetCookies(u, s.cookies)
	}
	c := *base
	c.Jar = jar
	return &c
}

// Done is closed when the seat is lost or released.
func (s *Seat) Done() <-chan struct{} { return s.done }

// Err reports why the seat was lost: ErrEnded when the program ended, nil
// while it is held or after Close.
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

// hold answers the socket's pings and keeps the seat on its interval until
// ctx ends or the socket is lost.
func (s *Seat) hold(ctx context.Context, keep time.Duration, logger *slog.Logger) {
	defer close(s.done)
	defer s.conn.Close(websocket.StatusNormalClosure, "")

	msgs := make(chan socketMsg)
	readErr := make(chan error, 1)
	go func() {
		for {
			m, err := readMessage(ctx, s.conn)
			if err != nil {
				readErr <- err
				return
			}
			select {
			case msgs <- m:
			case <-ctx.Done():
				return
			}
		}
	}()

	t := time.NewTicker(keep)
	defer t.Stop()
	for {
		var err error
		select {
		case <-ctx.Done():
			return
		case err = <-readErr:
		case <-t.C:
			err = s.conn.Write(ctx, websocket.MessageText, []byte(`{"type":"keepSeat"}`))
		case m := <-msgs:
			switch m.Type {
			case "ping":
				err = pong(ctx, s.conn)
			case "reconnect":
				err = errors.New("niconico asked the seat to reconnect")
			}
		}
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			if !errors.Is(err, ErrEnded) {
				logger.Warn("Lost the Niconico seat", "error", err)
			}
			s.mu.Lock()
			s.err = err
			s.mu.Unlock()
			return
		}
	}
}

// seatURL reads the program's watch page for this viewer's watch socket.
func seatURL(ctx context.Context, client *http.Client, lv, userAgent string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, watchURL+lv, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("niconico watch page: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxPage))
	if err != nil {
		return "", fmt.Errorf("niconico watch page: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("niconico watch page: status %d", resp.StatusCode)
	}
	m := embeddedData.FindSubmatch(body)
	if m == nil {
		return "", errors.New("niconico watch page has no embedded data")
	}
	var d struct {
		Site struct {
			Relive struct {
				WebSocketURL string `json:"webSocketUrl"`
			} `json:"relive"`
		} `json:"site"`
		Program struct {
			Status string `json:"status"`
		} `json:"program"`
	}
	if err := json.Unmarshal([]byte(html.UnescapeString(string(m[1]))), &d); err != nil {
		return "", fmt.Errorf("decode niconico watch page: %w", err)
	}
	if d.Program.Status == "ENDED" {
		return "", ErrEnded
	}
	ws := d.Site.Relive.WebSocketURL
	u, err := url.Parse(ws)
	if ws == "" || err != nil || u.Scheme != "wss" || !onNicovideo(u.Hostname()) {
		return "", fmt.Errorf("niconico program %s offers no seat", lv)
	}
	return ws, nil
}

type socketMsg struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

// readMessage reads one socket message, mapping a disconnect to an error:
// ErrEnded when the program ended.
func readMessage(ctx context.Context, conn *websocket.Conn) (socketMsg, error) {
	for {
		typ, data, err := conn.Read(ctx)
		if err != nil {
			return socketMsg{}, fmt.Errorf("niconico watch socket: %w", err)
		}
		if typ != websocket.MessageText {
			continue
		}
		var m socketMsg
		if json.Unmarshal(data, &m) != nil {
			continue
		}
		switch m.Type {
		case "disconnect":
			var d struct {
				Reason string `json:"reason"`
			}
			_ = json.Unmarshal(m.Data, &d)
			if d.Reason == "END_PROGRAM" {
				return m, ErrEnded
			}
			return m, fmt.Errorf("niconico watch socket disconnected: %s", d.Reason)
		case "error":
			return m, fmt.Errorf("niconico watch socket error: %s", m.Data)
		}
		return m, nil
	}
}

// pong answers a ping and keeps the seat, as the watch page does.
func pong(ctx context.Context, conn *websocket.Conn) error {
	if err := conn.Write(ctx, websocket.MessageText, []byte(`{"type":"pong"}`)); err != nil {
		return err
	}
	return conn.Write(ctx, websocket.MessageText, []byte(`{"type":"keepSeat"}`))
}

// streamGrant is the socket's stream message.
type streamGrant struct {
	URI                string        `json:"uri"`
	Quality            string        `json:"quality"`
	AvailableQualities []string      `json:"availableQualities"`
	Cookies            []grantCookie `json:"cookies"`
}

type grantCookie struct {
	Name    string `json:"name"`
	Value   string `json:"value"`
	Domain  string `json:"domain"`
	Path    string `json:"path"`
	Expires string `json:"expires"`
	Secure  bool   `json:"secure"`
}

// httpCookies converts the grant's cookies, dropping any for another site.
func (g streamGrant) httpCookies() []*http.Cookie {
	out := make([]*http.Cookie, 0, len(g.Cookies))
	for _, c := range g.Cookies {
		if c.Name == "" || !onNicovideo(strings.TrimPrefix(c.Domain, ".")) {
			continue
		}
		hc := &http.Cookie{Name: c.Name, Value: c.Value, Domain: c.Domain, Path: c.Path, Secure: c.Secure}
		if t, err := http.ParseTime(c.Expires); err == nil {
			hc.Expires = t
		}
		out = append(out, hc)
	}
	return out
}

func checkStreamURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || !onNicovideo(u.Hostname()) {
		return fmt.Errorf("niconico stream URL %q is not on nicovideo.jp", raw)
	}
	return nil
}

func onNicovideo(host string) bool {
	host = strings.ToLower(host)
	return host == "nicovideo.jp" || strings.HasSuffix(host, ".nicovideo.jp")
}
