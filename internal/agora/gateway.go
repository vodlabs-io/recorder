package agora

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// userAgent is the browser the client presents itself as: the gateway serves
// the Web SDK, and the version below is the SDK release it speaks for.
const (
	userAgent  = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140.0.0.0 Safari/537.36"
	sdkVersion = "4.24.5"
)

// pingInterval is how often the signalling socket is pinged; the gateway
// drops a client it has not heard from in a while.
const pingInterval = 3 * time.Second

// ErrChannelEnded reports that the broadcaster left the channel: the
// broadcast is over, or at least this stream of it.
var ErrChannelEnded = errors.New("agora: broadcaster left the channel")

// gatewayError is a refusal the gateway sent in reply to a request or as a
// notification, with the gateway's own code.
type gatewayError struct {
	Kind string
	Code int
}

func (e *gatewayError) Error() string {
	if name, ok := gatewayCodes[e.Code]; ok {
		return fmt.Sprintf("agora gateway: %s (%s, code %d)", e.Kind, name, e.Code)
	}
	return fmt.Sprintf("agora gateway: %s (code %d)", e.Kind, e.Code)
}

// gatewayCodes names the refusal codes worth recognising in a log line.
var gatewayCodes = map[int]string{
	2:  "token expired",
	3:  "invalid channel permission",
	4:  "invalid certificate",
	6:  "channel not found",
	7:  "invalid ticket",
	14: "uid banned",
	15: "ip banned",
	16: "channel banned",
	17: "kicked",
}

// signal is the signalling WebSocket to one gateway.
type signal struct {
	conn   *websocket.Conn
	logger *slog.Logger

	mu      sync.Mutex
	pending map[string]chan json.RawMessage

	// events receives every notification (a message with a _type and no
	// matching request). Buffered; a slow reader drops nothing because the
	// reader blocks instead.
	events chan notification
	done   chan struct{}
	err    error
}

type notification struct {
	Type    string          `json:"_type"`
	Message json.RawMessage `json:"_message"`
}

func dialGateway(ctx context.Context, client *http.Client, e edge, logger *slog.Logger) (*signal, error) {
	host := strings.ReplaceAll(e.IP, ".", "-")
	// The SDK races its own domain against the SD-RTN mirror; trying them in
	// turn reaches the same gateway.
	var errs []error
	for _, domain := range []string{"edge.agora.io", "edge.sd-rtn.com"} {
		u := fmt.Sprintf("wss://%s.%s:%d", host, domain, e.Port)
		dctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		conn, _, err := websocket.Dial(dctx, u, &websocket.DialOptions{
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
			pending: map[string]chan json.RawMessage{},
			events:  make(chan notification, 64),
			done:    make(chan struct{}),
		}
		go s.readLoop()
		return s, nil
	}
	return nil, fmt.Errorf("dial gateway %s:%d: %w", e.IP, e.Port, errors.Join(errs...))
}

func (s *signal) readLoop() {
	defer close(s.done)
	ctx := context.Background()
	for {
		_, data, err := s.conn.Read(ctx)
		if err != nil {
			s.err = err
			return
		}
		var head struct {
			ID     string `json:"_id"`
			Type   string `json:"_type"`
			Result string `json:"_result"`
		}
		if err := json.Unmarshal(data, &head); err != nil {
			s.logger.Debug("Unreadable gateway message", "error", err)
			continue
		}
		if head.Result != "" {
			s.mu.Lock()
			ch := s.pending[head.ID]
			delete(s.pending, head.ID)
			s.mu.Unlock()
			if ch != nil {
				ch <- data
			}
			continue
		}
		if head.Type == "" {
			continue
		}
		var n notification
		if err := json.Unmarshal(data, &n); err != nil {
			continue
		}
		select {
		case s.events <- n:
		case <-time.After(5 * time.Second):
			s.logger.Debug("Gateway notification dropped", "type", n.Type)
		}
	}
}

// request sends a message and waits for its reply, returning the reply's
// _message on success.
func (s *signal) request(ctx context.Context, typ string, msg any) (json.RawMessage, error) {
	id := randomHex(3)
	ch := make(chan json.RawMessage, 1)
	s.mu.Lock()
	s.pending[id] = ch
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.pending, id)
		s.mu.Unlock()
	}()
	if err := s.send(ctx, map[string]any{"_id": id, "_type": typ, "_message": msg}); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	select {
	case raw := <-ch:
		var reply struct {
			Result  string          `json:"_result"`
			Message json.RawMessage `json:"_message"`
		}
		if err := json.Unmarshal(raw, &reply); err != nil {
			return nil, err
		}
		if reply.Result != "success" {
			var e struct {
				ErrorCode int `json:"error_code"`
				Code      int `json:"code"`
			}
			_ = json.Unmarshal(reply.Message, &e)
			return nil, &gatewayError{Kind: typ + " refused", Code: max(e.ErrorCode, e.Code)}
		}
		return reply.Message, nil
	case <-s.done:
		return nil, fmt.Errorf("gateway closed during %s: %w", typ, s.err)
	case <-ctx.Done():
		return nil, fmt.Errorf("%s: %w", typ, ctx.Err())
	}
}

func (s *signal) send(ctx context.Context, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return s.conn.Write(wctx, websocket.MessageText, b)
}

// keepalive pings the gateway until ctx ends.
func (s *signal) keepalive(ctx context.Context) {
	t := time.NewTicker(pingInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.done:
			return
		case <-t.C:
			start := time.Now()
			if _, err := s.request(ctx, "ping", nil); err != nil {
				if ctx.Err() == nil {
					s.logger.Debug("Gateway ping failed", "error", err)
				}
				continue
			}
			_ = s.send(ctx, map[string]any{"_type": "ping_back",
				"_message": map[string]any{"pingpongElapse": time.Since(start).Milliseconds()}})
		}
	}
}

// close says goodbye and closes the socket.
func (s *signal) close() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	_ = s.send(ctx, map[string]any{"_id": randomHex(3), "_type": "leave"})
	cancel()
	_ = s.conn.Close(websocket.StatusNormalClosure, "")
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
