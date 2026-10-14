// Package younow records a YouNow broadcast as an FLV stream.
//
// YouNow plays broadcasts over WebRTC from the Props video stack's SFU. A
// viewer joins the broadcast's room on the stack's signaling socket with a
// video token YouNow mints for a signed-in account; the SFU then offers the
// broadcaster's H.264 and Opus, the viewer answers, and both sides trickle
// ICE candidates. The socket stays open for the playback, kept alive every
// 5 s, and is where the SFU says the room closed. The token admits a join for
// 20 s after it is minted, so a younow:// URL is used at once.
//
// Its form is younow://<broadcast id>?token=<video token>&user=<viewer's user
// id>&ws=<signaling socket URL>.
package younow

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/pion/webrtc/v4"
	"github.com/vodlabs-io/recorder/internal/rtcflv"
)

// Scheme is the URL scheme of a YouNow broadcast.
const Scheme = "younow"

const (
	siteOrigin = "https://www.younow.com"
	userAgent  = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140.0.0.0 Safari/537.36"
	// applicationID and sdkVersion are what the web player names itself.
	applicationID = "YouNow-Web"
	sdkVersion    = "js-1.1.98"
	// maxBandwidth is the web player's maxBw, in kbit/s.
	maxBandwidth = 1000

	keepaliveInterval = 5 * time.Second
	// mediaTimeout ends a playback that has produced no media for this long.
	mediaTimeout = 20 * time.Second
	writeTimeout = 10 * time.Second
	// reconnects bounds the quiet reconnects the SFU may ask for in a row.
	reconnects = 5
)

// startTimeout bounds the wait from the join to the first media.
var startTimeout = 20 * time.Second

// ErrRoomClosed reports that the SFU closed the room: the broadcast ended.
var ErrRoomClosed = errors.New("younow: the broadcast's room closed")

// Room is a younow:// URL.
type Room struct {
	Broadcast string
	Token     string
	User      string
	Signaling string
}

var numeric = regexp.MustCompile(`^[1-9][0-9]{0,19}$`)

// IsURL reports whether u is a younow:// URL.
func IsURL(u string) bool {
	return strings.HasPrefix(u, Scheme+"://")
}

// Parse decodes a younow:// URL.
func Parse(raw string) (Room, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return Room{}, fmt.Errorf("parse younow url: %w", err)
	}
	if u.Scheme != Scheme {
		return Room{}, fmt.Errorf("not a younow url: scheme %q", u.Scheme)
	}
	q := u.Query()
	r := Room{Broadcast: u.Host, Token: q.Get("token"), User: q.Get("user"), Signaling: q.Get("ws")}
	switch {
	case !numeric.MatchString(r.Broadcast):
		return Room{}, errors.New("younow url lacks a broadcast id")
	case r.Token == "":
		return Room{}, errors.New("younow url lacks a video token")
	case !numeric.MatchString(r.User):
		return Room{}, errors.New("younow url lacks a viewer")
	}
	ws, err := url.Parse(r.Signaling)
	if err != nil || ws.Scheme != "wss" || !strings.HasSuffix(ws.Hostname(), ".propsproject.com") {
		return Room{}, fmt.Errorf("younow url names an unknown signaling socket %q", r.Signaling)
	}
	return r, nil
}

// URL encodes r as a younow:// URL.
func (r Room) URL() string {
	q := url.Values{"token": {r.Token}, "user": {r.User}, "ws": {r.Signaling}}
	return (&url.URL{Scheme: Scheme, Host: r.Broadcast, RawQuery: q.Encode()}).String()
}

// Open joins the broadcast u names and returns it as an FLV stream on a
// timeline starting at zero: AVC video and Opus audio. Reading ends with
// io.EOF when the room closes, or with the error that ended the playback.
// Close, or the end of ctx, stops it.
func Open(ctx context.Context, u string, logger *slog.Logger) (io.ReadCloser, error) {
	r, err := Parse(u)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	pr, pw := io.Pipe()
	p := &playback{
		room:   r,
		peerID: newUUID(),
		logger: logger.With("younow_broadcast", r.Broadcast),
		cancel: cancel,
		out:    pw,
		inbox:  make(chan inbound, 64),
	}
	if err := p.start(ctx); err != nil {
		p.finish(err)
		return nil, err
	}
	go p.supervise(ctx)
	return &stream{PipeReader: pr, p: p}, nil
}

type stream struct {
	*io.PipeReader
	p *playback
}

func (s *stream) Close() error {
	// Closing the reader first releases a muxer blocked writing to it.
	err := s.PipeReader.Close()
	s.p.finish(errors.New("younow: stream closed"))
	return err
}

// signal is a message on the signaling socket, either way. Every message the
// client sends repeats who it is (see base).
type signal struct {
	Join          bool            `json:"join,omitempty"`
	Token         string          `json:"token,omitempty"`
	SDPSemantics  string          `json:"sdpSemantics,omitempty"`
	KeepAlive     bool            `json:"keepAlive,omitempty"`
	Visibility    string          `json:"browserVisibilityState,omitempty"`
	SDP           *sdp            `json:"sdp,omitempty"`
	ICE           *ice            `json:"ice,omitempty"`
	ICEDone       bool            `json:"iceDone,omitempty"`
	Disconnect    bool            `json:"disconnect,omitempty"`
	Reconnect     bool            `json:"reconnect,omitempty"`
	Reason        json.RawMessage `json:"reason,omitempty"`
	AuthKey       string          `json:"authKey,omitempty"`
	MediaServerID string          `json:"mediaServerId,omitempty"`
	Streams       []streamInfo    `json:"streams,omitempty"`
	ICEServers    []iceServer     `json:"iceServers,omitempty"`
}

type sdp struct {
	SDP  string `json:"sdp"`
	Type string `json:"type"`
}

type ice struct {
	Candidate        string  `json:"candidate"`
	SDPMid           *string `json:"sdpMid,omitempty"`
	SDPMLineIndex    *uint16 `json:"sdpMLineIndex,omitempty"`
	UsernameFragment *string `json:"usernameFragment,omitempty"`
}

type streamInfo struct {
	IsHost bool   `json:"isHost"`
	Stream string `json:"stream"`
	UserID string `json:"userId"`
}

type iceServer struct {
	URLs       []string `json:"urls"`
	Username   string   `json:"username,omitempty"`
	Credential string   `json:"credential,omitempty"`
}

// inbound is a message read from a socket, or the error that ended it.
type inbound struct {
	sock *socket
	msg  signal
	err  error
}

// socket is one connection to the signaling server.
type socket struct {
	conn *websocket.Conn
	wmu  sync.Mutex
}

// playback is one join of a broadcast's room.
type playback struct {
	room   Room
	peerID string
	logger *slog.Logger
	cancel context.CancelFunc
	out    *io.PipeWriter

	mu      sync.Mutex
	sock    *socket
	authKey string
	pc      *webrtc.PeerConnection
	media   *rtcflv.Receiver
	tracks  map[webrtc.RTPCodecType]bool

	inbox      chan inbound
	answeredAt time.Time
	endOnce    sync.Once
}

// base is what every message the client sends repeats.
func (p *playback) base() map[string]any {
	p.mu.Lock()
	defer p.mu.Unlock()
	user, _ := strconv.ParseInt(p.room.User, 10, 64)
	m := map[string]any{
		"peerId":        p.peerID,
		"userId":        user,
		"authKey":       nil,
		"roomId":        p.room.Broadcast,
		"applicationId": applicationID,
		"sdkVersion":    sdkVersion,
		"device":        "Chrome-140.0.0.0",
		"os":            "Mac OS-10.15.7",
		"maxBw":         maxBandwidth,
	}
	if p.authKey != "" {
		m["authKey"] = p.authKey
	}
	return m
}

// dial opens a signaling socket, as the web player does: who it is travels in
// the URL too.
func (p *playback) dial(ctx context.Context) (*socket, error) {
	q := url.Values{
		"peerId": {p.peerID},
		"roomId": {p.room.Broadcast},
		"userId": {p.room.User},
		"token":  {p.room.Token},
		"isHost": {"false"},
	}
	dctx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	conn, _, err := websocket.Dial(dctx, p.room.Signaling+"?"+q.Encode(), &websocket.DialOptions{
		HTTPHeader: http.Header{"Origin": {siteOrigin}, "User-Agent": {userAgent}},
	})
	if err != nil {
		return nil, fmt.Errorf("dial younow signaling: %w", err)
	}
	conn.SetReadLimit(1 << 20)
	s := &socket{conn: conn}
	go p.readLoop(ctx, s)
	return s, nil
}

func (p *playback) readLoop(ctx context.Context, s *socket) {
	for {
		_, data, err := s.conn.Read(ctx)
		if err != nil {
			p.deliver(ctx, inbound{sock: s, err: err})
			return
		}
		var m signal
		if json.Unmarshal(data, &m) != nil {
			p.logger.Debug("Unreadable YouNow signal", "bytes", len(data))
			continue
		}
		p.deliver(ctx, inbound{sock: s, msg: m})
	}
}

func (p *playback) deliver(ctx context.Context, in inbound) {
	select {
	case p.inbox <- in:
	case <-ctx.Done():
	}
}

// send writes a message on the current socket, with base merged in.
func (p *playback) send(ctx context.Context, fields map[string]any) error {
	p.mu.Lock()
	s := p.sock
	p.mu.Unlock()
	if s == nil {
		return errors.New("younow: no signaling socket")
	}
	m := p.base()
	for k, v := range fields {
		m[k] = v
	}
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	wctx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	s.wmu.Lock()
	defer s.wmu.Unlock()
	return s.conn.Write(wctx, websocket.MessageText, b)
}

// start joins the room and waits for the first media.
func (p *playback) start(ctx context.Context) error {
	s, err := p.dial(ctx)
	if err != nil {
		return err
	}
	p.mu.Lock()
	p.sock = s
	p.mu.Unlock()
	if err := p.send(ctx, map[string]any{"join": true, "token": p.room.Token, "sdpSemantics": "unified-plan"}); err != nil {
		return fmt.Errorf("join younow room: %w", err)
	}
	tctx, cancel := context.WithTimeout(ctx, startTimeout)
	defer cancel()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-tctx.Done():
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return errors.New("younow: no media within the start timeout")
		case in := <-p.inbox:
			if in.err != nil {
				return fmt.Errorf("younow signaling closed before playback: %w", in.err)
			}
			if err := p.handle(ctx, in.msg); err != nil {
				return err
			}
		case <-tick.C:
			if p.media != nil && p.media.Idle() < time.Since(p.answeredAt) {
				p.logger.Info("Playing YouNow broadcast")
				return nil
			}
		}
	}
}

// handle acts on one message from the server. The error it returns ends the
// playback.
func (p *playback) handle(ctx context.Context, m signal) error {
	switch {
	case m.Disconnect:
		p.logger.Info("YouNow SFU closed the room", "reason", string(m.Reason))
		return ErrRoomClosed
	case m.SDP != nil && m.SDP.Type == "offer":
		return p.answer(ctx, m)
	case m.ICE != nil:
		if p.pc == nil || m.ICE.Candidate == "" {
			return nil
		}
		c := webrtc.ICECandidateInit{Candidate: m.ICE.Candidate, SDPMid: m.ICE.SDPMid, SDPMLineIndex: m.ICE.SDPMLineIndex}
		if err := p.pc.AddICECandidate(c); err != nil {
			p.logger.Debug("YouNow ICE candidate refused", "error", err)
		}
	case m.AuthKey != "":
		p.mu.Lock()
		p.authKey = m.AuthKey
		p.mu.Unlock()
		p.logger.Debug("Joined YouNow room", "media_server", m.MediaServerID)
	}
	return nil
}

var h264Rtpmap = regexp.MustCompile(`(?m)^a=rtpmap:(\d+) H264/90000\r?$`)

// answer applies the SFU's offer, and sends the answer.
func (p *playback) answer(ctx context.Context, m signal) error {
	offer := webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: m.SDP.SDP}
	if p.pc == nil {
		var servers []webrtc.ICEServer
		for _, s := range m.ICEServers {
			servers = append(servers, webrtc.ICEServer{URLs: s.URLs, Username: s.Username, Credential: s.Credential})
		}
		pc, err := rtcflv.NewPeerConnection(webrtc.Configuration{ICEServers: servers}, rtcflv.PeerOptions{H264Fmtp: offerFmtp(m.SDP.SDP)})
		if err != nil {
			return err
		}
		p.pc = pc
		p.media = rtcflv.NewReceiver(pc, p.out, p.finish)
		p.tracks = map[webrtc.RTPCodecType]bool{}
		pc.OnTrack(p.onTrack)
		pc.OnICECandidate(func(c *webrtc.ICECandidate) {
			if c == nil {
				return
			}
			ci := c.ToJSON()
			if err := p.send(ctx, map[string]any{"ice": ice{Candidate: ci.Candidate, SDPMid: ci.SDPMid, SDPMLineIndex: ci.SDPMLineIndex, UsernameFragment: ci.UsernameFragment}}); err != nil && ctx.Err() == nil {
				p.logger.Debug("YouNow trickle failed", "error", err)
			}
		})
		for _, s := range m.Streams {
			p.logger.Debug("YouNow stream offered", "host", s.IsHost, "stream", s.Stream, "user", s.UserID)
		}
	}
	if err := p.pc.SetRemoteDescription(offer); err != nil {
		return fmt.Errorf("apply younow offer: %w", err)
	}
	answer, err := p.pc.CreateAnswer(nil)
	if err != nil {
		return err
	}
	if err := p.pc.SetLocalDescription(answer); err != nil {
		return err
	}
	if p.answeredAt.IsZero() {
		p.answeredAt = time.Now()
	}
	return p.send(ctx, map[string]any{"sdp": sdp{SDP: answer.SDP, Type: "answer"}})
}

// onTrack records the first video and the first audio track. The SFU lists
// the broadcaster first; a guest on stage comes after.
func (p *playback) onTrack(t *webrtc.TrackRemote, rcv *webrtc.RTPReceiver) {
	p.mu.Lock()
	taken := p.tracks[t.Kind()]
	p.tracks[t.Kind()] = true
	p.mu.Unlock()
	p.logger.Debug("YouNow track", "kind", t.Kind().String(), "stream", t.StreamID(), "codec", t.Codec().MimeType, "recorded", !taken)
	if taken {
		return
	}
	p.media.OnTrack(t, rcv)
}

// offerFmtp is the format line of the offer's first H.264 payload, or a
// baseline one when the offer has none.
func offerFmtp(offer string) string {
	if m := h264Rtpmap.FindStringSubmatch(offer); m != nil {
		re := regexp.MustCompile(`(?m)^a=fmtp:` + m[1] + ` (.+?)\r?$`)
		if f := re.FindStringSubmatch(offer); f != nil {
			return f[1]
		}
	}
	return "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f"
}

// supervise keeps the socket alive and watches the playback until it ends.
// A socket the SFU asks to replace, or that drops, is reopened and the room
// rejoined with the authKey the first join was given.
func (p *playback) supervise(ctx context.Context) {
	keepalive := time.NewTicker(keepaliveInterval)
	defer keepalive.Stop()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	redials := 0
	for {
		select {
		case <-ctx.Done():
			p.finish(ctx.Err())
			return
		case in := <-p.inbox:
			p.mu.Lock()
			current := in.sock == p.sock
			p.mu.Unlock()
			if !current {
				continue
			}
			switch {
			case in.err != nil, in.msg.Reconnect:
				if in.msg.Reconnect {
					p.logger.Info("YouNow SFU asked for a reconnect", "reason", string(in.msg.Reason))
				} else {
					p.logger.Info("YouNow signaling dropped", "error", in.err)
				}
				redials++
				if redials > reconnects {
					p.finish(fmt.Errorf("younow: signaling dropped %d times", redials))
					return
				}
				if err := p.rejoin(ctx, in.sock); err != nil {
					p.finish(err)
					return
				}
			default:
				if err := p.handle(ctx, in.msg); err != nil {
					p.finish(err)
					return
				}
			}
		case <-keepalive.C:
			if err := p.send(ctx, map[string]any{"keepAlive": true, "browserVisibilityState": "visible"}); err != nil && ctx.Err() == nil {
				p.logger.Debug("YouNow keepalive failed", "error", err)
			}
		case <-tick.C:
			if idle := p.media.Idle(); idle > mediaTimeout {
				p.finish(fmt.Errorf("younow: no media for %s", idle.Round(time.Second)))
				return
			} else if idle < 2*time.Second {
				redials = 0
			}
			switch p.pc.ConnectionState() {
			case webrtc.PeerConnectionStateFailed, webrtc.PeerConnectionStateClosed:
				p.finish(fmt.Errorf("younow: media connection %s", p.pc.ConnectionState()))
				return
			}
		}
	}
}

// rejoin replaces old with a new socket and rejoins the room on it, as the web
// player's quiet reconnect does.
func (p *playback) rejoin(ctx context.Context, old *socket) error {
	old.conn.Close(websocket.StatusCode(4501), "reconnect")
	s, err := p.dial(ctx)
	if err != nil {
		return err
	}
	p.mu.Lock()
	p.sock = s
	p.mu.Unlock()
	return p.send(ctx, map[string]any{"join": true})
}

// finish ends the playback once: the stream's reader sees err (io.EOF for a
// room the SFU closed), and the connections close.
func (p *playback) finish(err error) {
	p.endOnce.Do(func() {
		if errors.Is(err, ErrRoomClosed) {
			err = io.EOF
		}
		p.out.CloseWithError(err)
		p.cancel()
		p.mu.Lock()
		s := p.sock
		p.mu.Unlock()
		if s != nil {
			s.conn.Close(websocket.StatusNormalClosure, "")
		}
		if p.pc != nil {
			_ = p.pc.Close()
		}
	})
}

func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}
