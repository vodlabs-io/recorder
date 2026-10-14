// Package antmedia plays a broadcast from an Ant Media Server over WebRTC, the
// way its JavaScript player does: a signalling WebSocket that asks to play the
// stream, an offer from the server that the player answers, and trickled ICE
// candidates both ways. The broadcaster's H.264 video and Opus audio come back
// as an FLV stream, so the FLV writers record it like any HTTP-FLV origin.
package antmedia

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/pion/webrtc/v4"
	"github.com/vodlabs-io/recorder/internal/rtcflv"
)

const (
	// mediaTimeout ends a session that has produced no media for this long:
	// a broadcaster who stopped sending without the server saying so, or a
	// path that died.
	mediaTimeout = 20 * time.Second
	// pingInterval is how often the player pings the signalling socket.
	pingInterval = 10 * time.Second
)

// startTimeout bounds the wait from the play request to the first media.
var startTimeout = 20 * time.Second

// userAgent is the browser the signalling socket presents itself as.
const userAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140.0.0.0 Safari/537.36"

// h264Fmtp is the H.264 format the server offers.
const h264Fmtp = "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f"

var (
	// ErrNoStream reports a stream the server is not (or no longer) carrying.
	ErrNoStream = errors.New("antmedia: no such stream")
	// ErrUnauthorized reports a play request the server refused: a token or
	// subscriber code that is wrong or expired.
	ErrUnauthorized = errors.New("antmedia: play refused")
	// ErrFinished reports that the server ended the playback: the
	// broadcaster stopped publishing.
	ErrFinished = errors.New("antmedia: playback finished")
)

// Grant is what a viewer needs to play one stream.
type Grant struct {
	// WebSocketURL is the application's signalling endpoint,
	// wss://<host>:5443/<application>/websocket.
	WebSocketURL string
	StreamID     string
	// Token is a one-time play token; "" when the server wants none.
	Token string
	// SubscriberID and SubscriberCode identify a time-based subscriber; both
	// are "" when the server wants none.
	SubscriberID   string
	SubscriberCode string
}

func (g Grant) dialURL() (string, error) {
	u, err := url.Parse(g.WebSocketURL)
	if err != nil || (u.Scheme != "wss" && u.Scheme != "ws") || u.Host == "" {
		return "", fmt.Errorf("antmedia: bad signalling url %q", g.WebSocketURL)
	}
	if g.SubscriberID != "" {
		q := u.Query()
		q.Set("token", g.token())
		q.Set("subscriberId", g.SubscriberID)
		q.Set("subscriberCode", g.SubscriberCode)
		u.RawQuery = q.Encode()
	}
	return u.String(), nil
}

func (g Grant) token() string {
	if g.Token == "" {
		return "null"
	}
	return g.Token
}

// message is one signalling frame, either way.
type message struct {
	Command    string       `json:"command"`
	StreamID   string       `json:"streamId,omitempty"`
	Definition string       `json:"definition,omitempty"`
	Type       string       `json:"type,omitempty"`
	SDP        string       `json:"sdp,omitempty"`
	Label      *uint16      `json:"label,omitempty"`
	ID         string       `json:"id,omitempty"`
	Candidate  string       `json:"candidate,omitempty"`
	StreamInfo []streamInfo `json:"streamInfo,omitempty"`
	// StreamHeight picks a rendition in forceStreamQuality.
	StreamHeight int `json:"streamHeight,omitempty"`
}

// streamInfo is one rendition the server can play.
type streamInfo struct {
	StreamWidth  int `json:"streamWidth"`
	StreamHeight int `json:"streamHeight"`
	VideoBitrate int `json:"videoBitrate"`
}

// Play plays the stream g names and returns it as an FLV stream: AVC video
// and Opus audio on a timeline starting at zero, at the tallest rendition the
// server offers. Reading ends with io.EOF when the server finishes the
// playback, or with the error that ended it. Close, or the end of ctx, stops
// the playback.
//
// The signalling socket is dialled with a client of its own: it can stay
// quiet for longer than the per-read deadlines a stream client sets.
func Play(ctx context.Context, g Grant, logger *slog.Logger) (io.ReadCloser, error) {
	dialURL, err := g.dialURL()
	if err != nil {
		return nil, err
	}
	if g.StreamID == "" {
		return nil, errors.New("antmedia: grant names no stream")
	}
	ctx, cancel := context.WithCancel(ctx)
	pr, pw := io.Pipe()
	s := &session{
		grant:  g,
		logger: logger.With("antmedia_stream", g.StreamID),
		cancel: cancel,
		out:    pw,
		inbox:  make(chan message, 64),
		done:   make(chan struct{}),
	}
	if err := s.connect(ctx, &http.Client{}, dialURL); err != nil {
		s.teardown()
		return nil, err
	}
	go s.supervise(ctx)
	return &stream{PipeReader: pr, s: s}, nil
}

type stream struct {
	*io.PipeReader
	s *session
}

func (r *stream) Close() error {
	// Closing the reader first releases a muxer blocked writing to it.
	err := r.PipeReader.Close()
	r.s.finish(errors.New("antmedia: stream closed"))
	return err
}

// session is one playback.
type session struct {
	grant  Grant
	logger *slog.Logger
	cancel context.CancelFunc
	conn   *websocket.Conn
	pc     *webrtc.PeerConnection
	media  *rtcflv.Receiver
	out    *io.PipeWriter

	writeMu sync.Mutex
	inbox   chan message
	done    chan struct{} // closed when the socket's read loop ends
	readErr error

	// height is the rendition asked for once playback starts; 0 leaves the
	// server's choice.
	height int

	endOnce sync.Once
}

func (s *session) connect(ctx context.Context, client *http.Client, dialURL string) error {
	dctx, dcancel := context.WithTimeout(ctx, 10*time.Second)
	conn, _, err := websocket.Dial(dctx, dialURL, &websocket.DialOptions{
		HTTPClient: client,
		HTTPHeader: http.Header{"User-Agent": {userAgent}},
	})
	dcancel()
	if err != nil {
		return fmt.Errorf("dial antmedia signalling: %w", err)
	}
	conn.SetReadLimit(4 << 20)
	s.conn = conn
	go s.readLoop()

	pc, err := rtcflv.NewPeerConnection(webrtc.Configuration{}, rtcflv.PeerOptions{H264Fmtp: h264Fmtp})
	if err != nil {
		return err
	}
	s.pc = pc
	s.media = rtcflv.NewReceiver(pc, s.out, s.finish)
	pc.OnTrack(s.media.OnTrack)
	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		ci := c.ToJSON()
		m := message{Command: "takeCandidate", StreamID: s.grant.StreamID, Candidate: ci.Candidate, Label: ci.SDPMLineIndex}
		if ci.SDPMid != nil {
			m.ID = *ci.SDPMid
		}
		_ = s.send(ctx, m)
	})

	if err := s.send(ctx, message{Command: "getStreamInfo", StreamID: s.grant.StreamID}); err != nil {
		return err
	}
	if err := s.send(ctx, s.playRequest()); err != nil {
		return err
	}

	// Wait for the server's offer, answer it, and hold on until the first
	// media arrives or the server refuses.
	tctx, tcancel := context.WithTimeout(ctx, startTimeout)
	defer tcancel()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	start := time.Now()
	for {
		select {
		case <-tctx.Done():
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return errors.New("antmedia: no media within the start timeout")
		case <-s.done:
			return fmt.Errorf("antmedia signalling closed: %w", s.readErr)
		case m := <-s.inbox:
			if err := s.handle(ctx, m); err != nil {
				return err
			}
		case <-tick.C:
			if s.media.Idle() < time.Since(start) {
				s.logger.Info("Playing Ant Media stream", "height", s.height)
				return nil
			}
		}
	}
}

func (s *session) playRequest() message {
	return message{Command: "play", StreamID: s.grant.StreamID}
}

// send writes m to the signalling socket. The play request carries the
// grant's credentials alongside.
func (s *session) send(ctx context.Context, m message) error {
	var v any = m
	if m.Command == "play" {
		v = map[string]any{
			"command": "play", "streamId": m.StreamID, "token": s.grant.token(),
			"subscriberId": s.grant.SubscriberID, "subscriberCode": s.grant.SubscriberCode,
		}
	}
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return s.conn.Write(wctx, websocket.MessageText, b)
}

func (s *session) readLoop() {
	defer close(s.done)
	for {
		_, data, err := s.conn.Read(context.Background())
		if err != nil {
			s.readErr = err
			return
		}
		var m message
		if json.Unmarshal(data, &m) != nil {
			continue
		}
		select {
		case s.inbox <- m:
		default:
			// Notifications are advisory; a full queue drops the news.
		}
	}
}

// handle acts on one message from the server. The error it returns ends the
// session.
func (s *session) handle(ctx context.Context, m message) error {
	switch m.Command {
	case "streamInformation":
		s.height = tallest(m.StreamInfo)
	case "takeConfiguration":
		if m.Type != "offer" {
			return nil
		}
		if err := s.pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: m.SDP}); err != nil {
			return fmt.Errorf("apply antmedia offer: %w", err)
		}
		answer, err := s.pc.CreateAnswer(nil)
		if err != nil {
			return err
		}
		if err := s.pc.SetLocalDescription(answer); err != nil {
			return err
		}
		return s.send(ctx, message{Command: "takeConfiguration", StreamID: s.grant.StreamID, Type: "answer", SDP: answer.SDP})
	case "takeCandidate":
		if m.Candidate == "" {
			return nil
		}
		init := webrtc.ICECandidateInit{Candidate: m.Candidate, SDPMLineIndex: m.Label}
		if m.ID != "" {
			id := m.ID
			init.SDPMid = &id
		}
		if err := s.pc.AddICECandidate(init); err != nil {
			s.logger.Debug("Ant Media candidate not added", "error", err)
		}
	case "notification":
		switch m.Definition {
		case "play_started":
			if s.height > 0 {
				// The server may start on a lower rendition than the tallest.
				_ = s.send(ctx, message{Command: "forceStreamQuality", StreamID: s.grant.StreamID, StreamHeight: s.height})
			}
		case "play_finished":
			return ErrFinished
		}
	case "stop":
		return ErrFinished
	case "error":
		switch m.Definition {
		case "no_stream_exist", "not_initialized_yet":
			return ErrNoStream
		case "unauthorized_access", "subscriberBlocked":
			return fmt.Errorf("%w: %s", ErrUnauthorized, m.Definition)
		case "highResourceUsage", "already_playing", "no_peer_associated_before":
			return fmt.Errorf("antmedia: server error %s", m.Definition)
		}
		s.logger.Debug("Ant Media server error", "definition", m.Definition)
	}
	return nil
}

// tallest is the height of the tallest rendition listed, or 0 for none.
func tallest(infos []streamInfo) int {
	h := 0
	for _, i := range infos {
		h = max(h, i.StreamHeight)
	}
	return h
}

// supervise watches the server's messages and the media watchdog until the
// session ends.
func (s *session) supervise(ctx context.Context) {
	ping := time.NewTicker(pingInterval)
	defer ping.Stop()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			s.finish(ctx.Err())
			return
		case <-s.done:
			s.finish(fmt.Errorf("antmedia signalling closed: %w", s.readErr))
			return
		case m := <-s.inbox:
			if err := s.handle(ctx, m); err != nil {
				if errors.Is(err, ErrFinished) || errors.Is(err, ErrNoStream) {
					s.logger.Info("Ant Media playback ended", "reason", err)
				}
				s.finish(err)
				return
			}
		case <-ping.C:
			if err := s.send(ctx, message{Command: "ping"}); err != nil && ctx.Err() == nil {
				s.logger.Debug("Ant Media ping failed", "error", err)
			}
		case <-tick.C:
			if idle := s.media.Idle(); idle > mediaTimeout {
				s.finish(fmt.Errorf("antmedia: no media for %s", idle.Round(time.Second)))
				return
			}
			switch s.pc.ConnectionState() {
			case webrtc.PeerConnectionStateFailed, webrtc.PeerConnectionStateClosed:
				s.finish(fmt.Errorf("antmedia: media connection %s", s.pc.ConnectionState()))
				return
			}
		}
	}
}

// finish ends the session once: the stream's reader sees err (io.EOF for a
// playback the server finished), and the playback stops.
func (s *session) finish(err error) {
	s.endOnce.Do(func() {
		if errors.Is(err, ErrFinished) || errors.Is(err, ErrNoStream) {
			err = io.EOF
		}
		s.out.CloseWithError(err)
		s.teardown()
	})
}

func (s *session) teardown() {
	s.cancel()
	if s.conn != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = s.send(ctx, message{Command: "stop", StreamID: s.grant.StreamID})
		cancel()
		_ = s.conn.Close(websocket.StatusNormalClosure, "")
	}
	if s.pc != nil {
		_ = s.pc.Close()
	}
}
