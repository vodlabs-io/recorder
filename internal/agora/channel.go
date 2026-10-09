// Package agora records a broadcast carried over Agora's real-time network, the
// way the Agora Web SDK watches one: an access-point lookup, a signalling
// WebSocket to an edge gateway, and a WebRTC peer connection that receives the
// broadcaster's H.264 video and Opus audio. Open hands the media back as an FLV
// stream, so the FLV writers record it like any HTTP-FLV origin.
package agora

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// Scheme is the URL scheme of a stream that is an Agora channel rather than
// an HTTP resource.
const Scheme = "agora"

// Channel names an Agora channel and the credentials to join it as audience.
//
// Its URL form is agora://<app id>/<channel>?token=<t>&uid=<n>, optionally with
// mint=<platform>&room=<id>&user=<id> when the platform binds a token to one
// viewer.
type Channel struct {
	AppID string
	Name  string
	Token string
	UID   uint32
	// Mint names a platform whose join tokens belong to one viewer: two
	// clients joining with the same token evict each other, so a recorder
	// mints its own rather than sharing the one the check returned. Empty
	// means Token and UID are used as given.
	Mint string
	// Room is the platform's id for the broadcast, which the mint needs, and
	// User the broadcaster's, for finding a broadcast that replaced it.
	Room string
	User string
}

// IsURL reports whether u names an Agora channel.
func IsURL(u string) bool {
	return strings.HasPrefix(u, Scheme+"://")
}

// Parse decodes an agora:// URL.
func Parse(raw string) (Channel, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return Channel{}, fmt.Errorf("parse agora url: %w", err)
	}
	if u.Scheme != Scheme {
		return Channel{}, fmt.Errorf("not an agora url: scheme %q", u.Scheme)
	}
	c := Channel{
		AppID: u.Host,
		Name:  strings.TrimPrefix(u.Path, "/"),
		Token: u.Query().Get("token"),
		Mint:  u.Query().Get("mint"),
		Room:  u.Query().Get("room"),
		User:  u.Query().Get("user"),
	}
	if uid := u.Query().Get("uid"); uid != "" {
		n, err := strconv.ParseUint(uid, 10, 32)
		if err != nil {
			return Channel{}, fmt.Errorf("agora url: uid %q: %w", uid, err)
		}
		c.UID = uint32(n)
	}
	if c.AppID == "" || c.Name == "" {
		return Channel{}, fmt.Errorf("agora url names no app id or channel")
	}
	return c, nil
}

// URL encodes c as an agora:// URL.
func (c Channel) URL() string {
	q := url.Values{}
	if c.Token != "" {
		q.Set("token", c.Token)
	}
	if c.UID != 0 {
		q.Set("uid", strconv.FormatUint(uint64(c.UID), 10))
	}
	if c.Mint != "" {
		q.Set("mint", c.Mint)
	}
	if c.Room != "" {
		q.Set("room", c.Room)
	}
	if c.User != "" {
		q.Set("user", c.User)
	}
	u := url.URL{Scheme: Scheme, Host: c.AppID, Path: "/" + c.Name, RawQuery: q.Encode()}
	return u.String()
}
