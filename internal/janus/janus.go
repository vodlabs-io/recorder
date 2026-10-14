// Package janus plays a mountpoint of a Janus WebRTC server's streaming plugin
// the way janus.js does over its HTTP transport: create a session, attach the
// streaming plugin, ask to watch the mountpoint, answer the server's offer,
// and long-poll the session for events. The broadcaster's H.264 (or VP8)
// video and Opus audio come back as an FLV stream, so the FLV writers record
// it like any HTTP-FLV origin.
package janus

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
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

	"github.com/pion/webrtc/v4"
	"github.com/vodlabs-io/recorder/internal/rtcflv"
)

const (
	// mediaTimeout ends a session that has produced no media for this long.
	mediaTimeout = 20 * time.Second
	// keepaliveInterval is how often the session is kept alive, as janus.js
	// does.
	keepaliveInterval = 25 * time.Second
	// pollTimeout bounds one long poll; the server answers an idle one
	// within 30 s.
	pollTimeout = 50 * time.Second
	// pollFailures is how many long polls in a row may fail before the
	// session is given up.
	pollFailures = 3
)

// startTimeout bounds the wait from the watch request to the first media.
var startTimeout = 20 * time.Second

// userAgent is the browser the requests present themselves as.
const userAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140.0.0.0 Safari/537.36"

const plugin = "janus.plugin.streaming"

var (
	// ErrNoStream reports a mountpoint the server is not (or no longer)
	// carrying.
	ErrNoStream = errors.New("janus: no such stream")
	// ErrFinished reports that the server ended the playback.
	ErrFinished = errors.New("janus: playback finished")
)

// Grant is what a viewer needs to watch one mountpoint.
type Grant struct {
	// Server is the HTTP transport's endpoint; sessions are made under it.
	Server string
	// Mount is the streaming plugin's mountpoint id.
	Mount string
	// Codec, when set, asks the mountpoint for that video codec ("h264").
	Codec string
	// Origin and Referer, when set, are sent as those headers.
	Origin  string
	Referer string
}

// Play watches the mountpoint g names and returns it as an FLV stream: AVC (or
// VP8) video and Opus audio on a timeline starting at zero. Reading ends with
// io.EOF when the server ends the playback, or with the error that ended it.
// Close, or the end of ctx, stops the playback.
//
// The long polls use a client of their own: they stay open for longer than
// the per-read deadlines a stream client sets.
func Play(ctx context.Context, g Grant, logger *slog.Logger) (io.ReadCloser, error) {
	u, err := url.Parse(g.Server)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return nil, fmt.Errorf("janus: bad server url %q", g.Server)
	}
	if g.Mount == "" {
		return nil, errors.New("janus: grant names no mountpoint")
	}
	ctx, cancel := context.WithCancel(ctx)
	pr, pw := io.Pipe()
	s := &session{
		grant:  g,
		base:   strings.TrimRight(g.Server, "/"),
		client: &http.Client{Timeout: pollTimeout},
		logger: logger.With("janus_mount", g.Mount),
		cancel: cancel,
		out:    pw,
		inbox:  make(chan event, 64),
		done:   make(chan struct{}),
	}
	if err := s.connect(ctx); err != nil {
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
	r.s.finish(errors.New("janus: stream closed"))
	return err
}

// event is one message from the server, either a reply or a polled event.
type event struct {
	Janus       string          `json:"janus"`
	Transaction string          `json:"transaction,omitempty"`
	Sender      int64           `json:"sender,omitempty"`
	Data        json.RawMessage `json:"data,omitempty"`
	Error       *struct {
		Code   int    `json:"code"`
		Reason string `json:"reason"`
	} `json:"error,omitempty"`
	PluginData *struct {
		Plugin string          `json:"plugin"`
		Data   json.RawMessage `json:"data"`
	} `json:"plugindata,omitempty"`
	JSEP *struct {
		Type string `json:"type"`
		SDP  string `json:"sdp"`
	} `json:"jsep,omitempty"`
	Reason string `json:"reason,omitempty"`
	// Type and Receiving describe a "media" event.
	Type      string `json:"type,omitempty"`
	Receiving *bool  `json:"receiving,omitempty"`
}

// pluginReply is the streaming plugin's part of an event.
type pluginReply struct {
	ErrorCode int    `json:"error_code"`
	Error     string `json:"error"`
	Result    *struct {
		Status string `json:"status"`
	} `json:"result"`
}

// session is one playback.
type session struct {
	grant    Grant
	base     string
	client   *http.Client
	logger   *slog.Logger
	cancel   context.CancelFunc
	id       int64
	handleID int64
	pc       *webrtc.PeerConnection
	media    *rtcflv.Receiver
	out      *io.PipeWriter
	// answeredAt is when the first answer was made, after the receiver.
	answeredAt time.Time

	inbox   chan event
	done    chan struct{} // closed when the poll loop ends
	pollErr error

	endOnce sync.Once
}

func (s *session) connect(ctx context.Context) error {
	var created struct {
		ID int64 `json:"id"`
	}
	if err := s.request(ctx, s.base, map[string]any{"janus": "create"}, &created); err != nil {
		return fmt.Errorf("create janus session: %w", err)
	}
	s.id = created.ID
	var attached struct {
		ID int64 `json:"id"`
	}
	if err := s.request(ctx, s.sessionURL(), map[string]any{"janus": "attach", "plugin": plugin, "opaque_id": "streamingtest-" + randomString(12)}, &attached); err != nil {
		return fmt.Errorf("attach janus streaming plugin: %w", err)
	}
	s.handleID = attached.ID
	go s.pollLoop(ctx)

	watch := map[string]any{"request": "watch", "id": s.grant.Mount}
	if s.grant.Codec != "" {
		watch["codec"] = s.grant.Codec
	}
	if err := s.message(ctx, watch, nil); err != nil {
		return err
	}

	// Wait for the server's offer, answer it, and hold on until the first
	// media arrives or the server refuses.
	tctx, tcancel := context.WithTimeout(ctx, startTimeout)
	defer tcancel()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-tctx.Done():
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return errors.New("janus: no media within the start timeout")
		case <-s.done:
			return fmt.Errorf("janus session lost: %w", s.pollErr)
		case e := <-s.inbox:
			if err := s.handle(ctx, e); err != nil {
				return err
			}
		case <-tick.C:
			if s.media != nil && s.media.Idle() < time.Since(s.answeredAt) {
				s.logger.Info("Playing Janus stream")
				return nil
			}
		}
	}
}

func (s *session) sessionURL() string {
	return s.base + "/" + strconv.FormatInt(s.id, 10)
}

func (s *session) handleURL() string {
	return s.sessionURL() + "/" + strconv.FormatInt(s.handleID, 10)
}

// message sends the streaming plugin a request, with an SDP when jsep is set.
func (s *session) message(ctx context.Context, body map[string]any, jsep *webrtc.SessionDescription) error {
	m := map[string]any{"janus": "message", "body": body}
	if jsep != nil {
		m["jsep"] = map[string]string{"type": jsep.Type.String(), "sdp": jsep.SDP}
	}
	if err := s.request(ctx, s.handleURL(), m, nil); err != nil {
		return fmt.Errorf("janus %v request: %w", body["request"], err)
	}
	return nil
}

// request posts m with a fresh transaction and decodes the reply's data into
// data when it is set. A reply of "ack" carries none.
func (s *session) request(ctx context.Context, endpoint string, m map[string]any, data any) error {
	m["transaction"] = randomString(12)
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	rctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, http.MethodPost, endpoint, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	s.headers(req)
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	var e event
	if err := json.Unmarshal(body, &e); err != nil {
		return err
	}
	switch e.Janus {
	case "success", "ack":
	case "error":
		if e.Error != nil {
			return fmt.Errorf("janus error %d: %s", e.Error.Code, e.Error.Reason)
		}
		return errors.New("janus error")
	default:
		return fmt.Errorf("unexpected janus reply %q", e.Janus)
	}
	if data != nil {
		return json.Unmarshal(e.Data, data)
	}
	// A plugin reply may come back on the request itself.
	if e.PluginData != nil || e.JSEP != nil {
		e.Janus = "event"
		s.deliver(e)
	}
	return nil
}

func (s *session) headers(req *http.Request) {
	req.Header.Set("User-Agent", userAgent)
	if s.grant.Origin != "" {
		req.Header.Set("Origin", s.grant.Origin)
	}
	if s.grant.Referer != "" {
		req.Header.Set("Referer", s.grant.Referer)
	}
}

func (s *session) deliver(e event) {
	select {
	case s.inbox <- e:
	default:
		// Events are advisory; a full queue drops the news.
	}
}

// pollLoop long-polls the session for events until it ends.
func (s *session) pollLoop(ctx context.Context) {
	defer close(s.done)
	failures := 0
	for ctx.Err() == nil {
		events, err := s.poll(ctx)
		if err != nil {
			if ctx.Err() != nil {
				s.pollErr = ctx.Err()
				return
			}
			failures++
			if failures >= pollFailures {
				s.pollErr = err
				return
			}
			s.logger.Debug("Janus poll failed", "error", err)
			select {
			case <-ctx.Done():
			case <-time.After(time.Second):
			}
			continue
		}
		failures = 0
		for _, e := range events {
			if e.Janus == "keepalive" {
				continue
			}
			s.deliver(e)
		}
	}
	s.pollErr = ctx.Err()
}

func (s *session) poll(ctx context.Context) ([]event, error) {
	q := url.Values{"rid": {strconv.FormatInt(time.Now().UnixMilli(), 10)}, "maxev": {"10"}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.sessionURL()+"?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	s.headers(req)
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("janus poll: status %d", resp.StatusCode)
	}
	body = bytes.TrimSpace(body)
	if len(body) > 0 && body[0] == '[' {
		var events []event
		if err := json.Unmarshal(body, &events); err != nil {
			return nil, err
		}
		return events, nil
	}
	var e event
	if err := json.Unmarshal(body, &e); err != nil {
		return nil, err
	}
	if e.Janus == "error" && e.Error != nil {
		// 458: the session is gone (timed out or destroyed).
		return nil, fmt.Errorf("janus poll: error %d: %s", e.Error.Code, e.Error.Reason)
	}
	return []event{e}, nil
}

// handle acts on one event from the server. The error it returns ends the
// session.
func (s *session) handle(ctx context.Context, e event) error {
	switch e.Janus {
	case "event":
		if e.PluginData != nil {
			var r pluginReply
			_ = json.Unmarshal(e.PluginData.Data, &r)
			if r.ErrorCode != 0 {
				// 455 no such mountpoint, 456 mountpoint not live (or not
				// ready) yet.
				if r.ErrorCode == 455 || r.ErrorCode == 456 || r.ErrorCode == 457 {
					return fmt.Errorf("%w: %s", ErrNoStream, r.Error)
				}
				return fmt.Errorf("janus plugin error %d: %s", r.ErrorCode, r.Error)
			}
			if r.Result != nil && (r.Result.Status == "stopped" || r.Result.Status == "stopping") {
				return ErrFinished
			}
		}
		if e.JSEP != nil && e.JSEP.Type == "offer" {
			return s.answer(ctx, e.JSEP.SDP)
		}
	case "trickle":
		// The streaming plugin puts its candidates in the offer.
	case "hangup":
		s.logger.Debug("Janus hangup", "reason", e.Reason)
		return ErrFinished
	case "detached":
		return ErrFinished
	case "timeout":
		return errors.New("janus: session timed out")
	case "media":
		if e.Receiving != nil && !*e.Receiving {
			s.logger.Debug("Janus reports no media", "type", e.Type)
		}
	}
	return nil
}

var h264Fmtp = regexp.MustCompile(`(?m)^a=rtpmap:(\d+) H264/90000\r?$`)

// answer applies the server's offer and sends the answer with the start
// request.
func (s *session) answer(ctx context.Context, offer string) error {
	if s.pc != nil {
		// A renegotiation: the mountpoint's sources changed.
		if err := s.pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: offer}); err != nil {
			return fmt.Errorf("apply janus offer: %w", err)
		}
	} else {
		opts := rtcflv.PeerOptions{H264Fmtp: offerFmtp(offer)}
		if !h264Fmtp.MatchString(offer) && strings.Contains(offer, " VP8/90000") {
			opts.VP8 = true
		}
		pc, err := rtcflv.NewPeerConnection(webrtc.Configuration{}, opts)
		if err != nil {
			return err
		}
		s.pc = pc
		s.media = rtcflv.NewReceiver(pc, s.out, s.finish)
		pc.OnTrack(s.media.OnTrack)
		pc.OnICECandidate(func(c *webrtc.ICECandidate) {
			cand := map[string]any{"completed": true}
			if c != nil {
				ci := c.ToJSON()
				cand = map[string]any{"candidate": ci.Candidate, "sdpMid": ci.SDPMid, "sdpMLineIndex": ci.SDPMLineIndex}
			}
			if err := s.request(ctx, s.handleURL(), map[string]any{"janus": "trickle", "candidate": cand}, nil); err != nil && ctx.Err() == nil {
				s.logger.Debug("Janus trickle failed", "error", err)
			}
		})
		if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: offer}); err != nil {
			return fmt.Errorf("apply janus offer: %w", err)
		}
	}
	answer, err := s.pc.CreateAnswer(nil)
	if err != nil {
		return err
	}
	if err := s.pc.SetLocalDescription(answer); err != nil {
		return err
	}
	if s.answeredAt.IsZero() {
		s.answeredAt = time.Now()
	}
	return s.message(ctx, map[string]any{"request": "start"}, &answer)
}

// offerFmtp is the format line of the offer's first H.264 payload, or a
// baseline one when the offer has none.
func offerFmtp(offer string) string {
	m := h264Fmtp.FindStringSubmatch(offer)
	if m != nil {
		re := regexp.MustCompile(`(?m)^a=fmtp:` + m[1] + ` (.+?)\r?$`)
		if f := re.FindStringSubmatch(offer); f != nil {
			return f[1]
		}
	}
	return "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f"
}

// supervise watches the server's events and the media watchdog until the
// session ends.
func (s *session) supervise(ctx context.Context) {
	keepalive := time.NewTicker(keepaliveInterval)
	defer keepalive.Stop()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			s.finish(ctx.Err())
			return
		case <-s.done:
			s.finish(fmt.Errorf("janus session lost: %w", s.pollErr))
			return
		case e := <-s.inbox:
			if err := s.handle(ctx, e); err != nil {
				if errors.Is(err, ErrFinished) || errors.Is(err, ErrNoStream) {
					s.logger.Info("Janus playback ended", "reason", err)
				}
				s.finish(err)
				return
			}
		case <-keepalive.C:
			if err := s.request(ctx, s.sessionURL(), map[string]any{"janus": "keepalive"}, nil); err != nil && ctx.Err() == nil {
				s.logger.Debug("Janus keepalive failed", "error", err)
			}
		case <-tick.C:
			if idle := s.media.Idle(); idle > mediaTimeout {
				s.finish(fmt.Errorf("janus: no media for %s", idle.Round(time.Second)))
				return
			}
			switch s.pc.ConnectionState() {
			case webrtc.PeerConnectionStateFailed, webrtc.PeerConnectionStateClosed:
				s.finish(fmt.Errorf("janus: media connection %s", s.pc.ConnectionState()))
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
	if s.id != 0 {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = s.request(ctx, s.sessionURL(), map[string]any{"janus": "destroy"}, nil)
		cancel()
	}
	if s.pc != nil {
		_ = s.pc.Close()
	}
}

func randomString(n int) string {
	b := make([]byte, (n+1)/2)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)[:n]
}
