// Package agora records a broadcast carried over Agora's real-time network, the
// way the Agora Web SDK watches one: an access-point lookup, a signalling
// WebSocket to an edge gateway, and a WebRTC peer connection that receives the
// broadcaster's H.264 video and Opus audio. Open hands the media back as an FLV
// stream, so the FLV writers record it like any HTTP-FLV origin.
package agora

import (
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Scheme is the URL scheme of a stream that is an Agora channel rather than
// an HTTP resource.
const Scheme = "agora"

// Channel names an Agora channel and the credentials to join it as audience.
//
// Its URL form is agora://<app id>/<channel>?token=<t>&uid=<n>, optionally with
// account=<a> for a token issued to a string user account,
// mint=<platform>&room=<id>&user=<id> when the platform binds a token to one
// viewer, and codec=vp8 for a channel whose broadcasters send VP8.
type Channel struct {
	AppID string
	Name  string
	Token string
	UID   uint32
	// Account is the string user account the token was issued to; the join
	// registers it for its numeric uid. Empty for a token issued to UID.
	Account string
	// Expires is when Token stops admitting the client, for a mint whose
	// tokens are renewed during the session; zero otherwise.
	Expires time.Time
	// Mint names a platform whose join tokens belong to one viewer: two
	// clients joining with the same token evict each other, so a recorder
	// mints its own rather than sharing the one the check returned. Empty
	// means Token and UID are used as given.
	Mint string
	// Room is the platform's id for the broadcast, which the mint needs, and
	// User the broadcaster's, for finding a broadcast that replaced it.
	Room string
	User string
	// Codec is the video codec the channel's broadcasters send: CodecVP8,
	// or empty for H.264.
	Codec string

	// login signs the mint's requests in as the user; nil when signed out.
	// Never part of the URL.
	login *seatLogin
}

// seatLogin holds the headers that sign a request in as the user.
type seatLogin struct {
	headers map[string]string
}

// signedIn reports whether the channel's mint signs its requests in.
func (c Channel) signedIn() bool { return c.login != nil && len(c.login.headers) > 0 }

// signIn adds the login's headers to req.
func (c Channel) signIn(req *http.Request) {
	if c.login == nil {
		return
	}
	for k, v := range c.login.headers {
		req.Header.Set(k, v)
	}
}

// CodecVP8 marks a channel carrying VP8 video. An Agora app picks one codec
// for all its clients, and a gateway forwards a broadcaster's video only to an
// audience that joined with the same one.
const CodecVP8 = "vp8"

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
		AppID:   u.Host,
		Name:    strings.TrimPrefix(u.Path, "/"),
		Token:   u.Query().Get("token"),
		Account: u.Query().Get("account"),
		Mint:    u.Query().Get("mint"),
		Room:    u.Query().Get("room"),
		User:    u.Query().Get("user"),
		Codec:   u.Query().Get("codec"),
	}
	if c.Codec != "" && c.Codec != CodecVP8 {
		return Channel{}, fmt.Errorf("agora url: unsupported codec %q", c.Codec)
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

// MintOf returns the platform whose token mint the agora:// URL raw names, or
// empty.
func MintOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != Scheme {
		return ""
	}
	return u.Query().Get("mint")
}

// WithCodec returns the agora:// URL raw with its codec set to codec: CodecVP8,
// or empty for H.264.
func WithCodec(raw, codec string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("parse agora url: %w", err)
	}
	if u.Scheme != Scheme {
		return "", fmt.Errorf("not an agora url: scheme %q", u.Scheme)
	}
	q := u.Query()
	if codec == "" {
		q.Del("codec")
	} else {
		q.Set("codec", codec)
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
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
	if c.Account != "" {
		q.Set("account", c.Account)
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
	if c.Codec != "" {
		q.Set("codec", c.Codec)
	}
	u := url.URL{Scheme: Scheme, Host: c.AppID, Path: "/" + c.Name, RawQuery: q.Encode()}
	return u.String()
}
