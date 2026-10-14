package agora

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/webrtc/v4"
	"github.com/vodlabs-io/recorder/internal/rtcflv"
)

// Payload types offered for the codecs recorded, matching the Web SDK's.
const (
	ptH264    = rtcflv.PTH264
	ptH264RTX = rtcflv.PTH264RTX
	ptVP8     = rtcflv.PTVP8
	ptVP8RTX  = rtcflv.PTVP8RTX
	ptOpus    = rtcflv.PTOpus
)

// mediaTimeout ends a session that has produced no media for this long: a
// broadcaster who stopped sending without leaving, or a path that died.
const mediaTimeout = 20 * time.Second

// Open joins the channel u names and returns its broadcast as an FLV stream:
// AVC video and Opus audio on a timeline starting at zero. Reading ends with
// io.EOF when the broadcaster leaves, or with the error that ended the
// session. Close, or the end of ctx, leaves the channel.
func Open(ctx context.Context, u string, client *http.Client, logger *slog.Logger) (io.ReadCloser, error) {
	ch, err := Parse(u)
	if err != nil {
		return nil, err
	}
	if client == nil || client.Timeout != 0 {
		// The signalling socket outlives any whole-request timeout.
		client = &http.Client{}
	}
	if ch.Mint != "" {
		minted, err := mint(ctx, client, ch)
		if err != nil {
			logger.Warn("Agora token mint failed, joining with the shared token",
				"mint", ch.Mint, "error", err)
		} else {
			ch = minted
		}
	}

	ctx, cancel := context.WithCancel(ctx)
	pr, pw := io.Pipe()
	s := &session{
		ch:     ch,
		logger: logger.With("agora_channel", ch.Name),
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
	r.s.finish(errors.New("agora: stream closed"))
	return err
}

// session is one membership of the channel.
type session struct {
	ch     Channel
	logger *slog.Logger
	cancel context.CancelFunc
	sig    *signal
	pc     *webrtc.PeerConnection
	media  *rtcflv.Receiver
	out    *io.PipeWriter

	// publisher is the uid whose streams are being received.
	publisher atomic.Uint32

	endOnce sync.Once
}

func (s *session) connect(ctx context.Context, client *http.Client) error {
	sid := strings.ToUpper(randomHex(16))
	grant, err := lookupGateway(ctx, client, s.ch, sid)
	if err != nil {
		return err
	}
	if s.ch.UID == 0 {
		s.ch.UID = grant.UID
	}

	pc, offer, err := newPeer(s.ch.Codec == CodecVP8)
	if err != nil {
		return err
	}
	s.pc = pc
	s.media = rtcflv.NewReceiver(pc, s.out, s.finish)

	var dialErr error
	for _, e := range grant.Edges {
		if s.sig, dialErr = dialGateway(ctx, client, e, s.logger); dialErr == nil {
			break
		}
	}
	if s.sig == nil {
		return dialErr
	}

	reply, err := s.sig.request(ctx, "join_v3", s.joinMessage(sid, grant, offer))
	if err != nil {
		return fmt.Errorf("join channel: %w", err)
	}
	var joined joinReply
	if err := json.Unmarshal(reply, &joined); err != nil {
		return fmt.Errorf("decode join reply: %w", err)
	}
	answer, err := joined.answerSDP(s.ch.Codec == CodecVP8)
	if err != nil {
		return err
	}
	s.pc.OnTrack(s.media.OnTrack)
	if err := s.pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: answer}); err != nil {
		return fmt.Errorf("apply gateway answer: %w", err)
	}
	s.logger.Info("Joined Agora channel", "uid", joined.UID, "gateway", grant.Edges[0].IP)
	return nil
}

func (s *session) joinMessage(sid string, grant apGrant, offer localParams) map[string]any {
	codec := func(pt int, name string, rate int, extra map[string]any) map[string]any {
		c := map[string]any{"payloadType": pt, "rtpMap": map[string]any{"encodingName": name, "clockRate": rate}}
		for k, v := range extra {
			c[k] = v
		}
		return c
	}
	fb := []any{map[string]any{"type": "nack"}, map[string]any{"type": "nack", "parameter": "pli"},
		map[string]any{"type": "ccm", "parameter": "fir"}}
	h264 := codec(ptH264, "H264", 90000, map[string]any{"rtcpFeedbacks": fb, "fmtp": map[string]any{"parameters": map[string]string{
		"level-asymmetry-allowed": "1", "packetization-mode": "1", "profile-level-id": "42001f"}}})
	rtx := codec(ptH264RTX, "rtx", 90000, map[string]any{"rtcpFeedbacks": []any{},
		"fmtp": map[string]any{"parameters": map[string]string{"apt": fmt.Sprint(ptH264)}}})
	video, codecName := h264, "h264"
	if s.ch.Codec == CodecVP8 {
		video, codecName = codec(ptVP8, "VP8", 90000, map[string]any{"rtcpFeedbacks": fb}), CodecVP8
		rtx = codec(ptVP8RTX, "rtx", 90000, map[string]any{"rtcpFeedbacks": []any{},
			"fmtp": map[string]any{"parameters": map[string]string{"apt": fmt.Sprint(ptVP8)}}})
	}
	opus := codec(ptOpus, "opus", 48000, map[string]any{"rtcpFeedbacks": []any{},
		"fmtp": map[string]any{"parameters": map[string]string{"minptime": "10", "useinbandfec": "1"}}})
	opus["rtpMap"].(map[string]any)["encodingParameters"] = 2
	none := map[string]any{"audioCodecs": []any{}, "audioExtensions": []any{}, "videoCodecs": []any{}, "videoExtensions": []any{}}

	return map[string]any{
		"p2p_id": 1, "session_id": sid, "app_id": s.ch.AppID, "channel_key": s.ch.Token,
		"channel_name": s.ch.Name, "sdk_version": sdkVersion, "browser": userAgent,
		"process_id": "process-" + randomHex(16), "mode": "live", "codec": codecName, "role": "audience",
		"has_changed_gateway": false, "ap_response": grant.joinResponse(), "extend": "",
		"details": map[string]any{}, "features": map[string]any{"rejoin": true},
		"attributes": map[string]any{"userAttributes": map[string]any{
			"enableAudioMetadata": false, "enableAudioPts": false, "enableNetworkQualityProbe": false,
			"enablePublishedUserList": true, "enableUserList": false, "maxSubscription": 50,
			"enableUserLicenseCheck": true, "enableRTX": true, "enableNTPReport": true,
			"enableInstantVideo": true, "enableDataStream2": false, "enableAutFeedback": true,
			"enableUserAutoRebalanceCheck": true, "enableXR": true, "enableLossbasedBwe": true,
			"enableAutCC": true, "enablePreallocPC": true, "preSubNum": 1, "enablePubTWCC": false,
			"enableSubTWCC": true, "enablePubRTX": true, "enableSubRTX": true, "enableVosFallback": false,
			"enableQualityFallback": false, "enableDuplicatedIframe": false, "enableDualStreamFlag": false,
		}},
		"join_ts": time.Now().UnixMilli(),
		"ortc": map[string]any{
			"iceParameters": map[string]any{"iceUfrag": offer.ufrag, "icePwd": offer.pwd},
			"dtlsParameters": map[string]any{"fingerprints": []any{
				map[string]any{"hashFunction": "sha-256", "fingerprint": offer.fingerprint}}},
			"rtpCapabilities": map[string]any{
				"send": none, "recv": none,
				"sendrecv": map[string]any{"audioCodecs": []any{opus}, "audioExtensions": []any{},
					"videoCodecs": []any{video, rtx}, "videoExtensions": []any{}},
			},
			"version": "2",
		},
	}
}

// supervise watches the gateway's notifications and the media watchdog until
// the session ends.
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
			s.finish(fmt.Errorf("agora gateway closed: %w", s.sig.err))
			return
		case n := <-s.sig.events:
			if err := s.onNotification(n); err != nil {
				s.finish(err)
				return
			}
		case <-tick.C:
			if idle := s.media.Idle(); idle > mediaTimeout {
				s.finish(fmt.Errorf("agora: no media for %s", idle.Round(time.Second)))
				return
			}
			switch s.pc.ConnectionState() {
			case webrtc.PeerConnectionStateFailed, webrtc.PeerConnectionStateClosed:
				s.finish(fmt.Errorf("agora: media connection %s", s.pc.ConnectionState()))
				return
			}
		}
	}
}

func (s *session) onNotification(n notification) error {
	var m struct {
		UID       uint32 `json:"uid"`
		ErrorCode int    `json:"error_code"`
		Reason    int    `json:"reason"`
	}
	_ = json.Unmarshal(n.Message, &m)
	switch n.Type {
	case "on_add_video_stream", "on_add_audio_stream":
		if s.publisher.CompareAndSwap(0, m.UID) {
			s.logger.Debug("Receiving broadcaster", "uid", m.UID)
		}
	case "on_user_offline", "on_remove_stream":
		if p := s.publisher.Load(); p != 0 && m.UID == p {
			s.logger.Info("Broadcaster left the channel", "uid", m.UID, "event", n.Type, "reason", m.Reason)
			return ErrChannelEnded
		}
	case "on_user_banned":
		return &gatewayError{Kind: "removed from channel", Code: m.ErrorCode}
	case "on_token_privilege_did_expire":
		return &gatewayError{Kind: "token expired", Code: 2}
	case "on_p2p_lost":
		return errors.New("agora: gateway lost the media connection")
	}
	return nil
}

// finish ends the session once: the stream's reader sees err (io.EOF for a
// broadcaster who left), and the channel is left.
func (s *session) finish(err error) {
	s.endOnce.Do(func() {
		if errors.Is(err, ErrChannelEnded) {
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

// localParams are the parts of the local offer the gateway is told about.
type localParams struct {
	ufrag, pwd, fingerprint string
}

// newPeer creates a peer connection with one receive-only video (H.264, or
// VP8 when vp8 is set) and audio transceiver, and returns it with its offer
// applied.
func newPeer(vp8 bool) (*webrtc.PeerConnection, localParams, error) {
	pc, err := rtcflv.NewPeerConnection(webrtc.Configuration{}, rtcflv.PeerOptions{
		H264Fmtp: "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42001f",
		Kinds:    []webrtc.RTPCodecType{webrtc.RTPCodecTypeVideo, webrtc.RTPCodecTypeAudio},
		VP8:      vp8,
	})
	if err != nil {
		return nil, localParams{}, err
	}
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		pc.Close()
		return nil, localParams{}, err
	}
	if err := pc.SetLocalDescription(offer); err != nil {
		pc.Close()
		return nil, localParams{}, err
	}
	var lp localParams
	for _, l := range strings.Split(offer.SDP, "\r\n") {
		switch {
		case strings.HasPrefix(l, "a=ice-ufrag:") && lp.ufrag == "":
			lp.ufrag = strings.TrimPrefix(l, "a=ice-ufrag:")
		case strings.HasPrefix(l, "a=ice-pwd:") && lp.pwd == "":
			lp.pwd = strings.TrimPrefix(l, "a=ice-pwd:")
		case strings.HasPrefix(l, "a=fingerprint:sha-256 ") && lp.fingerprint == "":
			lp.fingerprint = strings.TrimPrefix(l, "a=fingerprint:sha-256 ")
		}
	}
	if lp.ufrag == "" || lp.pwd == "" || lp.fingerprint == "" {
		pc.Close()
		return nil, localParams{}, errors.New("local offer lacks ICE or DTLS parameters")
	}
	return pc, lp, nil
}
