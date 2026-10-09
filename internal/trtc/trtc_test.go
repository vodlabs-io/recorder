package trtc

import (
	"context"
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

func TestRoomURLRoundTrip(t *testing.T) {
	r := Room{SDKAppID: "20021627", Name: "1289492513076154369", UserID: "42", UserSig: "eJw+/=", Source: "273178158"}
	got, err := Parse(r.URL())
	if err != nil {
		t.Fatal(err)
	}
	if got != r {
		t.Fatalf("round trip: got %+v, want %+v", got, r)
	}
	if !IsURL(r.URL()) || IsURL("agora://app/chan") {
		t.Fatal("IsURL misclassifies")
	}
	for _, bad := range []string{
		"trtc:///room?userid=1&usersig=s",
		"trtc://app/?userid=1&usersig=s",
		"trtc://app/room?usersig=s",
		"trtc://app/room?userid=1",
		"https://app/room?userid=1&usersig=s",
	} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("Parse(%q) accepted", bad)
		}
	}
}

func TestPickPublisher(t *testing.T) {
	list := json.RawMessage(`{"userList":[
		{"srcTinyId":"","userId":"9","role":20},
		{"srcTinyId":"111","userId":"7","role":20},
		{"srcTinyId":"222","userId":"273178158","role":20}]}`)

	s := &session{room: Room{Source: "273178158"}}
	if u, ok := s.pickPublisher(list); !ok || u.SrcTinyID != "222" {
		t.Errorf("named source: got %+v %v", u, ok)
	}
	s = &session{}
	if u, ok := s.pickPublisher(list); !ok || u.SrcTinyID != "111" {
		t.Errorf("first anchor: got %+v %v", u, ok)
	}
	// A named source not (yet) in the room is waited for, not substituted.
	s = &session{room: Room{Source: "5"}}
	if u, ok := s.pickPublisher(list); ok {
		t.Errorf("picked %+v for an absent source", u)
	}

	s = &session{publisher: "222"}
	if !s.publisherPresent(list) {
		t.Error("publisher reported gone while listed")
	}
	if s.publisherPresent(json.RawMessage(`{"userList":[]}`)) {
		t.Error("publisher reported present in an empty room")
	}
}

// fakeServer plays the signalling server: it answers join and the member
// list, and records every command it was sent.
type fakeServer struct {
	joinCode int
	members  string
	got      chan map[string]any
}

func (f *fakeServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Get("sdkAppId") != "1400" || q.Get("userId") != "42" || q.Get("userSig") != "sig" {
		http.Error(w, "bad credentials", http.StatusForbidden)
		return
	}
	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer c.CloseNow()
	ctx := r.Context()
	write := func(v any) {
		b, _ := json.Marshal(v)
		_ = c.Write(ctx, websocket.MessageText, b)
	}
	write(map[string]any{"seq": 0, "cmd": 19, "code": 0, "tinyId": "777", "data": map[string]any{}})
	for {
		_, data, err := c.Read(ctx)
		if err != nil {
			return
		}
		var m map[string]any
		if json.Unmarshal(data, &m) != nil {
			continue
		}
		f.got <- m
		seq := m["seq"]
		switch m["cmd"] {
		case cmdJoin:
			write(map[string]any{"seq": seq, "cmd": replyJoined, "code": f.joinCode, "message": "refused",
				"data": map[string]any{"iceServer": map[string]any{"urls": []string{}}}})
		case cmdGetUserList:
			write(map[string]any{"seq": seq, "cmd": pushUserList, "data": json.RawMessage(f.members)})
			write(map[string]any{"seq": seq, "cmd": replyUserList, "code": 0, "data": json.RawMessage(f.members)})
		}
	}
}

func withFakeServer(t *testing.T, f *fakeServer) {
	t.Helper()
	srv := httptest.NewTLSServer(f)
	t.Cleanup(srv.Close)
	old := signalDomains
	signalDomains = []string{strings.TrimPrefix(srv.URL, "https://")}
	t.Cleanup(func() { signalDomains = old })
	oldClient := http.DefaultClient
	http.DefaultClient = srv.Client()
	t.Cleanup(func() { http.DefaultClient = oldClient })
}

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestOpenRefusedJoin(t *testing.T) {
	f := &fakeServer{joinCode: -100013, members: `{"userList":[]}`, got: make(chan map[string]any, 16)}
	withFakeServer(t, f)
	room := Room{SDKAppID: "1400", Name: "room1", UserID: "42", UserSig: "sig"}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := Open(ctx, room.URL(), http.DefaultClient, discard())
	var se *serverError
	if !errors.As(err, &se) || se.Code != -100013 {
		t.Fatalf("got %v, want the server's refusal", err)
	}
	join := <-f.got
	data := join["data"].(map[string]any)
	if join["cmd"] != cmdJoin || data["roomId"] != "room1" || data["useStringRoomId"] != true ||
		data["trtcRole"] != float64(roleAudience) || data["userSig"] != "sig" {
		t.Errorf("join = %v", join)
	}
}

func TestOpenEmptyRoomEnds(t *testing.T) {
	f := &fakeServer{members: `{"userList":[]}`, got: make(chan map[string]any, 64)}
	withFakeServer(t, f)
	old := publisherWaitFor
	publisherWaitFor = 50 * time.Millisecond
	t.Cleanup(func() { publisherWaitFor = old })
	room := Room{SDKAppID: "1400", Name: "room1", UserID: "42", UserSig: "sig"}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := Open(ctx, room.URL(), http.DefaultClient, discard()); !errors.Is(err, ErrRoomEnded) {
		t.Fatalf("got %v, want ErrRoomEnded", err)
	}
	<-f.got // join
	list := <-f.got
	if list["cmd"] != cmdGetUserList || list["tinyId"] != "777" {
		t.Errorf("member list request = %v; it must carry the tinyId the server assigned", list)
	}
}
