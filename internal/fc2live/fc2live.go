// Package fc2live takes a seat on an FC2 Live channel, for recording it.
//
// FC2 Live grants its HLS per viewer: the channel's control socket answers a
// viewer with playlists that work only once read while that viewer holds the
// socket, and whose segments are signed for the address that read them. So a
// channel is named by an fc2live://<channel id> URL, and whoever records it
// holds a control socket of its own for as long as it reads the playlist.
package fc2live

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

	"github.com/coder/websocket"
	"github.com/vodlabs-io/recorder/internal/useragent"
)

// Scheme names a channel to record.
const Scheme = "fc2live"

const (
	siteOrigin  = "https://live.fc2.com"
	memberPath  = "/api/memberApi.php"
	controlPath = "/api/getControlServer.php"
	// ageCheck is the cookie the site's age gate sets; adult channels turn an
	// anonymous viewer away without it.
	ageCheck      = "age_check=1"
	clientVersion = "2.1.0 [1]"

	// openTimeout bounds taking a seat, from the channel read to the
	// playlist grant.
	openTimeout = 45 * time.Second
	// heartbeatEvery is the site player's heartbeat interval.
	heartbeatEvery = 30 * time.Second
	// idleTimeout is how long the socket may stay silent; heartbeats are
	// answered, so silence means it is gone.
	idleTimeout = 90 * time.Second
	// grantRetry spaces asks for the playlists while a just-started
	// broadcast is still being converted.
	grantRetry = 3 * time.Second
	maxBody    = 1 << 20
)

// ErrEnded reports a broadcast that ended, or was not on, while a seat was
// held or taken.
var ErrEnded = errors.New("fc2 live broadcast ended")

var channelPattern = regexp.MustCompile(`^(2_)?[0-9]{1,12}$`)

// IsURL reports whether u names an FC2 Live channel.
func IsURL(u string) bool {
	return strings.HasPrefix(u, Scheme+"://")
}

// Parse returns the channel id an fc2live:// URL names.
func Parse(raw string) (string, error) {
	if !IsURL(raw) {
		return "", fmt.Errorf("not an fc2live URL: %q", raw)
	}
	id := strings.TrimSuffix(strings.TrimPrefix(raw, Scheme+"://"), "/")
	if !channelPattern.MatchString(id) {
		return "", fmt.Errorf("fc2live URL %q names no channel", raw)
	}
	return id, nil
}

// Seat is a held control socket on a channel. Its playlists stay usable
// until Done is closed.
type Seat struct {
	// MasterURL is the HLS master playlist the seat was granted.
	MasterURL string

	conn   *websocket.Conn
	cancel context.CancelFunc
	done   chan struct{}

	mu  sync.Mutex
	err error
}

// Open takes a seat on the channel raw names, as an anonymous viewer, and
// holds it until ctx ends or Close is called. client makes the API calls and
// dials the socket.
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
	// The socket's lifetime is bounded by context; a client timeout would cut
	// it.
	dialClient := *client
	dialClient.Timeout = 0
	dialClient.Jar = nil

	openCtx, cancelOpen := context.WithTimeout(ctx, openTimeout)
	defer cancelOpen()
	version, err := broadcastVersion(openCtx, &dialClient, id, userAgent)
	if err != nil {
		return nil, err
	}
	wsURL, err := controlSocket(openCtx, &dialClient, id, version, userAgent)
	if err != nil {
		return nil, err
	}
	conn, _, err := websocket.Dial(openCtx, wsURL, &websocket.DialOptions{
		HTTPClient: &dialClient,
		HTTPHeader: http.Header{"Origin": {siteOrigin}, "User-Agent": {userAgent}},
	})
	if err != nil {
		return nil, fmt.Errorf("dial fc2 live control socket: %w", err)
	}
	conn.SetReadLimit(4 << 20)

	s := &Seat{conn: conn, done: make(chan struct{})}
	s.MasterURL, err = s.grant(openCtx)
	if err != nil {
		conn.CloseNow()
		return nil, err
	}
	holdCtx, cancel := context.WithCancel(ctx)
	s.cancel = cancel
	logger.Info("Took an FC2 Live seat", "channel", id)
	go s.hold(holdCtx, logger)
	return s, nil
}

// Client returns base: the seat's playlists need no cookies.
func (s *Seat) Client(base *http.Client) *http.Client { return base }

// Done is closed when the seat is lost or released.
func (s *Seat) Done() <-chan struct{} { return s.done }

// Err reports why the seat was lost: ErrEnded when the broadcast ended, nil
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

// grant waits for the socket to accept the viewer, then asks for the
// playlists until a converted one is offered, and returns the best master:
// the high-latency one (2 s segments), else the low-latency one.
func (s *Seat) grant(ctx context.Context) (string, error) {
	for {
		m, err := readMessage(ctx, s.conn)
		if err != nil {
			return "", err
		}
		if m.Name == "connect_complete" {
			break
		}
	}
	for id := 1; ; id++ {
		req := fmt.Sprintf(`{"name":"get_hls_information","arguments":{},"id":%d}`, id)
		if err := s.conn.Write(ctx, websocket.MessageText, []byte(req)); err != nil {
			return "", fmt.Errorf("ask for fc2 live playlists: %w", err)
		}
		var info hlsInformation
		for {
			m, err := readMessage(ctx, s.conn)
			if err != nil {
				return "", err
			}
			if m.Name == "_response_" && m.ID == id {
				if err := json.Unmarshal(m.Arguments, &info); err != nil {
					return "", fmt.Errorf("decode fc2 live playlists: %w", err)
				}
				break
			}
		}
		if info.Status != 0 {
			return "", fmt.Errorf("fc2 live playlists refused: status %d", info.Status)
		}
		for _, list := range [][]playlist{info.HighLatency, info.Playlists, info.MiddleLatency} {
			for _, p := range list {
				if p.Mode < 10 && p.Status == 0 {
					if err := checkStreamURL(p.URL); err != nil {
						return "", err
					}
					return p.URL, nil
				}
			}
		}
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("fc2 live offered no converted playlist: %w", ctx.Err())
		case <-time.After(grantRetry):
		}
	}
}

// hold sends the heartbeat until ctx ends or the socket is lost.
func (s *Seat) hold(ctx context.Context, logger *slog.Logger) {
	defer close(s.done)
	defer s.conn.Close(websocket.StatusNormalClosure, "")

	readErr := make(chan error, 1)
	go func() {
		for {
			rctx, cancel := context.WithTimeout(ctx, idleTimeout)
			_, err := readMessage(rctx, s.conn)
			cancel()
			if err != nil {
				readErr <- err
				return
			}
		}
	}()

	t := time.NewTicker(heartbeatEvery)
	defer t.Stop()
	for n := 1000; ; n++ {
		var err error
		select {
		case <-ctx.Done():
			return
		case err = <-readErr:
		case <-t.C:
			hb := fmt.Sprintf(`{"name":"heartbeat","arguments":{},"id":%d}`, n)
			err = s.conn.Write(ctx, websocket.MessageText, []byte(hb))
		}
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			if !errors.Is(err, ErrEnded) {
				logger.Warn("Lost the FC2 Live seat", "error", err)
			}
			s.mu.Lock()
			s.err = err
			s.mu.Unlock()
			return
		}
	}
}

type socketMsg struct {
	Name      string          `json:"name"`
	ID        int             `json:"id"`
	Arguments json.RawMessage `json:"arguments"`
}

type playlist struct {
	Mode   int    `json:"mode"`
	Status int    `json:"status"`
	URL    string `json:"url"`
}

// hlsInformation is the answer to get_hls_information. A playlist's mode is
// its quality in tens (9 audio only) and its latency in units; modes 0-2 are
// masters over every quality.
type hlsInformation struct {
	Status        int        `json:"status"`
	Playlists     []playlist `json:"playlists"`
	HighLatency   []playlist `json:"playlists_high_latency"`
	MiddleLatency []playlist `json:"playlists_middle_latency"`
}

// readMessage reads one socket message, mapping the broadcast's end to
// ErrEnded and a disconnection to an error.
func readMessage(ctx context.Context, conn *websocket.Conn) (socketMsg, error) {
	for {
		typ, data, err := conn.Read(ctx)
		if err != nil {
			return socketMsg{}, fmt.Errorf("fc2 live control socket: %w", err)
		}
		if typ != websocket.MessageText {
			continue
		}
		var m socketMsg
		if json.Unmarshal(data, &m) != nil {
			continue
		}
		switch m.Name {
		case "publish_stop":
			return m, ErrEnded
		case "initial_connect":
			var a struct {
				Publish bool `json:"publish"`
			}
			if json.Unmarshal(m.Arguments, &a) == nil && !a.Publish {
				return m, ErrEnded
			}
		case "control_disconnection":
			var a struct {
				Code int `json:"code"`
			}
			_ = json.Unmarshal(m.Arguments, &a)
			return m, fmt.Errorf("fc2 live control socket disconnected: code %d", a.Code)
		}
		return m, nil
	}
}

// broadcastVersion reads the channel for its current broadcast's version.
func broadcastVersion(ctx context.Context, client *http.Client, id, userAgent string) (string, error) {
	body, err := post(ctx, client, memberPath, id, userAgent, url.Values{
		"channel": {"1"}, "profile": {"1"}, "user": {"1"}, "streamid": {id},
	})
	if err != nil {
		return "", fmt.Errorf("fc2 live channel: %w", err)
	}
	var r struct {
		Data struct {
			Channel struct {
				IsPublish int    `json:"is_publish"`
				Version   string `json:"version"`
			} `json:"channel_data"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return "", fmt.Errorf("decode fc2 live channel: %w", err)
	}
	if r.Data.Channel.IsPublish == 0 {
		return "", ErrEnded
	}
	return r.Data.Channel.Version, nil
}

// controlSocket asks for the channel's control socket.
func controlSocket(ctx context.Context, client *http.Client, id, version, userAgent string) (string, error) {
	body, err := post(ctx, client, controlPath, id, userAgent, url.Values{
		"channel_id": {id}, "mode": {"play"}, "orz": {""}, "channel_version": {version},
		"client_version": {clientVersion}, "client_type": {"pc"}, "client_app": {"browser_hls"}, "ipv6": {""},
	})
	if err != nil {
		return "", fmt.Errorf("fc2 live control server: %w", err)
	}
	var c struct {
		URL   string `json:"url"`
		Token string `json:"control_token"`
	}
	if err := json.Unmarshal(body, &c); err != nil {
		return "", fmt.Errorf("decode fc2 live control server: %w", err)
	}
	u, err := url.Parse(c.URL)
	if c.URL == "" || err != nil || u.Scheme != "wss" || !onFC2(u.Hostname()) || c.Token == "" {
		return "", fmt.Errorf("fc2 live control server refused: %.200s", body)
	}
	q := u.Query()
	q.Set("control_token", c.Token)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

func post(ctx context.Context, client *http.Client, path, id, userAgent string, form url.Values) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, siteOrigin+path, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Referer", siteOrigin+"/"+id+"/")
	req.Header.Set("Cookie", ageCheck)
	resp, err := client.Do(req)
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

func checkStreamURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || !onFC2(u.Hostname()) {
		return fmt.Errorf("fc2 live stream URL %q is not on fc2.com", raw)
	}
	return nil
}

func onFC2(host string) bool {
	host = strings.ToLower(host)
	return host == "fc2.com" || strings.HasSuffix(host, ".fc2.com")
}
