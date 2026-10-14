package janus

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
)

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// fakeJanus is a Janus HTTP transport with one session and one streaming
// handle. watch answers a watch request with the events to queue for the
// session's long poll; start is handed the start request's answer.
type fakeJanus struct {
	t     *testing.T
	watch func(body map[string]any) []map[string]any
	start func(answer string)

	mu        sync.Mutex
	events    []map[string]any
	destroyed bool
	wake      chan struct{}
}

func newFakeJanus(t *testing.T) (*fakeJanus, string) {
	f := &fakeJanus{t: t, wake: make(chan struct{}, 1)}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	return f, srv.URL + "/janus/eyJ0b2tlbiI6MX0="
}

func (f *fakeJanus) queue(events ...map[string]any) {
	f.mu.Lock()
	f.events = append(f.events, events...)
	f.mu.Unlock()
	select {
	case f.wake <- struct{}{}:
	default:
	}
}

func (f *fakeJanus) serve(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/janus/eyJ0b2tlbiI6MX0=")
	reply := func(v any) { _ = json.NewEncoder(w).Encode(v) }
	if r.Method == http.MethodGet {
		if path != "/7" || r.URL.Query().Get("maxev") == "" {
			f.t.Errorf("poll %s", r.URL)
		}
		select {
		case <-f.wake:
		case <-time.After(500 * time.Millisecond):
		case <-r.Context().Done():
			return
		}
		f.mu.Lock()
		evs := f.events
		f.events = nil
		f.mu.Unlock()
		if len(evs) == 0 {
			evs = []map[string]any{{"janus": "keepalive"}}
		}
		reply(evs)
		return
	}
	var m map[string]any
	_ = json.NewDecoder(r.Body).Decode(&m)
	tx := m["transaction"]
	switch {
	case path == "" && m["janus"] == "create":
		reply(map[string]any{"janus": "success", "transaction": tx, "data": map[string]any{"id": 7}})
	case path == "/7" && m["janus"] == "attach":
		if m["plugin"] != "janus.plugin.streaming" {
			f.t.Errorf("attach %v", m)
		}
		reply(map[string]any{"janus": "success", "transaction": tx, "data": map[string]any{"id": 9}})
	case path == "/7" && m["janus"] == "keepalive":
		reply(map[string]any{"janus": "ack", "transaction": tx})
	case path == "/7" && m["janus"] == "destroy":
		f.mu.Lock()
		f.destroyed = true
		f.mu.Unlock()
		reply(map[string]any{"janus": "success", "transaction": tx})
	case path == "/7/9" && m["janus"] == "trickle":
		reply(map[string]any{"janus": "ack", "transaction": tx})
	case path == "/7/9" && m["janus"] == "message":
		body, _ := m["body"].(map[string]any)
		reply(map[string]any{"janus": "ack", "transaction": tx})
		switch body["request"] {
		case "watch":
			f.queue(f.watch(body)...)
		case "start":
			jsep, _ := m["jsep"].(map[string]any)
			if jsep["type"] != "answer" {
				f.t.Errorf("start without an answer: %v", m)
			}
			sdp, _ := jsep["sdp"].(string)
			if f.start != nil {
				f.start(sdp)
			}
		}
	default:
		f.t.Errorf("unexpected %s %s %v", r.Method, r.URL.Path, m)
		w.WriteHeader(http.StatusNotFound)
	}
}

func pluginEvent(data map[string]any) map[string]any {
	return map[string]any{"janus": "event", "sender": 9, "plugindata": map[string]any{"plugin": "janus.plugin.streaming", "data": data}}
}

func TestPlayNoSuchMountpoint(t *testing.T) {
	f, server := newFakeJanus(t)
	f.watch = func(body map[string]any) []map[string]any {
		if body["id"] != "c1" || body["codec"] != "h264" {
			t.Errorf("watch %v", body)
		}
		return []map[string]any{pluginEvent(map[string]any{"streaming": "event", "error_code": 455, "error": "No such mountpoint/stream c1"})}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := Play(ctx, Grant{Server: server, Mount: "c1", Codec: "h264"}, quietLogger())
	if !errors.Is(err, ErrNoStream) {
		t.Errorf("Play = %v, want ErrNoStream", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.destroyed {
		t.Error("the session was not destroyed")
	}
}

func TestPlayNoOfferTimesOut(t *testing.T) {
	old := startTimeout
	startTimeout = 300 * time.Millisecond
	defer func() { startTimeout = old }()
	f, server := newFakeJanus(t)
	f.watch = func(map[string]any) []map[string]any { return nil }
	_, err := Play(context.Background(), Grant{Server: server, Mount: "c1"}, quietLogger())
	if err == nil || !strings.Contains(err.Error(), "start timeout") {
		t.Errorf("Play = %v, want a start timeout", err)
	}
}

func TestPlayBadGrant(t *testing.T) {
	if _, err := Play(context.Background(), Grant{Server: "wss://x/janus/", Mount: "c1"}, quietLogger()); err == nil {
		t.Error("a wss server URL was accepted")
	}
	if _, err := Play(context.Background(), Grant{Server: "https://x/janus/"}, quietLogger()); err == nil {
		t.Error("a grant without a mountpoint was accepted")
	}
}

func TestOfferFmtp(t *testing.T) {
	offer := "v=0\r\nm=video 9 UDP/TLS/RTP/SAVPF 96 126\r\na=rtpmap:96 VP8/90000\r\na=rtpmap:126 H264/90000\r\na=fmtp:126 profile-level-id=42e01f;packetization-mode=1\r\n"
	if got := offerFmtp(offer); got != "profile-level-id=42e01f;packetization-mode=1" {
		t.Errorf("fmtp %q", got)
	}
	if got := offerFmtp("v=0\r\n"); !strings.Contains(got, "packetization-mode=1") {
		t.Errorf("default fmtp %q", got)
	}
}

// h264Frame is one IDR access unit in Annex B: an SPS, a PPS and a slice.
var h264Frame = []byte{
	0, 0, 0, 1, 0x67, 0x42, 0xc0, 0x1f, 0xda, 0x01, 0x40, 0x16, 0xec, 0x04, 0x40, 0x00, 0x00, 0x03, 0x00, 0x40, 0x00, 0x00, 0x0f, 0x03, 0xc6, 0x0c, 0x92,
	0, 0, 0, 1, 0x68, 0xce, 0x3c, 0x80,
	0, 0, 0, 1, 0x65, 0x88, 0x84, 0x00, 0x33, 0xff, 0xfe, 0xf6, 0xf0, 0xfe, 0x05, 0x36, 0x56, 0x04, 0x50, 0x96, 0x7b, 0x3f, 0x53, 0xe1,
}

// TestPlayMedia plays a mountpoint a pion peer serves, end to end.
func TestPlayMedia(t *testing.T) {
	server, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	track, err := webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264, ClockRate: 90000,
		SDPFmtpLine: "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f"}, "video", "janus")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := server.AddTrack(track); err != nil {
		t.Fatal(err)
	}
	offer, err := server.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	gathered := webrtc.GatheringCompletePromise(server)
	if err := server.SetLocalDescription(offer); err != nil {
		t.Fatal(err)
	}
	<-gathered

	f, url := newFakeJanus(t)
	f.watch = func(map[string]any) []map[string]any {
		return []map[string]any{{
			"janus": "event", "sender": 9,
			"plugindata": map[string]any{"plugin": "janus.plugin.streaming", "data": map[string]any{"streaming": "event", "result": map[string]any{"status": "preparing"}}},
			"jsep":       map[string]any{"type": "offer", "sdp": server.LocalDescription().SDP},
		}}
	}
	f.start = func(answer string) {
		if err := server.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: answer}); err != nil {
			t.Errorf("apply answer: %v", err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		tick := time.NewTicker(40 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				_ = track.WriteSample(media.Sample{Data: h264Frame, Duration: 40 * time.Millisecond})
			}
		}
	}()

	stream, err := Play(ctx, Grant{Server: url, Mount: "c1", Codec: "h264"}, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4096)
	var got bytes.Buffer
	deadline := time.Now().Add(5 * time.Second)
	for got.Len() < 13+11 && time.Now().Before(deadline) {
		n, err := stream.Read(buf)
		got.Write(buf[:n])
		if err != nil {
			t.Fatalf("read: %v", err)
		}
	}
	if !bytes.HasPrefix(got.Bytes(), []byte("FLV")) {
		t.Fatalf("stream does not open with an FLV header: %q", got.Bytes()[:min(16, got.Len())])
	}

	// The server hanging up ends the stream with EOF.
	f.queue(map[string]any{"janus": "hangup", "sender": 9, "reason": "Close PC"})
	done := make(chan error, 1)
	go func() {
		_, err := io.Copy(io.Discard, stream)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("stream ended with %v, want EOF", err)
		}
	case <-time.After(5 * time.Second):
		t.Error("stream did not end after a hangup")
	}
	_ = stream.Close()
}
