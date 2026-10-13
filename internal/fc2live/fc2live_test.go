package fc2live

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestParse(t *testing.T) {
	for raw, want := range map[string]string{
		"fc2live://58117384":   "58117384",
		"fc2live://2_10750278": "2_10750278",
		"fc2live://58117384/":  "58117384",
	} {
		if got, err := Parse(raw); err != nil || got != want {
			t.Errorf("Parse(%q) = %q, %v", raw, got, err)
		}
	}
	for _, raw := range []string{"fc2live://", "fc2live://abc", "fc2live://3_1", "nicolive://lv1", "https://live.fc2.com/58117384/"} {
		if _, err := Parse(raw); err == nil {
			t.Errorf("Parse(%q) accepted", raw)
		}
	}
}

const hlsReply = `{"name":"_response_","id":%ID%,"arguments":{"status":0,` +
	`"playlists":[{"mode":0,"status":0,"url":"https://us-west-1-media.live.fc2.com/a/stream/1/0/master_playlist?targets=20,30"},` +
	`{"mode":30,"status":0,"url":"https://us-west-1-media.live.fc2.com/a/stream/1/30/playlist"}],` +
	`"playlists_high_latency":[{"mode":1,"status":%STATUS%,"url":"https://us-west-1-media.live.fc2.com/a/stream/1/1/master_playlist?targets=20,30"}],` +
	`"playlists_middle_latency":[]}}`

// controlServer plays a control socket: the greeting, then a playlist
// answer per ask (the first ones still converting), then what after says.
func controlServer(t *testing.T, converting int, publish bool, after string) *websocket.Conn {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		ctx := r.Context()
		pub := "true"
		if !publish {
			pub = "false"
		}
		c.Write(ctx, websocket.MessageText, []byte(`{"name":"initial_connect","arguments":{"publish":`+pub+`}}`))
		c.Write(ctx, websocket.MessageText, []byte(`{"name":"connect_complete","arguments":{}}`))
		for asks := 0; ; asks++ {
			_, data, err := c.Read(ctx)
			if err != nil {
				return
			}
			if !strings.Contains(string(data), "get_hls_information") {
				continue
			}
			id := string(data[strings.LastIndex(string(data), ":")+1 : len(data)-1])
			status := "0"
			if asks < converting {
				status = "1"
			}
			reply := strings.ReplaceAll(strings.ReplaceAll(hlsReply, "%ID%", id), "%STATUS%", status)
			if asks < converting {
				reply = strings.ReplaceAll(reply, `"status":0,"url"`, `"status":1,"url"`)
			}
			c.Write(ctx, websocket.MessageText, []byte(`{"name":"user_count","arguments":{}}`))
			c.Write(ctx, websocket.MessageText, []byte(reply))
			if asks >= converting && after != "" {
				c.Write(ctx, websocket.MessageText, []byte(after))
			}
		}
	}))
	t.Cleanup(srv.Close)
	conn, _, err := websocket.Dial(context.Background(), "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.CloseNow() })
	return conn
}

func TestGrantPicksTheHighLatencyMaster(t *testing.T) {
	s := &Seat{conn: controlServer(t, 0, true, "")}
	got, err := s.grant(context.Background())
	if err != nil || got != "https://us-west-1-media.live.fc2.com/a/stream/1/1/master_playlist?targets=20,30" {
		t.Errorf("grant = %q, %v", got, err)
	}
}

func TestGrantWaitsForConversion(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s := &Seat{conn: controlServer(t, 1, true, "")}
	start := time.Now()
	got, err := s.grant(ctx)
	if err != nil || !strings.Contains(got, "/1/master_playlist") || time.Since(start) < grantRetry {
		t.Errorf("grant = %q, %v after %v", got, err, time.Since(start))
	}
}

func TestGrantOnAnEndedBroadcast(t *testing.T) {
	s := &Seat{conn: controlServer(t, 0, false, "")}
	if _, err := s.grant(context.Background()); !errors.Is(err, ErrEnded) {
		t.Errorf("grant = %v, want ErrEnded", err)
	}
}

func TestReadMessageEnds(t *testing.T) {
	conn := controlServer(t, 0, true, `{"name":"publish_stop","arguments":{}}`)
	s := &Seat{conn: conn}
	if _, err := s.grant(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := readMessage(context.Background(), conn); !errors.Is(err, ErrEnded) {
		t.Errorf("readMessage after publish_stop = %v", err)
	}
	conn = controlServer(t, 0, true, `{"name":"control_disconnection","arguments":{"code":4507}}`)
	s = &Seat{conn: conn}
	if _, err := s.grant(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := readMessage(context.Background(), conn); err == nil || !strings.Contains(err.Error(), "4507") {
		t.Errorf("readMessage after control_disconnection = %v", err)
	}
}

func TestCheckStreamURL(t *testing.T) {
	if checkStreamURL("https://us-west-1-media.live.fc2.com/a/stream/1/1/master_playlist") != nil {
		t.Error("rejected an fc2.com playlist")
	}
	if checkStreamURL("https://evil.test/a.m3u8") == nil || checkStreamURL("http://live.fc2.com/x") == nil {
		t.Error("accepted a playlist off fc2.com or over http")
	}
}
