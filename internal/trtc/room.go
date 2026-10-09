// Package trtc records a broadcast carried over Tencent's TRTC, the way the
// TRTC Web SDK watches one: a signalling WebSocket that joins the room as
// audience, and a WebRTC subscription to the broadcaster's H.264 video and
// Opus audio. Open hands the media back as an FLV stream, so the FLV writers
// record it like any HTTP-FLV origin.
package trtc

import (
	"fmt"
	"net/url"
	"strings"
)

// Scheme is the URL scheme of a stream that is a TRTC room rather than an
// HTTP resource.
const Scheme = "trtc"

// Room names a TRTC room and the credentials to join it as audience.
//
// Its URL form is trtc://<sdk app id>/<room>?userid=<id>&usersig=<sig>,
// optionally with src=<id> naming the broadcaster to receive when several
// publish.
type Room struct {
	SDKAppID string
	// Name is the room id, as a string room id.
	Name    string
	UserID  string
	UserSig string
	// Source is the publishing user to subscribe to; empty means the first
	// one that publishes video.
	Source string
}

// IsURL reports whether u names a TRTC room.
func IsURL(u string) bool {
	return strings.HasPrefix(u, Scheme+"://")
}

// Parse decodes a trtc:// URL.
func Parse(raw string) (Room, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return Room{}, fmt.Errorf("parse trtc url: %w", err)
	}
	if u.Scheme != Scheme {
		return Room{}, fmt.Errorf("not a trtc url: scheme %q", u.Scheme)
	}
	q := u.Query()
	r := Room{
		SDKAppID: u.Host,
		Name:     strings.TrimPrefix(u.Path, "/"),
		UserID:   q.Get("userid"),
		UserSig:  q.Get("usersig"),
		Source:   q.Get("src"),
	}
	if r.SDKAppID == "" || r.Name == "" || r.UserID == "" || r.UserSig == "" {
		return Room{}, fmt.Errorf("trtc url lacks an app id, room, user id or signature")
	}
	return r, nil
}

// URL encodes r as a trtc:// URL.
func (r Room) URL() string {
	q := url.Values{}
	q.Set("userid", r.UserID)
	q.Set("usersig", r.UserSig)
	if r.Source != "" {
		q.Set("src", r.Source)
	}
	u := url.URL{Scheme: Scheme, Host: r.SDKAppID, Path: "/" + r.Name, RawQuery: q.Encode()}
	return u.String()
}
