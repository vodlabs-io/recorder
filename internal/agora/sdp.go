package agora

import (
	"errors"
	"fmt"
	"strings"
)

// joinReply is the part of the gateway's join answer the media connection
// needs: its ICE and DTLS parameters, and the SSRCs it pre-subscribed the
// client to.
type joinReply struct {
	UID  uint32 `json:"uid"`
	Ortc struct {
		Cname          string `json:"cname"`
		DtlsParameters struct {
			Fingerprints []struct {
				Algorithm   string `json:"algorithm"`
				Fingerprint string `json:"fingerprint"`
			} `json:"fingerprints"`
		} `json:"dtlsParameters"`
		IceParameters struct {
			Candidates []struct {
				Foundation string `json:"foundation"`
				IP         string `json:"ip"`
				Port       int    `json:"port"`
				Priority   int64  `json:"priority"`
				Protocol   string `json:"protocol"`
				Type       string `json:"type"`
			} `json:"candidates"`
			IcePwd   string `json:"icePwd"`
			IceUfrag string `json:"iceUfrag"`
		} `json:"iceParameters"`
	} `json:"ortc"`
	Attributes struct {
		UserAttributes struct {
			PreSubSsrcs []struct {
				A    uint32 `json:"a"`
				V    uint32 `json:"v"`
				VRtx uint32 `json:"v_rtx"`
			} `json:"preSubSsrcs"`
		} `json:"userAttributes"`
	} `json:"attributes"`
}

// answerSDP renders the gateway's join answer as the SDP answer to the local
// offer: an ICE-lite peer sending H.264 (with RTX) and Opus on the SSRCs it
// pre-subscribed the client to.
func (j joinReply) answerSDP() (string, error) {
	o := j.Ortc
	if o.IceParameters.IceUfrag == "" || len(o.IceParameters.Candidates) == 0 {
		return "", errors.New("join reply lacks ICE parameters")
	}
	if len(o.DtlsParameters.Fingerprints) == 0 {
		return "", errors.New("join reply lacks a DTLS fingerprint")
	}
	if len(j.Attributes.UserAttributes.PreSubSsrcs) == 0 {
		return "", errors.New("join reply pre-subscribed no streams")
	}
	ssrcs := j.Attributes.UserAttributes.PreSubSsrcs[0]
	fp := o.DtlsParameters.Fingerprints[0]
	cname := o.Cname
	if cname == "" {
		cname = "agora"
	}

	var b strings.Builder
	w := func(format string, a ...any) { fmt.Fprintf(&b, format+"\r\n", a...) }
	transport := func() {
		w("c=IN IP4 127.0.0.1")
		w("a=rtcp:9 IN IP4 0.0.0.0")
		w("a=ice-ufrag:%s", o.IceParameters.IceUfrag)
		w("a=ice-pwd:%s", o.IceParameters.IcePwd)
		for _, c := range o.IceParameters.Candidates {
			w("a=candidate:%s 1 %s %d %s %d typ %s", c.Foundation, c.Protocol, c.Priority, c.IP, c.Port, c.Type)
		}
		w("a=fingerprint:%s %s", fp.Algorithm, fp.Fingerprint)
		w("a=setup:active")
	}
	w("v=0")
	w("o=- 0 0 IN IP4 127.0.0.1")
	w("s=AgoraGateway")
	w("t=0 0")
	w("a=group:BUNDLE 0 1")
	w("a=msid-semantic:WMS")
	w("a=ice-lite")

	w("m=video 9 UDP/TLS/RTP/SAVPF %d %d", ptH264, ptH264RTX)
	transport()
	w("a=mid:0")
	w("a=rtpmap:%d H264/90000", ptH264)
	w("a=fmtp:%d level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42001f", ptH264)
	w("a=rtcp-fb:%d nack", ptH264)
	w("a=rtcp-fb:%d nack pli", ptH264)
	w("a=rtcp-fb:%d ccm fir", ptH264)
	w("a=rtpmap:%d rtx/90000", ptH264RTX)
	w("a=fmtp:%d apt=%d", ptH264RTX, ptH264)
	w("a=sendonly")
	w("a=rtcp-mux")
	w("a=rtcp-rsize")
	if ssrcs.VRtx != 0 {
		w("a=ssrc-group:FID %d %d", ssrcs.V, ssrcs.VRtx)
	}
	for _, ssrc := range []uint32{ssrcs.V, ssrcs.VRtx} {
		if ssrc == 0 {
			continue
		}
		w("a=ssrc:%d cname:%s", ssrc, cname)
		w("a=ssrc:%d msid:agora video", ssrc)
	}

	w("m=audio 9 UDP/TLS/RTP/SAVPF %d", ptOpus)
	transport()
	w("a=mid:1")
	w("a=rtpmap:%d opus/48000/2", ptOpus)
	w("a=fmtp:%d minptime=10;useinbandfec=1", ptOpus)
	w("a=sendonly")
	w("a=rtcp-mux")
	w("a=rtcp-rsize")
	w("a=ssrc:%d cname:%s", ssrcs.A, cname)
	w("a=ssrc:%d msid:agora audio", ssrcs.A)
	return b.String(), nil
}
