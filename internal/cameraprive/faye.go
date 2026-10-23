package cameraprive

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/coder/websocket"
)

const (
	// idleTO drops a socket that has gone this long without a message; the
	// server answers each /meta/connect within its 30 s advice.
	idleTO   = 75 * time.Second
	maxFrame = 4 << 20
)

// Room channels, after the instance prefix.
const (
	chNotificate   = "/notificate/"
	chInitialized  = "/chat_initialized/"
	chUserChecked  = "/user/checked/"
	chDisconnected = "/disconnect_by_server/"
	chPong         = "/users/pong/"
	chUserCheck    = "/user/check"
	chConnectUser  = "/connect/user"
	chDisconnect   = "/disconnect"
	chPing         = "/users/ping"
)

// fayeMessage is one Bayeux message.
type fayeMessage struct {
	Channel    string          `json:"channel"`
	ClientID   string          `json:"clientId,omitempty"`
	ID         string          `json:"id,omitempty"`
	Successful *bool           `json:"successful,omitempty"`
	Error      string          `json:"error,omitempty"`
	Data       json.RawMessage `json:"data,omitempty"`
}

// serverVars are the room's settings for the viewer, sent when the viewer is
// checked and again when the chat is initialized.
type serverVars struct {
	StreamingType  string `json:"streaming_type"`
	ContainCredits any    `json:"contain_credits"`
}

// faye is a viewer in a room's chat: a Faye client on the room's chat server,
// signed with the keys the room page handed the viewer. It joins as the
// site's player does: subscribe to the viewer's own channels, ask for a check
// (/user/check), connect as a free-chat viewer once checked, and, for a
// signed-in viewer, keep answering the room's pongs with pings.
type faye struct {
	r        *room
	conn     *websocket.Conn
	clientID string
	nextID   int
	// initialized is set once the room has taken the viewer in.
	initialized bool
	vars        serverVars
	pending     []fayeMessage
}

// dialFaye opens a socket to the room's chat server and shakes hands.
func dialFaye(ctx context.Context, client *http.Client, r *room, userAgent string) (*faye, error) {
	conn, _, err := websocket.Dial(ctx, "wss://"+strings.TrimPrefix(r.ChatURL, "https://"), &websocket.DialOptions{
		HTTPClient: client,
		HTTPHeader: http.Header{"Origin": {siteOrigin}, "User-Agent": {userAgent}},
	})
	if err != nil {
		return nil, fmt.Errorf("dial cameraprive chat server: %w", err)
	}
	conn.SetReadLimit(maxFrame)
	f := &faye{r: r, conn: conn}
	if err := f.send(ctx, map[string]any{
		"channel": "/meta/handshake", "version": "1.0", "supportedConnectionTypes": []string{"websocket"},
	}); err != nil {
		f.close()
		return nil, err
	}
	for f.clientID == "" {
		msgs, err := f.read(ctx)
		if err != nil {
			f.close()
			return nil, fmt.Errorf("cameraprive chat handshake: %w", err)
		}
		for _, m := range msgs {
			if m.Channel != "/meta/handshake" {
				continue
			}
			if m.Successful == nil || !*m.Successful || m.ClientID == "" {
				f.close()
				return nil, fmt.Errorf("cameraprive chat handshake refused: %s", m.Error)
			}
			f.clientID = m.ClientID
		}
	}
	steps := []map[string]any{f.metaConnect()}
	subs := []string{chNotificate, chInitialized, chUserChecked, chDisconnected}
	if r.Member {
		subs = append(subs, chPong)
	}
	for _, ch := range subs {
		steps = append(steps, map[string]any{"channel": "/meta/subscribe", "subscription": f.channel(ch + r.Hashkey)})
	}
	steps = append(steps, map[string]any{"channel": f.channel(chUserCheck)})
	for _, m := range steps {
		if err := f.send(ctx, m); err != nil {
			f.close()
			return nil, err
		}
	}
	return f, nil
}

func (f *faye) channel(suffix string) string { return "/" + f.r.Instance + suffix }

func (f *faye) metaConnect() map[string]any {
	return map[string]any{"channel": "/meta/connect", "connectionType": "websocket"}
}

// send writes one message, signed as the site's client signs it: everything
// but the handshake and /meta/connect carries the viewer's keys, and the
// requests that identify the viewer carry chat_info too.
func (f *faye) send(ctx context.Context, m map[string]any) error {
	f.nextID++
	m["id"] = strconv.Itoa(f.nextID)
	if f.clientID != "" {
		m["clientId"] = f.clientID
	}
	ch, _ := m["channel"].(string)
	if ch != "/meta/handshake" && ch != "/meta/connect" && ch != "/meta/disconnect" {
		ext := map[string]string{"hashkey": f.r.Hashkey, "htsp": f.r.HTSP, "htrp": f.r.HTRP}
		sub, _ := m["subscription"].(string)
		for _, k := range []string{chUserChecked, chDisconnected, chNotificate, chInitialized} {
			if strings.Contains(sub, strings.TrimSuffix(k, "/")) {
				ext["chat_info"] = f.r.ChatInfo
			}
		}
		for _, k := range []string{chUserCheck, chConnectUser, chDisconnect} {
			if strings.Contains(ch, k) {
				ext["chat_info"] = f.r.ChatInfo
			}
		}
		m["ext"] = ext
	}
	b, err := json.Marshal([]any{m})
	if err != nil {
		return err
	}
	wctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	if err := f.conn.Write(wctx, websocket.MessageText, b); err != nil {
		return fmt.Errorf("cameraprive chat write: %w", err)
	}
	return nil
}

func (f *faye) read(ctx context.Context) ([]fayeMessage, error) {
	_, b, err := f.conn.Read(ctx)
	if err != nil {
		return nil, err
	}
	var msgs []fayeMessage
	if err := json.Unmarshal(b, &msgs); err != nil {
		return nil, fmt.Errorf("decode cameraprive chat frame: %w", err)
	}
	return msgs, nil
}

// errQuiet is a socket that went quiet past idleTO.
var errQuiet = errors.New("cameraprive chat socket went quiet")

// next returns the viewer's next room message, keeping the session alive on
// the way. The channel returned is stripped of the instance prefix.
func (f *faye) next(ctx context.Context) (string, json.RawMessage, error) {
	for {
		for len(f.pending) > 0 {
			m := f.pending[0]
			f.pending = f.pending[1:]
			if ch := strings.TrimPrefix(m.Channel, "/"+f.r.Instance); ch != m.Channel {
				return ch, m.Data, nil
			}
		}
		rctx, cancel := context.WithTimeout(ctx, idleTO)
		msgs, err := f.read(rctx)
		cancel()
		if err != nil {
			if ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded) {
				return "", nil, errQuiet
			}
			return "", nil, err
		}
		for _, m := range msgs {
			if err := f.handle(ctx, m); err != nil {
				return "", nil, err
			}
			if !strings.HasPrefix(m.Channel, "/meta/") && len(m.Data) > 0 {
				f.pending = append(f.pending, m)
			}
		}
	}
}

func (f *faye) handle(ctx context.Context, m fayeMessage) error {
	switch {
	case m.Channel == "/meta/connect":
		return f.send(ctx, f.metaConnect())
	case m.Channel == f.channel(chUserChecked+f.r.Hashkey):
		var d struct {
			Vars serverVars `json:"server_vars"`
		}
		_ = json.Unmarshal(m.Data, &d)
		f.vars = d.Vars
		st := d.Vars.StreamingType
		if st == "" {
			st = "rtc"
		}
		return f.send(ctx, map[string]any{
			"channel": f.channel(chConnectUser),
			"data":    map[string]string{"chat_type": "free", "stream_type": st},
		})
	case m.Channel == f.channel(chInitialized+f.r.Hashkey) && !f.initialized:
		f.initialized = true
		if f.r.Member {
			return f.send(ctx, map[string]any{"channel": f.channel(chPing)})
		}
	case m.Channel == f.channel(chPong+f.r.Hashkey):
		return f.send(ctx, map[string]any{
			"channel": f.channel(chPing),
			"data":    map[string]string{"streaming_type": f.vars.StreamingType},
		})
	}
	return nil
}

// leave tells the room the viewer left, as the site's client does, and waits
// for the room to let the seat go: a room admits one guest seat per viewer, so
// the next would be refused while this one is held.
func (f *faye) leave() {
	defer f.close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if f.send(ctx, map[string]any{"channel": f.channel(chDisconnect), "data": map[string]string{"message": "leave"}}) != nil {
		return
	}
	want := strconv.Itoa(f.nextID)
	for acked := false; !acked; {
		msgs, err := f.read(ctx)
		if err != nil {
			return
		}
		for _, m := range msgs {
			acked = acked || (m.ID == want && m.Channel == f.channel(chDisconnect))
		}
	}
	_ = f.send(ctx, map[string]any{"channel": "/meta/disconnect"})
}

func (f *faye) close() { _ = f.conn.Close(websocket.StatusNormalClosure, "") }
