package trtc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// The browser the client presents itself as, and the Web SDK release it
// speaks for.
const (
	userAgent  = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140.0.0.0 Safari/537.36"
	sdkVersion = "5.15.1.1701"
)

// signalDomains are the overseas signalling servers the SDK's scheduler
// hands out, main first.
var signalDomains = []string{"signaling.rtc-web.com", "signaling.rtc-web.io"}

// heartbeatInterval is how often the SDK reports quality, which is what tells
// the server the client is still there.
const heartbeatInterval = 2 * time.Second

// Signalling commands the client sends, and the server's numbered replies and
// pushes it acts on.
const (
	cmdJoin         = "join"
	cmdGetUserList  = "get_user_list"
	cmdSubscribe    = "subscribe"
	cmdLeave        = "leave"
	cmdHeartbeat    = "quality_report"
	replyJoined     = 20
	replyUserList   = 4137
	replySubscribed = 4102
	pushUserList    = 23
)

// message is one signalling frame. Commands go up named, replies and pushes
// come down numbered.
type message struct {
	Cmd     json.RawMessage `json:"cmd"`
	Seq     int             `json:"seq"`
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
	TinyID  json.Number     `json:"tinyId"`
}

func (m message) cmd() int {
	n, _ := strconv.Atoi(string(m.Cmd))
	return n
}

// serverError is a refusal the server sent, with its own code.
type serverError struct {
	Op      string
	Code    int
	Message string
}

func (e *serverError) Error() string {
	return fmt.Sprintf("trtc %s refused: %s (code %d)", e.Op, e.Message, e.Code)
}

// signal is the signalling WebSocket.
type signal struct {
	conn   *websocket.Conn
	logger *slog.Logger
	userID string

	mu      sync.Mutex
	seq     int
	tinyID  string
	pending map[int]chan message // keyed by reply cmd*1e6 + seq

	pushes chan message
	done   chan struct{}
	err    error
}

func dialSignal(ctx context.Context, client *http.Client, r Room, logger *slog.Logger) (*signal, error) {
	q := url.Values{}
	q.Set("sdkAppId", r.SDKAppID)
	q.Set("userId", r.UserID)
	q.Set("userSig", r.UserSig)
	q.Set("keepAlive", "1")
	var errs []error
	for _, domain := range signalDomains {
		dctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		conn, _, err := websocket.Dial(dctx, "wss://"+domain+"?"+q.Encode(), &websocket.DialOptions{
			HTTPClient: client,
			HTTPHeader: http.Header{"User-Agent": {userAgent}},
		})
		cancel()
		if err != nil {
			errs = append(errs, err)
			continue
		}
		conn.SetReadLimit(4 << 20)
		s := &signal{
			conn:    conn,
			logger:  logger,
			userID:  r.UserID,
			pending: map[int]chan message{},
			pushes:  make(chan message, 64),
			done:    make(chan struct{}),
		}
		go s.readLoop()
		return s, nil
	}
	return nil, fmt.Errorf("dial trtc signalling: %w", errors.Join(errs...))
}

func pendingKey(reply, seq int) int { return reply*1_000_000 + seq }

func (s *signal) readLoop() {
	defer close(s.done)
	ctx := context.Background()
	for {
		_, data, err := s.conn.Read(ctx)
		if err != nil {
			s.err = err
			return
		}
		var m message
		if json.Unmarshal(data, &m) != nil {
			// The answer to a ping is plain text.
			continue
		}
		s.mu.Lock()
		if m.TinyID != "" && s.tinyID == "" {
			s.tinyID = m.TinyID.String()
		}
		ch, ok := s.pending[pendingKey(m.cmd(), m.Seq)]
		if ok {
			delete(s.pending, pendingKey(m.cmd(), m.Seq))
		}
		s.mu.Unlock()
		if ok {
			ch <- m
			continue
		}
		select {
		case s.pushes <- m:
		default:
			// Pushes are advisory; a full queue drops the oldest news.
		}
	}
}

// request sends cmd with data and waits for its numbered reply.
func (s *signal) request(ctx context.Context, cmd string, data any, reply int) (message, error) {
	s.mu.Lock()
	s.seq++
	seq := s.seq
	ch := make(chan message, 1)
	s.pending[pendingKey(reply, seq)] = ch
	s.mu.Unlock()
	if err := s.send(ctx, cmd, data, seq); err != nil {
		return message{}, err
	}
	rctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	select {
	case m := <-ch:
		if m.Code != 0 {
			return m, &serverError{Op: cmd, Code: m.Code, Message: m.Message}
		}
		return m, nil
	case <-s.done:
		return message{}, fmt.Errorf("%s: signalling closed: %w", cmd, s.err)
	case <-rctx.Done():
		s.mu.Lock()
		delete(s.pending, pendingKey(reply, seq))
		s.mu.Unlock()
		return message{}, fmt.Errorf("%s: %w", cmd, rctx.Err())
	}
}

func (s *signal) send(ctx context.Context, cmd string, data any, seq int) error {
	s.mu.Lock()
	env := map[string]any{"cmd": cmd, "userId": s.userID, "seq": seq}
	if data != nil {
		env["data"] = data
	}
	if s.tinyID != "" {
		env["tinyId"] = s.tinyID
	}
	s.mu.Unlock()
	b, err := json.Marshal(env)
	if err != nil {
		return err
	}
	wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return s.conn.Write(wctx, websocket.MessageText, b)
}

// keepalive sends the SDK's heartbeat, a quality report, until ctx ends. Only
// the fields that identify the client are filled in.
func (s *signal) keepalive(ctx context.Context) {
	t := time.NewTicker(heartbeatInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.done:
			return
		case <-t.C:
			s.mu.Lock()
			s.seq++
			seq, tiny := s.seq, s.tinyID
			s.mu.Unlock()
			report := map[string]any{
				"str_sdk_version": sdkVersion,
				"uint64_datetime": time.Now().UnixMilli(),
				"msg_user_info":   map[string]any{"str_identifier": s.userID, "uint64_tinyid": tiny},
				"msg_event_msg":   []any{},
				"msg_device_info": map[string]any{"uint32_terminal_type": 15, "uint32_net_type": 0},
			}
			if err := s.send(ctx, cmdHeartbeat, report, seq); err != nil && ctx.Err() == nil {
				s.logger.Debug("TRTC heartbeat failed", "error", err)
			}
		}
	}
}

// close leaves the room and closes the socket.
func (s *signal) close() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	s.mu.Lock()
	s.seq++
	seq := s.seq
	s.mu.Unlock()
	_ = s.send(ctx, cmdLeave, map[string]any{}, seq)
	cancel()
	_ = s.conn.Close(websocket.StatusNormalClosure, "")
}
