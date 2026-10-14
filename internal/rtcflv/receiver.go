// Package rtcflv records a broadcast received over WebRTC as an FLV stream: a
// broadcaster's H.264 (or VP8) video and Opus audio, stamped on the sender's
// clock, in the layout the FLV writers already record from HTTP-FLV origins. The RTC
// readers (Agora, TRTC) each do their own signalling and hand the peer
// connection's tracks to a Receiver.
package rtcflv

import (
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/dtls/v3"
	"github.com/pion/interceptor"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media/samplebuilder"
)

// Payload types offered for the codecs recorded, matching the browsers'.
const (
	PTH264    = 102
	PTH264RTX = 103
	PTVP8     = 96
	PTVP8RTX  = 97
	PTOpus    = 111
)

// pliInterval spaces keyframe requests while the video waits for one.
const pliInterval = time.Second

// vp8KeyframeEvery is the longest a VP8 track goes without a keyframe before
// one is requested. A browser broadcaster's VP8 encoder makes keyframes only
// on demand, tens of seconds apart, and recordings cut segments at keyframes.
const vp8KeyframeEvery = 4 * time.Second

// PeerOptions shapes a receiving peer connection to what one service expects.
type PeerOptions struct {
	// H264Fmtp is the H.264 format line to offer.
	H264Fmtp string
	// Kinds are the receive-only transceivers, in the order the offer lists
	// them.
	Kinds []webrtc.RTPCodecType
	// SRTPProfiles, when set, are the only SRTP protection profiles offered.
	SRTPProfiles []dtls.SRTPProtectionProfile
	// VP8 offers VP8 video in place of H.264.
	VP8 bool
}

// NewPeerConnection creates a peer connection that can receive H.264 (or VP8)
// and Opus.
func NewPeerConnection(cfg webrtc.Configuration, opts PeerOptions) (*webrtc.PeerConnection, error) {
	me := &webrtc.MediaEngine{}
	fb := []webrtc.RTCPFeedback{{Type: "nack"}, {Type: "nack", Parameter: "pli"}, {Type: "ccm", Parameter: "fir"}}
	video := webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264, ClockRate: 90000, SDPFmtpLine: opts.H264Fmtp, RTCPFeedback: fb}
	videoPT, rtxPT := webrtc.PayloadType(PTH264), webrtc.PayloadType(PTH264RTX)
	if opts.VP8 {
		video = webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8, ClockRate: 90000, RTCPFeedback: fb}
		videoPT, rtxPT = PTVP8, PTVP8RTX
	}
	codecsToRegister := []struct {
		p webrtc.RTPCodecParameters
		k webrtc.RTPCodecType
	}{
		{webrtc.RTPCodecParameters{RTPCodecCapability: video, PayloadType: videoPT}, webrtc.RTPCodecTypeVideo},
		{webrtc.RTPCodecParameters{RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeRTX, ClockRate: 90000,
			SDPFmtpLine: fmt.Sprintf("apt=%d", videoPT)}, PayloadType: rtxPT}, webrtc.RTPCodecTypeVideo},
		{webrtc.RTPCodecParameters{RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2,
			SDPFmtpLine: "minptime=10;useinbandfec=1"}, PayloadType: PTOpus}, webrtc.RTPCodecTypeAudio},
	}
	for _, c := range codecsToRegister {
		if err := me.RegisterCodec(c.p, c.k); err != nil {
			return nil, err
		}
	}
	reg := &interceptor.Registry{}
	if err := webrtc.RegisterDefaultInterceptors(me, reg); err != nil {
		return nil, err
	}
	se := webrtc.SettingEngine{}
	if len(opts.SRTPProfiles) > 0 {
		se.SetSRTPProtectionProfiles(opts.SRTPProfiles...)
	}
	api := webrtc.NewAPI(webrtc.WithMediaEngine(me), webrtc.WithInterceptorRegistry(reg), webrtc.WithSettingEngine(se))
	pc, err := api.NewPeerConnection(cfg)
	if err != nil {
		return nil, err
	}
	for _, k := range opts.Kinds {
		if _, err := pc.AddTransceiverFromKind(k, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly}); err != nil {
			pc.Close()
			return nil, err
		}
	}
	return pc, nil
}

// Receiver turns a peer connection's H.264 or VP8 and Opus tracks into FLV. Writes go
// to w; the first error writing is handed to fail, once per write that fails.
type Receiver struct {
	pc    *webrtc.PeerConnection
	clock *mediaClock
	fail  func(error)

	muxMu     sync.Mutex
	mux       *flvMuxer
	origin    time.Time // wall time of FLV timestamp zero; zero until the first keyframe
	lastMedia atomic.Int64
}

// NewReceiver returns a Receiver writing pc's broadcast to w. Hand it each
// track with OnTrack.
func NewReceiver(pc *webrtc.PeerConnection, w io.Writer, fail func(error)) *Receiver {
	r := &Receiver{pc: pc, clock: newMediaClock(), fail: fail, mux: newFLVMuxer(w, 2)}
	r.lastMedia.Store(time.Now().UnixNano())
	return r
}

// Idle is how long since media was last written (or since the Receiver was
// made, before any was).
func (r *Receiver) Idle() time.Duration {
	return time.Since(time.Unix(0, r.lastMedia.Load()))
}

// ResetIdle restarts the idle clock, for a session whose setup took a while.
func (r *Receiver) ResetIdle() {
	r.lastMedia.Store(time.Now().UnixNano())
}

// OnTrack reads one received track into the FLV stream until it ends. It is
// the peer connection's OnTrack handler.
func (r *Receiver) OnTrack(t *webrtc.TrackRemote, rcv *webrtc.RTPReceiver) {
	go r.readRTCP(rcv, t.Codec().ClockRate)
	switch t.Kind() {
	case webrtc.RTPCodecTypeVideo:
		r.readVideo(t)
	case webrtc.RTPCodecTypeAudio:
		r.readAudio(t)
	}
}

func (r *Receiver) readRTCP(rcv *webrtc.RTPReceiver, rate uint32) {
	for {
		pkts, _, err := rcv.ReadRTCP()
		if err != nil {
			return
		}
		for _, p := range pkts {
			if sr, ok := p.(*rtcp.SenderReport); ok {
				r.clock.senderReport(sr.SSRC, rate, sr.NTPTime, sr.RTPTime)
			}
		}
	}
}

type pendingSample struct {
	rtpTS uint32
	data  []byte
	vp8   bool
}

func (r *Receiver) readVideo(t *webrtc.TrackRemote) {
	ssrc := uint32(t.SSRC())
	vp8 := strings.EqualFold(t.Codec().MimeType, webrtc.MimeTypeVP8)
	var depacketizer rtp.Depacketizer = &codecs.H264Packet{IsAVC: true}
	isKey := hasIDR
	if vp8 {
		depacketizer, isKey = &codecs.VP8Packet{}, isVP8Keyframe
	}
	sb := samplebuilder.New(512, depacketizer, 90000)
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
		_ = r.pc.WriteRTCP([]rtcp.Packet{&rtcp.PictureLossIndication{MediaSSRC: ssrc}})
	}
	requestKey()
	lastKey := time.Now()
	for {
		pkt, _, err := t.ReadRTP()
		if err != nil {
			return
		}
		sb.Push(pkt)
		if vp8 && time.Since(lastKey) >= vp8KeyframeEvery {
			requestKey()
		}
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
				if !isKey(smp.Data) {
					requestKey()
					continue
				}
				needKey = false
			}
			if vp8 && isKey(smp.Data) {
				lastKey = time.Now()
			}
			pending = append(pending, pendingSample{rtpTS: smp.PacketTimestamp, data: smp.Data, vp8: vp8})
			pending = r.drain(ssrc, 90000, pending, true)
		}
	}
}

func (r *Receiver) readAudio(t *webrtc.TrackRemote) {
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
			pending = r.drain(ssrc, 48000, pending, false)
		}
	}
}

// drain writes the samples whose wall time is known, in order, and returns
// the ones still waiting for a Sender Report.
func (r *Receiver) drain(ssrc, rate uint32, pending []pendingSample, video bool) []pendingSample {
	now := time.Now()
	for len(pending) > 0 {
		at, ok := r.clock.wall(ssrc, rate, pending[0].rtpTS, now)
		if !ok {
			return pending
		}
		if err := r.write(at, pending[0], video); err != nil {
			r.fail(err)
			return nil
		}
		pending = pending[1:]
	}
	return pending
}

func (r *Receiver) write(at time.Time, smp pendingSample, video bool) error {
	r.muxMu.Lock()
	defer r.muxMu.Unlock()
	if r.origin.IsZero() {
		// The recording starts at the first keyframe; audio before it has
		// no picture to accompany.
		if !video {
			return nil
		}
		r.origin = at
	}
	ts := at.Sub(r.origin).Milliseconds()
	if ts < 0 {
		return nil
	}
	r.lastMedia.Store(time.Now().UnixNano())
	switch {
	case video && smp.vp8:
		return r.mux.writeVP8(ts, smp.data)
	case video:
		return r.mux.writeVideo(ts, smp.data)
	}
	return r.mux.writeAudio(ts, smp.data)
}
