package agora

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestChannelURLRoundTrip(t *testing.T) {
	c := Channel{AppID: "app", Name: "chan", Token: "007e+/=", UID: 42, Mint: "superlive", Room: "152", User: "99"}
	got, err := Parse(c.URL())
	if err != nil {
		t.Fatal(err)
	}
	if got != c {
		t.Fatalf("round trip: got %+v, want %+v", got, c)
	}
	if !IsURL(c.URL()) || IsURL("https://example.com/live.flv") {
		t.Fatal("IsURL misclassifies")
	}
	for _, bad := range []string{"agora:///chan", "agora://app/", "https://app/chan", "agora://app/chan?uid=x"} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("Parse(%q) accepted", bad)
		}
	}
}

const joinReplyJSON = `{"attributes":{"userAttributes":{"preSubSsrcs":[{"a":20000,"a_rtx":20001,"v":40000,"v_rtx":40001}]}},
"ortc":{"cname":"o/i14u","dtlsParameters":{"fingerprints":[{"algorithm":"sha-256","fingerprint":"3D:8E"}],"role":"client"},
"iceParameters":{"candidates":[{"foundation":"udpcandidate","ip":"107.151.220.110","port":4702,"priority":2103266323,"protocol":"udp","type":"host"}],
"icePwd":"pwd","iceUfrag":"ufrag"}},"uid":11353}`

func TestAnswerSDP(t *testing.T) {
	var j joinReply
	if err := json.Unmarshal([]byte(joinReplyJSON), &j); err != nil {
		t.Fatal(err)
	}
	sdp, err := j.answerSDP()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"a=ice-lite\r\n",
		"a=candidate:udpcandidate 1 udp 2103266323 107.151.220.110 4702 typ host\r\n",
		"a=fingerprint:sha-256 3D:8E\r\n",
		"a=setup:active\r\n",
		"a=ssrc-group:FID 40000 40001\r\n",
		"a=ssrc:40000 cname:o/i14u\r\n",
		"a=ssrc:20000 cname:o/i14u\r\n",
		"a=rtpmap:102 H264/90000\r\n",
		"a=rtpmap:111 opus/48000/2\r\n",
	} {
		if !strings.Contains(sdp, want) {
			t.Errorf("answer lacks %q", want)
		}
	}

	var empty joinReply
	if _, err := empty.answerSDP(); err == nil {
		t.Error("an answer was built from a reply with no ICE parameters")
	}
}

func TestGatewayErrorNamesCode(t *testing.T) {
	e := &gatewayError{Kind: "removed from channel", Code: 14}
	if !strings.Contains(e.Error(), "uid banned") {
		t.Fatalf("%q does not name the code", e.Error())
	}
}
