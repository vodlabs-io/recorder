// Package xcams records an XCams (xcams.com) room as an FLV stream.
//
// The site plays a model's free chat over WebRTC from a Janus server. A viewer
// first takes a guest seat in the room over the site's visitor socket, which
// hands it a visitor hash, and the Janus server plays the model's mountpoint
// to that hash. So an xcams:// URL names the model's account, and the recorder
// takes a seat of its own each time it opens it. It holds the seat while it
// plays, and ends the recording once the model leaves the free chat.
package xcams

import (
	"context"
	"encoding/base64"
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
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/vodlabs-io/recorder/internal/janus"
)

// Scheme names a room the recorder takes a seat in itself.
const Scheme = "xcams"

// Endpoints the site's player uses; variables so tests can stand in for them.
var (
	socketURL      = "wss://svc.dnxnetwork.lu/socket.io/?EIO=3&transport=websocket"
	janusEndpoint  = "https://rtcweb.xmodels-live.ch/janus/"
	siteOrigin     = "https://www.xcams.com"
	pingEvery      = 10 * time.Second
	transportPing  = 25 * time.Second
	joinTimeout    = 15 * time.Second
	seatAttempts   = 2
	errNotFreeChat = errors.New("xcams: the room is not in its free chat")
)

// userAgent is the browser the seat presents itself as.
const userAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140.0.0.0 Safari/537.36"

var accountPattern = regexp.MustCompile(`^c[0-9]{1,12}$`)

// Room is an xcams:// URL: the model's account id, and the nickname it goes
// by (for the logs and the Referer).
//
// Its form is xcams://<account>?nick=<nickname>.
type Room struct {
	Account string
	Nick    string
}

// IsURL reports whether u is an xcams:// URL.
func IsURL(u string) bool {
	return strings.HasPrefix(u, Scheme+"://")
}

// Parse decodes an xcams:// URL.
func Parse(raw string) (Room, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return Room{}, fmt.Errorf("parse xcams url: %w", err)
	}
	if u.Scheme != Scheme {
		return Room{}, fmt.Errorf("not an xcams url: scheme %q", u.Scheme)
	}
	r := Room{Account: u.Host, Nick: u.Query().Get("nick")}
	if !accountPattern.MatchString(r.Account) {
		return Room{}, errors.New("xcams url lacks a model account")
	}
	return r, nil
}

// URL encodes r as an xcams:// URL.
func (r Room) URL() string {
	u := Scheme + "://" + r.Account
	if r.Nick != "" {
		u += "?" + url.Values{"nick": {r.Nick}}.Encode()
	}
	return u
}

// Open takes a seat in the room u names and returns its broadcast as an FLV
// stream on a timeline starting at zero. Reading ends with io.EOF when the
// model leaves the free chat or the broadcast ends, or with the error that
// ended the playback. Close, or the end of ctx, stops it.
func Open(ctx context.Context, u string, logger *slog.Logger) (io.ReadCloser, error) {
	r, err := Parse(u)
	if err != nil {
		return nil, err
	}
	logger = logger.With("xcams_account", r.Account, "xcams_nick", r.Nick)
	var errs []error
	for range seatAttempts {
		st, err := takeSeat(ctx, r, logger)
		if err != nil {
			return nil, err
		}
		stream, err := janus.Play(ctx, st.grant(r), logger)
		if err == nil {
			return st.watch(stream), nil
		}
		st.close()
		errs = append(errs, err)
		if ctx.Err() != nil || errors.Is(err, janus.ErrNoStream) {
			break
		}
		logger.Debug("Janus playback failed, taking another seat", "error", err)
	}
	return nil, errors.Join(errs...)
}

// message is one line the visitor socket sends.
type message struct {
	Action      string `json:"ACTION"`
	Success     *bool  `json:"SUCCESS"`
	Info        string `json:"INFO"`
	VisitorHash string `json:"VISITOR_HASH"`
	ChatType    string `json:"CHAT_TYPE"`
	Access      *bool  `json:"ACCESS"`
	Event       string `json:"EVENT"`
	NewValue    string `json:"NEW_VALUE"`
	Reason      string `json:"REASON"`
}

// seat is a guest's place in a room, held on the visitor socket.
type seat struct {
	conn        *websocket.Conn
	logger      *slog.Logger
	visitorHash string
	ctx         context.Context
	cancel      context.CancelFunc
	writeMu     sync.Mutex
	lines       chan message
	readErr     error
	done        chan struct{}
}

// takeSeat joins the room as a guest, the way the site's chat does.
func takeSeat(ctx context.Context, r Room, logger *slog.Logger) (*seat, error) {
	dctx, dcancel := context.WithTimeout(ctx, joinTimeout)
	defer dcancel()
	conn, _, err := websocket.Dial(dctx, socketURL, &websocket.DialOptions{
		HTTPClient: &http.Client{},
		HTTPHeader: http.Header{"User-Agent": {userAgent}, "Origin": {siteOrigin}},
	})
	if err != nil {
		return nil, fmt.Errorf("dial xcams visitor socket: %w", err)
	}
	conn.SetReadLimit(8 << 20)
	sctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	s := &seat{conn: conn, logger: logger, ctx: sctx, cancel: cancel, lines: make(chan message, 256), done: make(chan struct{})}
	go s.readLoop()
	if err := s.join(dctx, r); err != nil {
		s.close()
		return nil, err
	}
	go s.keepAlive()
	return s, nil
}

func (s *seat) join(ctx context.Context, r Room) error {
	if err := s.send(ctx, map[string]any{"ACTION": "TEST_CONNECTION"}); err != nil {
		return err
	}
	if _, err := s.await(ctx, "TEST_CONNECTION"); err != nil {
		return err
	}
	if err := s.send(ctx, map[string]any{
		"ACTION": "INIT_PAGE", "COMFROM": "", "CF0": "", "CF1": "", "CF2": "", "VISITOR_HASH": "",
		"LANGUAGE": "EN", "PP_PARAMETERS": "", "USER_AGENT": userAgent, "HTTP_REFERER": "",
	}); err != nil {
		return err
	}
	init, err := s.await(ctx, "INIT_PAGE")
	if err != nil {
		return err
	}
	if init.Success == nil || !*init.Success {
		return fmt.Errorf("xcams visitor socket refused the guest: %s", init.Info)
	}
	if err := s.send(ctx, map[string]any{
		"ACTION": "ENTER_VISITOR", "CHAT_MODE": "FREE", "MOBILE_STREAM": "Y", "CAMMER": r.Account,
		"STREAM_TYPE": "WebRTCStream", "MOBILE_APP": "N", "IS_NICKNAME_PROMOTION": "N",
		"TRANSLATION_STATE": false, "TRANSLATION_LANG": nil,
	}); err != nil {
		return err
	}
	enter, err := s.await(ctx, "ENTER_VISITOR")
	if err != nil {
		return err
	}
	if enter.Success == nil || !*enter.Success {
		return fmt.Errorf("%w: %s", errNotFreeChat, enter.Info)
	}
	if enter.ChatType != "" && enter.ChatType != "FREE" {
		return fmt.Errorf("%w: chat type %s", errNotFreeChat, enter.ChatType)
	}
	s.visitorHash = enter.VisitorHash
	if s.visitorHash == "" {
		s.visitorHash = init.VisitorHash
	}
	if s.visitorHash == "" {
		return errors.New("xcams visitor socket gave no visitor hash")
	}
	return nil
}

// await returns the next line with the given action.
func (s *seat) await(ctx context.Context, action string) (message, error) {
	for {
		select {
		case <-ctx.Done():
			return message{}, fmt.Errorf("xcams visitor socket: no %s reply: %w", action, ctx.Err())
		case <-s.done:
			return message{}, fmt.Errorf("xcams visitor socket closed: %w", s.readErr)
		case m := <-s.lines:
			if m.Action == action {
				return m, nil
			}
		}
	}
}

// send emits a visitorApp event.
func (s *seat) send(ctx context.Context, v map[string]any) error {
	b, err := json.Marshal([]any{"visitorApp", v})
	if err != nil {
		return err
	}
	return s.write(ctx, "42"+string(b))
}

func (s *seat) write(ctx context.Context, frame string) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return s.conn.Write(wctx, websocket.MessageText, []byte(frame))
}

func (s *seat) readLoop() {
	defer close(s.done)
	for {
		_, data, err := s.conn.Read(context.Background())
		if err != nil {
			s.readErr = err
			return
		}
		for _, m := range parseFrame(data) {
			select {
			case s.lines <- m:
			default:
				// Lines the seat does not wait for may be dropped.
			}
		}
	}
}

// parseFrame decodes the visitor lines a socket.io frame carries: a
// visitorApp event whose payload is newline-separated JSON.
func parseFrame(data []byte) []message {
	if len(data) < 2 || string(data[:2]) != "42" {
		return nil
	}
	var ev []json.RawMessage
	if json.Unmarshal(data[2:], &ev) != nil || len(ev) < 2 {
		return nil
	}
	var name, payload string
	if json.Unmarshal(ev[0], &name) != nil || name != "visitorApp" || json.Unmarshal(ev[1], &payload) != nil {
		return nil
	}
	var out []message
	for _, line := range strings.Split(payload, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var m message
		if json.Unmarshal([]byte(line), &m) == nil {
			out = append(out, m)
		}
	}
	return out
}

// keepAlive pings the socket as the site's chat does, until the seat closes.
func (s *seat) keepAlive() {
	ping := time.NewTicker(pingEvery)
	defer ping.Stop()
	tping := time.NewTicker(transportPing)
	defer tping.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ping.C:
			_ = s.send(s.ctx, map[string]any{"ACTION": "PING"})
		case <-tping.C:
			_ = s.write(s.ctx, "2")
		}
	}
}

func (s *seat) close() {
	s.cancel()
	_ = s.conn.Close(websocket.StatusNormalClosure, "")
}

// playToken is the Janus server's path token: the account to play and the
// visitor it plays to.
type playToken struct {
	Account     string `json:"account"`
	Action      string `json:"action"`
	VisitorHash string `json:"visitorHash"`
	Codec       string `json:"codec"`
}

func (s *seat) grant(r Room) janus.Grant {
	tok, _ := json.Marshal(playToken{Account: r.Account, Action: "play", VisitorHash: s.visitorHash, Codec: "h264"})
	referer := siteOrigin + "/"
	if r.Nick != "" {
		referer = siteOrigin + "/chat/" + url.PathEscape(r.Nick) + "/"
	}
	return janus.Grant{
		Server:  janusEndpoint + base64.StdEncoding.EncodeToString(tok),
		Mount:   r.Account,
		Codec:   "h264",
		Origin:  siteOrigin,
		Referer: referer,
	}
}

// ended reports why a line ends the free chat the seat watches, or "" when
// it does not.
func ended(m message) string {
	switch m.Action {
	case "PING":
		if m.Access != nil && !*m.Access {
			return "access withdrawn: " + m.Info
		}
	case "CAMMER_UPDATE":
		if m.Event == "UPDATE_CHAT_TYPE" && m.NewValue != "" && m.NewValue != "FREE" {
			return "chat type " + m.NewValue
		}
	case "CLOSE_REASON":
		if m.Reason != "" && m.Reason != "CAMMER_BLUR_DISAPPEARED" {
			return "room closed: " + m.Reason
		}
	case "CHAT_CONTROL_chatroom_remove":
		return "chatroom removed"
	}
	return ""
}

// watched is a playback that ends, with io.EOF, once the model leaves the
// free chat.
type watched struct {
	io.ReadCloser
	seat  *seat
	ended atomic.Bool
}

func (s *seat) watch(stream io.ReadCloser) *watched {
	w := &watched{ReadCloser: stream, seat: s}
	go func() {
		for {
			select {
			case <-s.ctx.Done():
				return
			case <-s.done:
				if s.ctx.Err() == nil {
					s.logger.Info("Visitor socket closed, ending the playback", "error", s.readErr)
					_ = stream.Close()
				}
				return
			case m := <-s.lines:
				if why := ended(m); why != "" {
					s.logger.Info("Free chat ended, ending the recording", "reason", why)
					w.ended.Store(true)
					_ = stream.Close()
					return
				}
			}
		}
	}()
	return w
}

func (w *watched) Read(p []byte) (int, error) {
	n, err := w.ReadCloser.Read(p)
	if err != nil && w.ended.Load() {
		return n, io.EOF
	}
	return n, err
}

func (w *watched) Close() error {
	err := w.ReadCloser.Close()
	w.seat.close()
	return err
}
