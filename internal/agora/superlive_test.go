package agora

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// superliveAPI fakes the Superlive endpoints the mint and watch call. reply
// answers livestream/retrieve, given how many times it has been asked.
func superliveAPI(t *testing.T, liveRoom string, reply func(n int64) string) *httptest.Server {
	t.Helper()
	var retrieves atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/initialize":
			io.WriteString(w, `{"guid":"dev1"}`)
		case "/users/profile":
			if liveRoom == "" {
				io.WriteString(w, `{"user":{"livestream_id":null}}`)
			} else {
				io.WriteString(w, `{"user":{"livestream_id":"`+liveRoom+`"}}`)
			}
		case "/livestream/retrieve":
			var body struct {
				ID int64 `json:"livestream_id"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if liveRoom != "" && body.ID != 152 {
				io.WriteString(w, `{"disallowed_reason":null,"stream_details":{"channel_id":"new","agora_channel_token":"tok2","agora_id":8}}`)
				return
			}
			io.WriteString(w, reply(retrieves.Add(1)))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

const (
	retrieveLive     = `{"disallowed_reason":null,"stream_details":{"channel_id":"chan","agora_channel_token":"tok","agora_id":7,"finished_at":null}}`
	retrieveFinished = `{"disallowed_reason":"finished","stream_details":{"channel_id":"","agora_channel_token":"","agora_id":0}}`
	retrievePrivate  = `{"disallowed_reason":"private_not_accepted","stream_details":{"channel_id":"","agora_channel_token":"","agora_id":0}}`
	// A broadcast that ended can still hand out its token.
	retrieveEndedWithToken = `{"disallowed_reason":null,"stream_details":{"channel_id":"chan","agora_channel_token":"tok","agora_id":7,"finished_at":1794000000}}`
)

func TestSuperliveMintRefusesAnEndedOrPrivateBroadcast(t *testing.T) {
	for name, reply := range map[string]string{
		"finished": retrieveFinished, "private": retrievePrivate, "ended with token": retrieveEndedWithToken,
	} {
		t.Run(name, func(t *testing.T) {
			srv := superliveAPI(t, "", func(int64) string { return reply })
			m := &superliveMinter{base: srv.URL + "/"}
			_, err := m.mint(context.Background(), srv.Client(), Channel{Mint: "superlive", Room: "152", User: "99"})
			var refused *RefusedError
			if !errors.As(err, &refused) {
				t.Fatalf("mint: %v, want a RefusedError", err)
			}
		})
	}
}

func TestSuperliveMintFollowsARestartedBroadcast(t *testing.T) {
	srv := superliveAPI(t, "153", func(int64) string { return retrieveFinished })
	m := &superliveMinter{base: srv.URL + "/"}
	ch, err := m.mint(context.Background(), srv.Client(), Channel{Mint: "superlive", Room: "152", User: "99"})
	if err != nil {
		t.Fatal(err)
	}
	if ch.Room != "153" || ch.Name != "new" || ch.Token != "tok2" || ch.UID != 8 {
		t.Fatalf("minted %+v", ch)
	}
}

// TestOpenDoesNotJoinARefusedBroadcast pins that a broadcast the platform
// refused is not joined with the shared token, which reaches only what
// was refused.
func TestOpenDoesNotJoinARefusedBroadcast(t *testing.T) {
	srv := superliveAPI(t, "", func(int64) string { return retrieveFinished })
	defer func(m *superliveMinter) { superlive = m }(superlive)
	superlive = &superliveMinter{base: srv.URL + "/"}

	u := Channel{AppID: "app", Name: "chan", Token: "shared", UID: 5, Mint: "superlive", Room: "152", User: "99"}.URL()
	_, err := Open(context.Background(), u, srv.Client(), slog.New(slog.DiscardHandler))
	var refused *RefusedError
	if !errors.As(err, &refused) || refused.Reason != "finished" {
		t.Fatalf("Open: %v, want the refusal", err)
	}
}

func TestSuperliveWatchEndsWithTheBroadcast(t *testing.T) {
	for name, tc := range map[string]struct {
		later string
		want  func(error) bool
	}{
		"finished": {retrieveFinished, func(err error) bool { return errors.Is(err, ErrChannelEnded) }},
		"ended":    {retrieveEndedWithToken, func(err error) bool { return errors.Is(err, ErrChannelEnded) }},
		"private": {retrievePrivate, func(err error) bool {
			var refused *RefusedError
			return errors.As(err, &refused) && refused.Reason == "private_not_accepted"
		}},
	} {
		t.Run(name, func(t *testing.T) {
			srv := superliveAPI(t, "", func(n int64) string {
				if n < 3 {
					return retrieveLive
				}
				return tc.later
			})
			m := &superliveMinter{base: srv.URL + "/"}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			err := m.watch(ctx, srv.Client(), Channel{Mint: "superlive", Room: "152"}, 10*time.Millisecond)
			if !tc.want(err) {
				t.Fatalf("watch: %v", err)
			}
		})
	}
}

func TestSuperliveWatchOutlastsAFailedRead(t *testing.T) {
	srv := superliveAPI(t, "", func(n int64) string {
		if n == 1 {
			return `not json`
		}
		return retrieveLive
	})
	m := &superliveMinter{base: srv.URL + "/"}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := m.watch(ctx, srv.Client(), Channel{Mint: "superlive", Room: "152"}, 10*time.Millisecond); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("watch of a live broadcast returned %v", err)
	}
}

func TestWatcherOnlyForSuperlive(t *testing.T) {
	if watcher(http.DefaultClient, Channel{Mint: "manyvids", Room: "1"}) != nil {
		t.Error("watching a ManyVids room")
	}
	if watcher(http.DefaultClient, Channel{Mint: "superlive"}) != nil {
		t.Error("watching a Superlive channel with no room")
	}
	if watcher(http.DefaultClient, Channel{Mint: "superlive", Room: "1"}) == nil {
		t.Error("not watching a Superlive room")
	}
}

func TestVideoInTheOtherCodecEndsTheSession(t *testing.T) {
	notify := func(s *session, typ, msg string) error {
		return s.onNotification(notification{Type: typ, Message: json.RawMessage(msg)})
	}
	for _, tc := range []struct {
		joined, want string
	}{{"", CodecVP8}, {CodecVP8, ""}} {
		s := &session{ch: Channel{Codec: tc.joined}, logger: slog.New(slog.DiscardHandler)}
		if err := notify(s, "on_add_audio_stream", `{"uid":1,"pt":111,"audio":true}`); err != nil {
			t.Fatal(err)
		}
		err := notify(s, "on_add_video_stream", `{"uid":1,"pt":0,"video":true}`)
		var codecErr *CodecError
		if !errors.As(err, &codecErr) || codecErr.Codec != tc.want {
			t.Errorf("joined %q: %v, want a CodecError for %q", tc.joined, err, tc.want)
		}
	}

	s := &session{logger: slog.New(slog.DiscardHandler)}
	if err := notify(s, "on_add_video_stream", `{"uid":1,"pt":102,"video":true}`); err != nil {
		t.Errorf("video in the joined codec: %v", err)
	}
	// Another publisher's video is not the broadcast being recorded.
	if err := notify(s, "on_add_video_stream", `{"uid":2,"pt":0,"video":true}`); err != nil {
		t.Errorf("a second publisher's video: %v", err)
	}
}

func TestWithCodec(t *testing.T) {
	u := Channel{AppID: "app", Name: "chan", Token: "t", Mint: "superlive", Room: "152"}.URL() + "&extra=1"
	vp8, err := WithCodec(u, CodecVP8)
	if err != nil {
		t.Fatal(err)
	}
	ch, err := Parse(vp8)
	if err != nil || ch.Codec != CodecVP8 || ch.Room != "152" || !strings.Contains(vp8, "extra=1") {
		t.Fatalf("WithCodec(vp8) = %q (%+v, %v)", vp8, ch, err)
	}
	h264, err := WithCodec(vp8, "")
	if err != nil || strings.Contains(h264, "codec=") {
		t.Fatalf("WithCodec(\"\") = %q, %v", h264, err)
	}
	if _, err := WithCodec("https://example.com/live.flv", CodecVP8); err == nil {
		t.Error("rewrote a URL that is not an Agora channel")
	}
}
