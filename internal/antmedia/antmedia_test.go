package antmedia

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestGrantDialURL(t *testing.T) {
	g := Grant{WebSocketURL: "wss://sf2.example.com:5443/WebRTCAppEE/websocket", StreamID: "abc",
		SubscriberID: "up:Guest_1:2:3", SubscriberCode: "123456"}
	u, err := g.dialURL()
	if err != nil {
		t.Fatal(err)
	}
	p, _ := url.Parse(u)
	q := p.Query()
	if p.Host != "sf2.example.com:5443" || q.Get("token") != "null" || q.Get("subscriberId") != "up:Guest_1:2:3" || q.Get("subscriberCode") != "123456" {
		t.Errorf("dial URL %q", u)
	}
	g.SubscriberID = ""
	if u, _ := g.dialURL(); strings.Contains(u, "?") {
		t.Errorf("dial URL without a subscriber %q carries a query", u)
	}
	if _, err := (Grant{WebSocketURL: "https://x/websocket"}).dialURL(); err == nil {
		t.Error("an https signalling URL was accepted")
	}
}

func TestTallest(t *testing.T) {
	if h := tallest([]streamInfo{{StreamHeight: 240}, {StreamHeight: 720}, {StreamHeight: 480}}); h != 720 {
		t.Errorf("tallest = %d", h)
	}
	if h := tallest(nil); h != 0 {
		t.Errorf("tallest(nil) = %d", h)
	}
}

// fakeServer answers a play request with reply, after checking the play
// request carries the grant's credentials.
func fakeServer(t *testing.T, reply map[string]any) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		ctx := r.Context()
		for {
			_, b, err := c.Read(ctx)
			if err != nil {
				return
			}
			var m map[string]any
			_ = json.Unmarshal(b, &m)
			switch m["command"] {
			case "getStreamInfo":
				out, _ := json.Marshal(map[string]any{"command": "streamInformation", "streamId": m["streamId"],
					"streamInfo": []map[string]any{{"streamWidth": 640, "streamHeight": 480, "videoCodec": "H264"}}})
				_ = c.Write(ctx, websocket.MessageText, out)
			case "play":
				if m["token"] != "tok" || m["subscriberId"] != "sub" || m["subscriberCode"] != "123456" {
					t.Errorf("play request %v", m)
				}
				out, _ := json.Marshal(reply)
				_ = c.Write(ctx, websocket.MessageText, out)
			}
		}
	}))
	t.Cleanup(srv.Close)
	return "ws" + strings.TrimPrefix(srv.URL, "http") + "/WebRTCAppEE/websocket"
}

func TestPlayRefused(t *testing.T) {
	for _, tc := range []struct {
		reply map[string]any
		want  error
	}{
		{map[string]any{"command": "error", "definition": "unauthorized_access"}, ErrUnauthorized},
		{map[string]any{"command": "error", "definition": "no_stream_exist"}, ErrNoStream},
		{map[string]any{"command": "notification", "definition": "play_finished"}, ErrFinished},
	} {
		ws := fakeServer(t, tc.reply)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_, err := Play(ctx, Grant{WebSocketURL: ws, StreamID: "abc", Token: "tok", SubscriberID: "sub", SubscriberCode: "123456"}, quietLogger())
		cancel()
		if !errors.Is(err, tc.want) {
			t.Errorf("%v: Play = %v, want %v", tc.reply, err, tc.want)
		}
	}
}

func TestPlayNoOfferTimesOut(t *testing.T) {
	old := startTimeout
	startTimeout = 300 * time.Millisecond
	defer func() { startTimeout = old }()
	ws := fakeServer(t, map[string]any{"command": "notification", "definition": "bitrateMeasurement"})
	_, err := Play(context.Background(), Grant{WebSocketURL: ws, StreamID: "abc", Token: "tok", SubscriberID: "sub", SubscriberCode: "123456"}, quietLogger())
	if err == nil || !strings.Contains(err.Error(), "start timeout") {
		t.Errorf("Play = %v, want a start timeout", err)
	}
}
