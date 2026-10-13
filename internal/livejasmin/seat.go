package livejasmin

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
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
)

const (
	siteOrigin = "https://www.livejasmin.com"
	// handshakeTO bounds everything from the room page to the stream's first
	// bytes.
	handshakeTO = 20 * time.Second
	// videoWait is how long a joined seat waits for the room to name its
	// stream. A room in a private or ticket show never does.
	videoWait = 10 * time.Second
	maxPage   = 4 << 20
	maxFrame  = 4 << 20
)

// errNoVideo is a room that admitted the seat but offered it no stream: a show
// a guest may not watch.
var errNoVideo = errors.New("livejasmin: the room offers guests no stream")

var (
	oneConnectionRe = regexp.MustCompile(`"oneConnection":\{"url":"([^"]+)","token":"([^"]+)"`)
	performerNickRe = regexp.MustCompile(`performerNick = '([^']+)'`)
)

// seat is one guest's place in a room: the room socket that holds it, and the
// stream the room handed it. The site ends a guest's seat after two minutes;
// Done closes then, or whenever the socket drops.
type seat struct {
	conn   *websocket.Conn
	body   io.ReadCloser
	cancel context.CancelFunc
	logger *slog.Logger

	joined time.Time
	done   chan struct{}
	once   sync.Once
	// reason is what the room said as it closed the seat.
	reason string
}

// Done is closed when the room has let the seat go.
func (s *seat) Done() <-chan struct{} { return s.done }

func (s *seat) end(reason string) {
	s.once.Do(func() {
		s.reason = reason
		close(s.done)
	})
}

// Close leaves the room.
func (s *seat) Close() {
	s.end("closed")
	s.cancel()
	if s.body != nil {
		s.body.Close()
	}
	s.conn.Close(websocket.StatusNormalClosure, "")
}

// openSeat takes a guest's seat in r's room and opens the stream it is
// offered. Every seat is a new guest: a fresh cookie jar, so a fresh session
// and socket token from the room page.
func openSeat(ctx context.Context, r Room, client *http.Client, userAgent string, logger *slog.Logger) (*seat, error) {
	jar, _ := cookiejar.New(nil)
	c := *client
	c.Jar = jar
	c.Timeout = 0

	hctx, hcancel := context.WithTimeout(ctx, handshakeTO)
	defer hcancel()

	page, err := get(hctx, &c, siteOrigin+"/en/chat/"+url.PathEscape(r.Name), userAgent)
	if err != nil {
		return nil, fmt.Errorf("room page: %w", err)
	}
	m := oneConnectionRe.FindSubmatch(page)
	nick := performerNickRe.FindSubmatch(page)
	if m == nil || nick == nil {
		return nil, ErrEnded
	}
	entry := strings.ReplaceAll(string(m[1]), `\/`, "/")
	u, err := url.Parse(entry)
	if err != nil || u.Scheme != "wss" || !strings.HasSuffix(u.Hostname(), ".dditsadn.com") {
		return nil, fmt.Errorf("room page names an unexpected socket %q", entry)
	}
	roomID := string(nick[1])

	sctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	conn, _, err := websocket.Dial(hctx, entry+"/socket.io/?EIO=4&transport=websocket&applicationId=oneconnection", &websocket.DialOptions{
		HTTPClient: &c,
		HTTPHeader: http.Header{"Origin": {siteOrigin}, "User-Agent": {userAgent}},
	})
	if err != nil {
		cancel()
		return nil, fmt.Errorf("dial room socket: %w", err)
	}
	conn.SetReadLimit(maxFrame)
	s := &seat{conn: conn, cancel: cancel, logger: logger, done: make(chan struct{})}
	stream, err := s.join(hctx, sctx, string(m[2]), roomID, r.Height, userAgent)
	if err != nil {
		s.Close()
		return nil, err
	}
	resp, err := openStream(hctx, sctx, &c, stream, userAgent)
	if err != nil {
		s.Close()
		return nil, err
	}
	s.body = resp
	return s, nil
}

// join connects the socket, joins the room and waits for the stream it
// offers, returning its HLS URL. From the socket's opening on, a reader
// answers the server's pings for the life of the seat.
func (s *seat) join(hctx, sctx context.Context, token, roomID string, height int, userAgent string) (string, error) {
	events := make(chan []byte, 16)
	go s.read(sctx, events)

	send := func(msg string) error {
		wctx, cancel := context.WithTimeout(hctx, handshakeTO)
		defer cancel()
		return s.conn.Write(wctx, websocket.MessageText, []byte(msg))
	}
	auth, _ := json.Marshal(map[string]any{
		"token":               token,
		"correlationId":       randomID(),
		"platform":            "desktop",
		"brandID":             "jasmin",
		"cobrandId":           "livejasmin",
		"subbrand":            "livejasmin",
		"siteName":            "LiveJasmin",
		"siteUrl":             siteOrigin,
		"chatHistoryRequired": false,
		"gravityCookieId":     "",
	})
	if height <= 0 {
		height = defaultHeight
	}
	join, _ := json.Marshal([]any{"join", map[string]any{
		"roomId":              roomID,
		"sendSharedObject":    []string{"data/chat_so"},
		"chatHistoryRequired": false,
		"isPassive":           false,
		"preferredResolution": map[string]any{"protocol": "h5live", "height": height, "correlationId": randomID()},
	}})

	var videoBy <-chan time.Time
	for {
		var msg []byte
		select {
		case msg = <-events:
		case <-videoBy:
			return "", errNoVideo
		case <-s.done:
			return "", fmt.Errorf("room socket closed while joining: %s", s.reason)
		case <-hctx.Done():
			return "", fmt.Errorf("join room: %w", hctx.Err())
		}
		switch {
		case len(msg) > 0 && msg[0] == '0':
			if err := send("40" + string(auth)); err != nil {
				return "", err
			}
		case strings.HasPrefix(string(msg), "44"):
			return "", fmt.Errorf("room socket refused the guest token: %.200s", msg)
		case strings.HasPrefix(string(msg), "40"):
			if err := send("420" + string(join)); err != nil {
				return "", err
			}
		case strings.HasPrefix(string(msg), "430"):
			var ack []struct {
				Error string `json:"error"`
			}
			if json.Unmarshal(msg[3:], &ack) == nil && len(ack) > 0 && ack[0].Error != "" {
				if strings.Contains(strings.ToLower(ack[0].Error), "left") {
					return "", ErrEnded
				}
				return "", fmt.Errorf("join room: %s", ack[0].Error)
			}
			s.joined = time.Now()
			videoBy = time.After(videoWait)
		default:
			name, args := event(msg)
			if name != "setVideoData" {
				continue
			}
			var data []struct {
				IsSuccess bool `json:"isSuccess"`
				Protocol  struct {
					H5Live struct {
						HLSURL string `json:"hlsUrl"`
					} `json:"h5live"`
				} `json:"protocol"`
			}
			if len(args) < 3 || json.Unmarshal(args[2], &data) != nil || len(data) == 0 {
				continue
			}
			if !data[0].IsSuccess || data[0].Protocol.H5Live.HLSURL == "" {
				return "", errNoVideo
			}
			return data[0].Protocol.H5Live.HLSURL, nil
		}
	}
}

// read drains the socket for the life of the seat: it answers pings, hands
// the handshake its messages, and ends the seat when the room closes it.
func (s *seat) read(ctx context.Context, events chan<- []byte) {
	for {
		_, msg, err := s.conn.Read(ctx)
		if err != nil {
			s.end("socket: " + err.Error())
			return
		}
		if string(msg) == "2" {
			wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			err := s.conn.Write(wctx, websocket.MessageText, []byte("3"))
			cancel()
			if err != nil {
				s.end("socket: " + err.Error())
				return
			}
			continue
		}
		switch name, args := event(msg); name {
		case "closeConnection":
			reason := ""
			if len(args) > 2 {
				_ = json.Unmarshal(args[2], &reason)
			}
			s.end(reason)
		case "part", "guestTimeoutStreamStopped":
			s.end(name)
		}
		select {
		case events <- msg:
		default:
			// Nobody is waiting on the handshake any more.
		}
	}
}

// event decodes a socket.io event packet, 42[name, args...].
func event(msg []byte) (string, []json.RawMessage) {
	if !strings.HasPrefix(string(msg), "42[") {
		return "", nil
	}
	var parts []json.RawMessage
	if json.Unmarshal(msg[2:], &parts) != nil || len(parts) == 0 {
		return "", nil
	}
	var name string
	_ = json.Unmarshal(parts[0], &name)
	return name, parts
}

// openStream reads the one-entry playlist the room offers and opens the
// entry, a fragmented MP4 that runs as long as the seat holds.
func openStream(hctx, sctx context.Context, c *http.Client, playlistURL, userAgent string) (io.ReadCloser, error) {
	pl, err := url.Parse(playlistURL)
	if err != nil || pl.Scheme != "https" || !strings.HasSuffix(pl.Hostname(), ".dditscdn.com") {
		return nil, fmt.Errorf("room names an unexpected stream %q", playlistURL)
	}
	body, err := get(hctx, c, playlistURL, userAgent)
	if err != nil {
		return nil, fmt.Errorf("stream playlist: %w", err)
	}
	var entry string
	sc := bufio.NewScanner(strings.NewReader(string(body)))
	for sc.Scan() {
		if line := strings.TrimSpace(sc.Text()); line != "" && !strings.HasPrefix(line, "#") {
			entry = line
			break
		}
	}
	if entry == "" {
		return nil, errors.New("stream playlist lists no entry")
	}
	ref, err := url.Parse(entry)
	if err != nil {
		return nil, fmt.Errorf("stream playlist entry: %w", err)
	}
	streamURL := pl.ResolveReference(ref)

	// The request lives with the seat; only reaching the first byte is bound
	// by the handshake's deadline.
	req, err := http.NewRequestWithContext(sctx, http.MethodGet, streamURL.String(), nil)
	if err != nil {
		return nil, err
	}
	setHeaders(req, userAgent)
	type result struct {
		resp *http.Response
		err  error
	}
	got := make(chan result, 1)
	go func() {
		resp, err := c.Do(req)
		got <- result{resp, err}
	}()
	var res result
	select {
	case res = <-got:
	case <-hctx.Done():
		go func() {
			if r := <-got; r.resp != nil {
				r.resp.Body.Close()
			}
		}()
		return nil, fmt.Errorf("open stream: %w", hctx.Err())
	}
	if res.err != nil {
		return nil, fmt.Errorf("open stream: %w", res.err)
	}
	if res.resp.StatusCode != http.StatusOK {
		res.resp.Body.Close()
		return nil, fmt.Errorf("open stream: status %d", res.resp.StatusCode)
	}
	return res.resp.Body, nil
}

func get(ctx context.Context, c *http.Client, rawURL, userAgent string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	setHeaders(req, userAgent)
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, maxPage))
}

func setHeaders(req *http.Request, userAgent string) {
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Origin", siteOrigin)
	req.Header.Set("Referer", siteOrigin+"/")
}

func randomID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
