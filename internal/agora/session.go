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

	"github.com/pion/interceptor"
	"github.com/pion/rtcp"
	"github.com/pion/rtp/codecs"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media/samplebuilder"
)

// Payload types offered for the two codecs recorded, matching the Web SDK's.
const (
	ptH264    = 102
	ptH264RTX = 103
	ptOpus    = 111
)

const (
	// mediaTimeout ends a session that has produced no media for this long:
	// a broadcaster who stopped sending without leaving, or a path that died.
	mediaTimeout = 20 * time.Second
	// pliInterval spaces keyframe requests while the video waits for one.
	pliInterval = time.Second
)

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
		clock:  newMediaClock(),
		out:    pw,
		mux:    newFLVMuxer(pw, 2),
	}
	if err := s.connect(ctx, client); err != nil {
		s.teardown()
		return nil, err
	}
	s.lastMedia.Store(time.Now().UnixNano())
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
	clock  *mediaClock

	// publisher is the uid whose streams are being received.
	publisher atomic.Uint32

	muxMu     sync.Mutex
	mux       *flvMuxer
	out       *io.PipeWriter
	origin    time.Time // wall time of FLV timestamp zero; zero until the first keyframe
	lastMedia atomic.Int64

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

	pc, offer, err := newPeer()
	if err != nil {
		return err
	}
	s.pc = pc

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
	answer, err := joined.answerSDP()
	if err != nil {
		return err
	}
	s.pc.OnTrack(s.onTrack)
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
	opus := codec(ptOpus, "opus", 48000, map[string]any{"rtcpFeedbacks": []any{},
		"fmtp": map[string]any{"parameters": map[string]string{"minptime": "10", "useinbandfec": "1"}}})
	opus["rtpMap"].(map[string]any)["encodingParameters"] = 2
	none := map[string]any{"audioCodecs": []any{}, "audioExtensions": []any{}, "videoCodecs": []any{}, "videoExtensions": []any{}}

	return map[string]any{
		"p2p_id": 1, "session_id": sid, "app_id": s.ch.AppID, "channel_key": s.ch.Token,
		"channel_name": s.ch.Name, "sdk_version": sdkVersion, "browser": userAgent,
		"process_id": "process-" + randomHex(16), "mode": "live", "codec": "h264", "role": "audience",
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
					"videoCodecs": []any{h264, rtx}, "videoExtensions": []any{}},
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
			if idle := time.Since(time.Unix(0, s.lastMedia.Load())); idle > mediaTimeout {
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

// onTrack reads one received track into the muxer.
func (s *session) onTrack(t *webrtc.TrackRemote, r *webrtc.RTPReceiver) {
	go s.readRTCP(r, t.Codec().ClockRate)
	switch t.Kind() {
	case webrtc.RTPCodecTypeVideo:
		s.readVideo(t)
	case webrtc.RTPCodecTypeAudio:
		s.readAudio(t)
	}
}

func (s *session) readRTCP(r *webrtc.RTPReceiver, rate uint32) {
	for {
		pkts, _, err := r.ReadRTCP()
		if err != nil {
			return
		}
		for _, p := range pkts {
			if sr, ok := p.(*rtcp.SenderReport); ok {
				s.clock.senderReport(sr.SSRC, rate, sr.NTPTime, sr.RTPTime)
			}
		}
	}
}

type pendingSample struct {
	rtpTS uint32
	data  []byte
}

func (s *session) readVideo(t *webrtc.TrackRemote) {
	ssrc := uint32(t.SSRC())
	sb := samplebuilder.New(512, &codecs.H264Packet{IsAVC: true}, 90000)
	var pending []pendingSample
	needKey := true
	var lastTS uint32
	var haveLast bool
	var lastPLI time.Time
	requestKey := func() {
		if time.Since(lastPLI) < pliInterval {
			return
		}
		lastPLI = time.Now()
		_ = s.pc.WriteRTCP([]rtcp.Packet{&rtcp.PictureLossIndication{MediaSSRC: ssrc}})
	}
	requestKey()
	for {
		pkt, _, err := t.ReadRTP()
		if err != nil {
			return
		}
		sb.Push(pkt)
		for smp := sb.Pop(); smp != nil; smp = sb.Pop() {
			// A frame repeated under the same timestamp adds nothing.
			if haveLast && smp.PacketTimestamp == lastTS {
				continue
			}
			lastTS, haveLast = smp.PacketTimestamp, true
			if smp.PrevDroppedPackets > 0 {
				// A frame was lost: what follows may reference it.
				needKey = true
			}
			if needKey {
				if !hasIDR(smp.Data) {
					requestKey()
					continue
				}
				needKey = false
			}
			pending = append(pending, pendingSample{rtpTS: smp.PacketTimestamp, data: smp.Data})
			pending = s.drain(ssrc, 90000, pending, true)
		}
	}
}

func (s *session) readAudio(t *webrtc.TrackRemote) {
	ssrc := uint32(t.SSRC())
	sb := samplebuilder.New(64, &codecs.OpusPacket{}, 48000)
	var pending []pendingSample
	for {
		pkt, _, err := t.ReadRTP()
		if err != nil {
			return
		}
		sb.Push(pkt)
		for smp := sb.Pop(); smp != nil; smp = sb.Pop() {
			pending = append(pending, pendingSample{rtpTS: smp.PacketTimestamp, data: smp.Data})
			pending = s.drain(ssrc, 48000, pending, false)
		}
	}
}

// drain writes the samples whose wall time is known, in order, and returns
// the ones still waiting for a Sender Report.
func (s *session) drain(ssrc, rate uint32, pending []pendingSample, video bool) []pendingSample {
	now := time.Now()
	for len(pending) > 0 {
		at, ok := s.clock.wall(ssrc, rate, pending[0].rtpTS, now)
		if !ok {
			return pending
		}
		if err := s.write(at, pending[0].data, video); err != nil {
			s.finish(err)
			return nil
		}
		pending = pending[1:]
	}
	return pending
}

func (s *session) write(at time.Time, data []byte, video bool) error {
	s.muxMu.Lock()
	defer s.muxMu.Unlock()
	if s.origin.IsZero() {
		// The recording starts at the first keyframe; audio before it has
		// no picture to accompany.
		if !video {
			return nil
		}
		s.origin = at
	}
	ts := at.Sub(s.origin).Milliseconds()
	if ts < 0 {
		return nil
	}
	s.lastMedia.Store(time.Now().UnixNano())
	if video {
		return s.mux.writeVideo(ts, data)
	}
	return s.mux.writeAudio(ts, data)
}

// localParams are the parts of the local offer the gateway is told about.
type localParams struct {
	ufrag, pwd, fingerprint string
}

// newPeer creates a peer connection with one receive-only video and audio
// transceiver, and returns it with its offer applied.
func newPeer() (*webrtc.PeerConnection, localParams, error) {
	me := &webrtc.MediaEngine{}
	fb := []webrtc.RTCPFeedback{{Type: "nack"}, {Type: "nack", Parameter: "pli"}, {Type: "ccm", Parameter: "fir"}}
	codecsToRegister := []struct {
		p webrtc.RTPCodecParameters
		k webrtc.RTPCodecType
	}{
		{webrtc.RTPCodecParameters{RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264, ClockRate: 90000,
			SDPFmtpLine: "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42001f", RTCPFeedback: fb}, PayloadType: ptH264}, webrtc.RTPCodecTypeVideo},
		{webrtc.RTPCodecParameters{RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeRTX, ClockRate: 90000,
			SDPFmtpLine: fmt.Sprintf("apt=%d", ptH264)}, PayloadType: ptH264RTX}, webrtc.RTPCodecTypeVideo},
		{webrtc.RTPCodecParameters{RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2,
			SDPFmtpLine: "minptime=10;useinbandfec=1"}, PayloadType: ptOpus}, webrtc.RTPCodecTypeAudio},
	}
	for _, c := range codecsToRegister {
		if err := me.RegisterCodec(c.p, c.k); err != nil {
			return nil, localParams{}, err
		}
	}
	reg := &interceptor.Registry{}
	if err := webrtc.RegisterDefaultInterceptors(me, reg); err != nil {
		return nil, localParams{}, err
	}
	api := webrtc.NewAPI(webrtc.WithMediaEngine(me), webrtc.WithInterceptorRegistry(reg))
	pc, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		return nil, localParams{}, err
	}
	for _, k := range []webrtc.RTPCodecType{webrtc.RTPCodecTypeVideo, webrtc.RTPCodecTypeAudio} {
		if _, err := pc.AddTransceiverFromKind(k, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly}); err != nil {
			pc.Close()
			return nil, localParams{}, err
		}
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
