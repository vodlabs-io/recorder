package younow

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
)

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestParse(t *testing.T) {
	ok := Room{Broadcast: "230905516", Token: "a.b.c", User: "63503560", Signaling: "wss://signaling.younow-prod.video.propsproject.com"}
	r, err := Parse(ok.URL())
	if err != nil || r != ok {
		t.Fatalf("Parse(URL()) = %+v, %v", r, err)
	}
	bad := []string{
		"https://www.younow.com/x",
		"younow://abc?token=a&user=1&ws=wss://signaling.younow-prod.video.propsproject.com",
		"younow://1?user=1&ws=wss://signaling.younow-prod.video.propsproject.com",
		"younow://1?token=a&ws=wss://signaling.younow-prod.video.propsproject.com",
		"younow://1?token=a&user=1&ws=wss://evil.test",
		"younow://1?token=a&user=1&ws=ws://signaling.younow-prod.video.propsproject.com",
	}
	for _, u := range bad {
		if _, err := Parse(u); err == nil {
			t.Errorf("Parse(%q) accepted", u)
		}
	}
}

// h264Frame is one IDR access unit in Annex B: an SPS, a PPS and a slice.
var h264Frame = []byte{
	0, 0, 0, 1, 0x67, 0x42, 0xc0, 0x1f, 0xda, 0x01, 0x40, 0x16, 0xec, 0x04, 0x40, 0x00, 0x00, 0x03, 0x00, 0x40, 0x00, 0x00, 0x0f, 0x03, 0xc6, 0x0c, 0x92,
	0, 0, 0, 1, 0x68, 0xce, 0x3c, 0x80,
	0, 0, 0, 1, 0x65, 0x88, 0x84, 0x00, 0x33, 0xff, 0xfe, 0xf6, 0xf0, 0xfe, 0x05, 0x36, 0x56, 0x04, 0x50, 0x96, 0x7b, 0x3f, 0x53, 0xe1,
}

// fakeSFU is a signaling socket in front of a pion peer serving H.264.
type fakeSFU struct {
	t     *testing.T
	pc    *webrtc.PeerConnection
	track *webrtc.TrackLocalStaticSample

	mu     sync.Mutex
	joins  []map[string]any
	query  []string
	kept   int
	closer func(msg string)
}

func newFakeSFU(t *testing.T) *fakeSFU {
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })
	track, err := webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264, ClockRate: 90000,
		SDPFmtpLine: "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f"}, "video", "fAB8s1VfJrRwiz2r")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pc.AddTrack(track); err != nil {
		t.Fatal(err)
	}
	return &fakeSFU{t: t, pc: pc, track: track}
}

func (f *fakeSFU) serve(w http.ResponseWriter, r *http.Request) {
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: []string{"www.younow.com"}})
	if err != nil {
		return
	}
	ctx := r.Context()
	f.mu.Lock()
	f.query = append(f.query, r.URL.RawQuery)
	f.closer = func(msg string) { _ = c.Write(context.Background(), websocket.MessageText, []byte(msg)) }
	f.mu.Unlock()
	write := func(m map[string]any) {
		b, _ := json.Marshal(m)
		_ = c.Write(ctx, websocket.MessageText, b)
	}
	for {
		_, data, err := c.Read(ctx)
		if err != nil {
			return
		}
		var m map[string]any
		_ = json.Unmarshal(data, &m)
		switch {
		case m["join"] == true:
			f.mu.Lock()
			f.joins = append(f.joins, m)
			n := len(f.joins)
			f.mu.Unlock()
			write(map[string]any{"userId": "63503560", "peerId": m["peerId"], "roomId": m["roomId"], "authKey": "k3y", "mediaServerId": "1"})
			if n > 1 {
				continue // a rejoin keeps the media it has
			}
			offer, err := f.pc.CreateOffer(nil)
			if err != nil {
				f.t.Error(err)
				return
			}
			gathered := webrtc.GatheringCompletePromise(f.pc)
			if err := f.pc.SetLocalDescription(offer); err != nil {
				f.t.Error(err)
				return
			}
			<-gathered
			write(map[string]any{
				"sdp":        map[string]any{"type": "offer", "sdp": f.pc.LocalDescription().SDP},
				"streams":    []any{map[string]any{"isHost": true, "stream": "fAB8s1VfJrRwiz2r", "userId": "15466670"}},
				"iceServers": []any{},
			})
		case m["sdp"] != nil:
			s := m["sdp"].(map[string]any)
			if err := f.pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: s["sdp"].(string)}); err != nil {
				f.t.Errorf("apply answer: %v", err)
			}
		case m["keepAlive"] == true:
			f.mu.Lock()
			f.kept++
			f.mu.Unlock()
			if m["authKey"] != "k3y" {
				f.t.Errorf("keepalive without the authKey: %v", m)
			}
		}
	}
}

func (f *fakeSFU) say(msg string) {
	f.mu.Lock()
	c := f.closer
	f.mu.Unlock()
	c(msg)
}

// TestOpenPlaysAndEnds plays a broadcast a fake SFU serves, end to end: join,
// offer and answer, media, a quiet reconnect, and the room closing.
func TestOpenPlaysAndEnds(t *testing.T) {
	f := newFakeSFU(t)
	srv := httptest.NewTLSServer(http.HandlerFunc(f.serve))
	defer srv.Close()
	// The fake answers on loopback; a real signaling host is under
	// propsproject.com, which Parse insists on.
	ws := "wss://" + strings.TrimPrefix(srv.URL, "https://")
	r := Room{Broadcast: "230905516", Token: "tok", User: "63503560", Signaling: ws}

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
				_ = f.track.WriteSample(media.Sample{Data: h264Frame, Duration: 40 * time.Millisecond})
			}
		}
	}()

	oldDial := http.DefaultTransport
	http.DefaultTransport = srv.Client().Transport
	defer func() { http.DefaultTransport = oldDial }()

	p := &playback{room: r, peerID: newUUID(), logger: quietLogger(), inbox: make(chan inbound, 64)}
	pr, pw := io.Pipe()
	pctx, pcancel := context.WithCancel(ctx)
	p.cancel, p.out = pcancel, pw
	if err := p.start(pctx); err != nil {
		t.Fatal(err)
	}
	go p.supervise(pctx)
	stream := &stream{PipeReader: pr, p: p}

	var got bytes.Buffer
	buf := make([]byte, 4096)
	deadline := time.Now().Add(5 * time.Second)
	for got.Len() < 13+11 && time.Now().Before(deadline) {
		n, err := stream.Read(buf)
		got.Write(buf[:n])
		if err != nil {
			t.Fatalf("read: %v", err)
		}
	}
	if !bytes.HasPrefix(got.Bytes(), []byte("FLV")) {
		t.Fatalf("stream does not open with an FLV header")
	}
	f.mu.Lock()
	join := f.joins[0]
	q := f.query[0]
	f.mu.Unlock()
	if join["token"] != "tok" || join["roomId"] != "230905516" || join["userId"] != float64(63503560) ||
		join["applicationId"] != applicationID || !strings.Contains(q, "isHost=false") || !strings.Contains(q, "token=tok") {
		t.Errorf("join %v query %q", join, q)
	}

	// A reconnect request is answered by a rejoin on a new socket, with the
	// authKey and without the token.
	drain := make(chan error, 1)
	go func() {
		_, err := io.Copy(io.Discard, stream)
		drain <- err
	}()
	f.say(`{"reconnect":true,"reason":"maintenance"}`)
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		n := len(f.joins)
		f.mu.Unlock()
		if n == 2 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	f.mu.Lock()
	if len(f.joins) != 2 || f.joins[1]["authKey"] != "k3y" || f.joins[1]["token"] != nil {
		t.Errorf("rejoins %v", f.joins)
	}
	f.mu.Unlock()

	// The room closing ends the stream with EOF.
	f.say(`{"disconnect":true,"reason":"room closed"}`)
	select {
	case err := <-drain:
		if err != nil {
			t.Errorf("stream ended with %v, want EOF", err)
		}
	case <-time.After(5 * time.Second):
		t.Error("stream did not end when the room closed")
	}
	_ = stream.Close()
}

func TestOfferFmtp(t *testing.T) {
	offer := "v=0\r\nm=video 9 UDP/TLS/RTP/SAVPF 100 101\r\na=rtpmap:100 H264/90000\r\na=fmtp:100 level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f\r\n"
	if got := offerFmtp(offer); got != "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f" {
		t.Errorf("fmtp %q", got)
	}
}
