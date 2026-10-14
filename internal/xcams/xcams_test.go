package xcams

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestParse(t *testing.T) {
	r := Room{Account: "c239252", Nick: "SaraLuna"}
	if !IsURL(r.URL()) || IsURL("https://www.xcams.com/chat/SaraLuna/") {
		t.Error("IsURL")
	}
	got, err := Parse(r.URL())
	if err != nil || got != r {
		t.Errorf("Parse(%q) = %+v, %v", r.URL(), got, err)
	}
	if got, err := Parse("xcams://c1"); err != nil || got.Account != "c1" || got.Nick != "" {
		t.Errorf("Parse without a nickname = %+v, %v", got, err)
	}
	for _, bad := range []string{"xcams://SaraLuna", "xcams://?nick=x", "xcams://c12x", "agora://c1"} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("Parse(%q) accepted", bad)
		}
	}
}

// frame wraps visitor lines in a socket.io visitorApp event.
func frame(lines ...string) []byte {
	b, _ := json.Marshal([]any{"visitorApp", strings.Join(lines, "\n")})
	return append([]byte("42"), b...)
}

func TestEnded(t *testing.T) {
	for line, want := range map[string]bool{
		`{"ACTION":"CLOSE_REASON","VOYEUR_ALLOWED":"Y","REASON":"CAMMER_TO_GROUP"}`:                    true,
		`{"ACTION":"CLOSE_REASON","REASON":"CAMMER_BLUR_DISAPPEARED"}`:                                  false,
		`{"ACCOUNT":"c239252","NEW_VALUE":"O2M","ACTION":"CAMMER_UPDATE","EVENT":"UPDATE_CHAT_TYPE"}`:  true,
		`{"ACCOUNT":"c239252","NEW_VALUE":"FREE","ACTION":"CAMMER_UPDATE","EVENT":"UPDATE_CHAT_TYPE"}`: false,
		`{"ACTION":"CHAT_CONTROL_chatroom_remove","OBJECT_TYPE":"chatroom"}`:                            true,
		`{"SUCCESS":false,"ACTION":"PING","INFO":"NO_ACCESS","ACCESS":false}`:                           true,
		`{"SUCCESS":true,"ACTION":"PING","ACCESS":true}`:                                                false,
		`{"ACTION":"TEXT","MESSAGE_TYPE":"TEXT","TEXT":"hi"}`:                                           false,
	} {
		lines := parseFrame(frame(line))
		if len(lines) != 1 {
			t.Fatalf("%s: %d lines", line, len(lines))
		}
		if got := ended(lines[0]) != ""; got != want {
			t.Errorf("%s: ended = %v", line, got)
		}
	}
	if parseFrame([]byte("3")) != nil || parseFrame([]byte(`42["other","{}"]`)) != nil {
		t.Error("a frame that is not a visitorApp event parsed")
	}
}

// stubSocket is a visitor socket that seats a guest with enter as the
// ENTER_VISITOR reply, then sends after.
func stubSocket(t *testing.T, enter string, after ...string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		defer c.CloseNow()
		ctx := r.Context()
		_ = c.Write(ctx, websocket.MessageText, []byte(`0{"sid":"x","pingInterval":25000,"pingTimeout":60000}`))
		_ = c.Write(ctx, websocket.MessageText, []byte("40"))
		for {
			_, data, err := c.Read(ctx)
			if err != nil {
				return
			}
			var ev []json.RawMessage
			if !strings.HasPrefix(string(data), "42") || json.Unmarshal(data[2:], &ev) != nil || len(ev) < 2 {
				continue
			}
			var m map[string]any
			_ = json.Unmarshal(ev[1], &m)
			switch m["ACTION"] {
			case "TEST_CONNECTION":
				_ = c.Write(ctx, websocket.MessageText, frame(`{"ACTION":"TEST_CONNECTION"}`))
			case "INIT_PAGE":
				_ = c.Write(ctx, websocket.MessageText, frame(`{"SUCCESS":true,"ACTION":"INIT_PAGE","VISITOR_HASH":"abc_1"}`))
			case "ENTER_VISITOR":
				if m["CAMMER"] != "c239252" || m["CHAT_MODE"] != "FREE" {
					t.Errorf("enter %v", m)
				}
				_ = c.Write(ctx, websocket.MessageText, frame(`{"ACTION":"ROOM_INFO","NUMBER_OF_VISITORS":3}`, enter))
				for _, l := range after {
					_ = c.Write(ctx, websocket.MessageText, frame(l))
				}
			}
		}
	}))
	t.Cleanup(srv.Close)
	old := socketURL
	socketURL = "ws" + strings.TrimPrefix(srv.URL, "http")
	t.Cleanup(func() { socketURL = old })
}

func TestTakeSeat(t *testing.T) {
	stubSocket(t, `{"SUCCESS":true,"ACTION":"ENTER_VISITOR","CHAT_TYPE":"FREE","VISITOR_HASH":"def_2"}`)
	r := Room{Account: "c239252", Nick: "SaraLuna"}
	s, err := takeSeat(context.Background(), r, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	if s.visitorHash != "def_2" {
		t.Errorf("visitor hash %q", s.visitorHash)
	}
	g := s.grant(r)
	tok, ok := strings.CutPrefix(g.Server, janusEndpoint)
	if !ok {
		t.Fatalf("server %q", g.Server)
	}
	b, err := base64.StdEncoding.DecodeString(tok)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `{"account":"c239252","action":"play","visitorHash":"def_2","codec":"h264"}` {
		t.Errorf("play token %s", b)
	}
	if g.Mount != "c239252" || g.Codec != "h264" || g.Referer != siteOrigin+"/chat/SaraLuna/" {
		t.Errorf("grant %+v", g)
	}
}

func TestTakeSeatInAPaidShow(t *testing.T) {
	stubSocket(t, `{"SUCCESS":false,"ACTION":"ENTER_VISITOR","INFO":"CAMMER_IS_PAID"}`)
	_, err := takeSeat(context.Background(), Room{Account: "c239252"}, quietLogger())
	if !errors.Is(err, errNotFreeChat) {
		t.Errorf("takeSeat = %v, want errNotFreeChat", err)
	}
}

// closer records a Close.
type closer struct {
	io.Reader
	closed chan struct{}
}

func (c *closer) Close() error {
	select {
	case <-c.closed:
	default:
		close(c.closed)
	}
	return nil
}

func TestWatchEndsOnAPaidShow(t *testing.T) {
	stubSocket(t, `{"SUCCESS":true,"ACTION":"ENTER_VISITOR","CHAT_TYPE":"FREE","VISITOR_HASH":"def_2"}`,
		`{"ACTION":"CLOSE_REASON","VOYEUR_ALLOWED":"Y","REASON":"CAMMER_TO_GROUP"}`)
	s, err := takeSeat(context.Background(), Room{Account: "c239252"}, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	pr, pw := io.Pipe()
	stream := &closer{Reader: pr, closed: make(chan struct{})}
	w := s.watch(stream)
	select {
	case <-stream.closed:
	case <-time.After(5 * time.Second):
		t.Fatal("the playback was not stopped")
	}
	_ = pw.CloseWithError(errors.New("closed pipe"))
	if _, err := w.Read(make([]byte, 1)); err != io.EOF {
		t.Errorf("read after a paid show = %v, want EOF", err)
	}
	_ = w.Close()
}
