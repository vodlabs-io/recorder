package trtc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/pion/dtls/v3"
	"github.com/pion/webrtc/v4"
	"github.com/vodlabs-io/recorder/internal/rtcflv"
)

const (
	// mediaTimeout ends a session that has produced no media for this long:
	// a broadcaster who stopped sending without leaving, or a path that died.
	mediaTimeout = 20 * time.Second
	// gatherWait bounds how long the offer waits on local ICE candidates. The
	// server is ICE-lite, so host candidates are enough to start with.
	gatherWait = 2 * time.Second
)

// publisherWaitFor bounds the wait for a broadcaster to show up in the room.
var publisherWaitFor = 15 * time.Second

// h264Fmtp is the H.264 format the Web SDK offers for the main stream.
const h264Fmtp = "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f"

// Audience role and live scene, as the SDK names them on the wire.
const (
	roleAudience = 21
	sceneLive    = 2
)

// ErrRoomEnded reports that the broadcaster left the room.
var ErrRoomEnded = errors.New("trtc: broadcaster left the room")

// Open joins the room u names and returns its broadcast as an FLV stream: AVC
// video and Opus audio on a timeline starting at zero. Reading ends with
// io.EOF when the broadcaster leaves, or with the error that ended the
// session. Close, or the end of ctx, leaves the room.
func Open(ctx context.Context, u string, client *http.Client, logger *slog.Logger) (io.ReadCloser, error) {
	r, err := Parse(u)
	if err != nil {
		return nil, err
	}
	if client == nil || client.Timeout != 0 {
		// The signalling socket outlives any whole-request timeout.
		client = &http.Client{}
	}
	ctx, cancel := context.WithCancel(ctx)
	pr, pw := io.Pipe()
	s := &session{
		room:   r,
		logger: logger.With("trtc_room", r.Name),
		cancel: cancel,
		out:    pw,
	}
	if err := s.connect(ctx, client); err != nil {
		s.teardown()
		return nil, err
	}
	s.media.ResetIdle()
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
	r.s.finish(errors.New("trtc: stream closed"))
	return err
}

// session is one membership of the room.
type session struct {
	room   Room
	logger *slog.Logger
	cancel context.CancelFunc
	sig    *signal
	pc     *webrtc.PeerConnection
	media  *rtcflv.Receiver
	out    *io.PipeWriter

	// publisher is the broadcaster being received, by the room's id for them.
	publisher string

	endOnce sync.Once
}

// roomUser is one member as the user lists describe them.
type roomUser struct {
	UserID    string      `json:"userId"`
	SrcTinyID json.Number `json:"srcTinyId"`
	Role      int         `json:"role"`
	Flag      int         `json:"flag"`
}

// iceServer is the relay the join reply offers.
type iceServer struct {
	URLs       []string `json:"urls"`
	Username   string   `json:"username"`
	Credential string   `json:"credential"`
}

func (s *session) connect(ctx context.Context, client *http.Client) error {
	sig, err := dialSignal(ctx, client, s.room, s.logger)
	if err != nil {
		return err
	}
	s.sig = sig

	joined, err := sig.request(ctx, cmdJoin, s.joinData(), replyJoined)
	if err != nil {
		return fmt.Errorf("join room: %w", err)
	}
	var j struct {
		ICEServer *iceServer `json:"iceServer"`
	}
	_ = json.Unmarshal(joined.Data, &j)

	pub, err := s.findPublisher(ctx)
	if err != nil {
		return err
	}
	s.publisher = pub.SrcTinyID.String()

	cfg := webrtc.Configuration{}
	if j.ICEServer != nil && len(j.ICEServer.URLs) > 0 {
		cfg.ICEServers = []webrtc.ICEServer{{URLs: j.ICEServer.URLs, Username: j.ICEServer.Username, Credential: j.ICEServer.Credential}}
	}
	pc, err := rtcflv.NewPeerConnection(cfg, rtcflv.PeerOptions{
		H264Fmtp: h264Fmtp,
		// The server answers in the Web SDK's layout: audio, the main video,
		// and a second video it fills with whatever else it sends (a small or
		// aux stream), which is never recorded.
		Kinds: []webrtc.RTPCodecType{webrtc.RTPCodecTypeAudio, webrtc.RTPCodecTypeVideo, webrtc.RTPCodecTypeVideo},
		// Offered AES-256-GCM, the server picks it and its packets then fail
		// authentication; the browsers' first choices work.
		SRTPProfiles: []dtls.SRTPProtectionProfile{dtls.SRTP_AEAD_AES_128_GCM, dtls.SRTP_AES128_CM_HMAC_SHA1_80},
	})
	if err != nil {
		return err
	}
	s.pc = pc
	s.media = rtcflv.NewReceiver(pc, s.out, s.finish)
	pc.OnTrack(s.media.OnTrack)

	offer, err := pc.CreateOffer(nil)
	if err != nil {
		return err
	}
	gathered := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(offer); err != nil {
		return err
	}
	select {
	case <-gathered:
	case <-time.After(gatherWait):
	case <-ctx.Done():
		return ctx.Err()
	}

	sub, err := sig.request(ctx, cmdSubscribe, map[string]any{
		"type": "offer", "sdp": pc.LocalDescription().SDP,
		"srcUserId": pub.UserID, "srcTinyId": pub.SrcTinyID,
		"audio": true, "bigVideo": true, "auxVideo": false, "smallVideo": false,
	}, replySubscribed)
	if err != nil {
		return fmt.Errorf("subscribe: %w", err)
	}
	var answer struct {
		SDP string `json:"sdp"`
	}
	if err := json.Unmarshal(sub.Data, &answer); err != nil || answer.SDP == "" {
		return errors.New("trtc: subscribe reply carries no answer")
	}
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: answer.SDP}); err != nil {
		return fmt.Errorf("apply subscribe answer: %w", err)
	}
	s.logger.Info("Joined TRTC room", "broadcaster", pub.UserID)
	return nil
}

func (s *session) joinData() map[string]any {
	return map[string]any{
		"roomId": s.room.Name, "useStringRoomId": true, "latencyLevel": 2,
		"trtcRole": roleAudience, "trtcScene": sceneLive, "sdpSemantics": "unified-plan",
		"version": sdkVersion, "ua": userAgent, "terminalType": 12, "netType": 0,
		"userSig": s.room.UserSig, "receiveMix": true, "isChorus": false,
		"enableNtpAudioFrame": false, "transcription": false, "downUseVp8": false,
	}
}

// findPublisher asks for the room's members until the broadcaster is among
// them: the one the URL names, or else the first anchor.
func (s *session) findPublisher(ctx context.Context) (roomUser, error) {
	deadline := time.Now().Add(publisherWaitFor)
	for {
		reply, err := s.sig.request(ctx, cmdGetUserList, map[string]any{}, replyUserList)
		if err != nil {
			return roomUser{}, fmt.Errorf("list room members: %w", err)
		}
		if u, ok := s.pickPublisher(reply.Data); ok {
			return u, nil
		}
		if time.Now().After(deadline) {
			return roomUser{}, ErrRoomEnded
		}
		select {
		case <-ctx.Done():
			return roomUser{}, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// anchorRole is the role a publishing member has.
const anchorRole = 20

func (s *session) pickPublisher(data json.RawMessage) (roomUser, bool) {
	var l struct {
		UserList []roomUser `json:"userList"`
	}
	_ = json.Unmarshal(data, &l)
	var first *roomUser
	for i, u := range l.UserList {
		if u.SrcTinyID == "" {
			continue
		}
		if s.room.Source != "" && u.UserID == s.room.Source {
			return u, true
		}
		if first == nil && (u.Role == anchorRole || u.Role == 0) {
			first = &l.UserList[i]
		}
	}
	if s.room.Source == "" && first != nil {
		return *first, true
	}
	return roomUser{}, false
}

// supervise watches the server's pushes and the media watchdog until the
// session ends.
func (s *session) supervise(ctx context.Context) {
	go s.sig.keepalive(ctx)
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			s.finish(ctx.Err())
			return
		case <-s.sig.done:
			s.finish(fmt.Errorf("trtc signalling closed: %w", s.sig.err))
			return
		case m := <-s.sig.pushes:
			if m.cmd() == pushUserList && !s.publisherPresent(m.Data) {
				s.logger.Info("Broadcaster left the room", "broadcaster", s.publisher)
				s.finish(ErrRoomEnded)
				return
			}
		case <-tick.C:
			if idle := s.media.Idle(); idle > mediaTimeout {
				s.finish(fmt.Errorf("trtc: no media for %s", idle.Round(time.Second)))
				return
			}
			switch s.pc.ConnectionState() {
			case webrtc.PeerConnectionStateFailed, webrtc.PeerConnectionStateClosed:
				s.finish(fmt.Errorf("trtc: media connection %s", s.pc.ConnectionState()))
				return
			}
		}
	}
}

// publisherPresent reports whether a pushed member list still holds the
// broadcaster being received.
func (s *session) publisherPresent(data json.RawMessage) bool {
	var l struct {
		UserList []roomUser `json:"userList"`
	}
	if json.Unmarshal(data, &l) != nil {
		return true
	}
	for _, u := range l.UserList {
		if u.SrcTinyID.String() == s.publisher {
			return true
		}
	}
	return false
}

// finish ends the session once: the stream's reader sees err (io.EOF for a
// broadcaster who left), and the room is left.
func (s *session) finish(err error) {
	s.endOnce.Do(func() {
		if errors.Is(err, ErrRoomEnded) {
			err = io.EOF
		}
		s.out.CloseWithError(err)
		s.teardown()
	})
}

func (s *session) teardown() {
	s.cancel()
	if s.sig != nil {
		s.sig.close()
	}
	if s.pc != nil {
		_ = s.pc.Close()
	}
}
