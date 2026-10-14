// Package asiancammodels records an AsianCamModels (asiancammodels.com) room
// as an FLV stream.
//
// The site plays a room's broadcast over WebRTC from an Ant Media Server. A
// viewer asks the site for a play grant just before playing: a token that
// lapses within seconds, and a time-based subscriber code. So an
// asiancammodels:// URL names the broadcast, and the recorder takes a fresh
// grant each time it opens it.
package asiancammodels

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/vodlabs-io/recorder/internal/antmedia"
	"github.com/vodlabs-io/recorder/internal/useragent"
)

// Scheme names a broadcast the recorder takes a play grant for itself.
const Scheme = "asiancammodels"

// siteOrigin is where play grants come from; a variable so tests can stand in
// for the site.
var siteOrigin = "https://www.asiancammodels.com"

// grantAttempts bounds the grants tried when the server refuses one.
const grantAttempts = 2

var (
	namePattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)
	idPattern   = regexp.MustCompile(`^[0-9]{1,20}$`)
	hashPattern = regexp.MustCompile(`^[A-Za-z0-9]{8,64}$`)
)

// Broadcast is an asiancammodels:// URL: the room, the chatroom the model's
// current broadcast opened, and the stream it publishes.
//
// Its form is asiancammodels://<room>?chatroom=<id>&stream=<hash>.
type Broadcast struct {
	Room     string
	Chatroom string
	Stream   string
}

// IsURL reports whether u is an asiancammodels:// URL.
func IsURL(u string) bool {
	return strings.HasPrefix(u, Scheme+"://")
}

// Parse decodes an asiancammodels:// URL.
func Parse(raw string) (Broadcast, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return Broadcast{}, fmt.Errorf("parse asiancammodels url: %w", err)
	}
	if u.Scheme != Scheme {
		return Broadcast{}, fmt.Errorf("not an asiancammodels url: scheme %q", u.Scheme)
	}
	q := u.Query()
	b := Broadcast{Room: u.Host, Chatroom: q.Get("chatroom"), Stream: q.Get("stream")}
	if !namePattern.MatchString(b.Room) || !idPattern.MatchString(b.Chatroom) || !hashPattern.MatchString(b.Stream) {
		return Broadcast{}, errors.New("asiancammodels url lacks a room, chatroom or stream")
	}
	return b, nil
}

// URL encodes b as an asiancammodels:// URL.
func (b Broadcast) URL() string {
	q := url.Values{"chatroom": {b.Chatroom}, "stream": {b.Stream}}
	return Scheme + "://" + b.Room + "?" + q.Encode()
}

// Open takes a play grant for the broadcast u names and returns it as an FLV
// stream on a timeline starting at zero. Reading ends with io.EOF when the
// broadcast ends, or with the error that ended the playback. Close, or the
// end of ctx, stops it.
func Open(ctx context.Context, u string, client *http.Client, logger *slog.Logger) (io.ReadCloser, error) {
	b, err := Parse(u)
	if err != nil {
		return nil, err
	}
	if client == nil {
		client = &http.Client{}
	}
	logger = logger.With("asiancammodels_room", b.Room)
	var errs []error
	for range grantAttempts {
		g, err := fetchGrant(ctx, client, b)
		if err != nil {
			return nil, err
		}
		stream, err := antmedia.Play(ctx, g, client, logger)
		if err == nil {
			return stream, nil
		}
		errs = append(errs, err)
		if !errors.Is(err, antmedia.ErrUnauthorized) {
			break
		}
		// A subscriber code read just before it rolled over is refused.
		logger.Debug("Play grant refused, taking another", "error", err)
	}
	return nil, errors.Join(errs...)
}

// grantReply is the site's play grant.
type grantReply struct {
	Error      json.RawMessage `json:"error"`
	Token      string          `json:"token"`
	WSURL      string          `json:"ws_url"`
	Subscriber struct {
		ID   string `json:"id"`
		Code string `json:"code"`
	} `json:"subscriber"`
	Stream struct {
		Hash string `json:"hash"`
	} `json:"stream"`
}

// fetchGrant asks the site for a play grant, as its player does before each
// play.
func fetchGrant(ctx context.Context, client *http.Client, b Broadcast) (antmedia.Grant, error) {
	q := url.Values{"chatroom_id": {b.Chatroom}, "stream_hash": {b.Stream}}
	rctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, http.MethodGet, siteOrigin+"/xhr/get-stream-token?"+q.Encode(), nil)
	if err != nil {
		return antmedia.Grant{}, err
	}
	req.Header.Set("User-Agent", useragent.Fallback)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("Referer", siteOrigin+"/"+b.Room+"/webcam/")
	resp, err := client.Do(req)
	if err != nil {
		return antmedia.Grant{}, fmt.Errorf("asiancammodels play grant: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return antmedia.Grant{}, fmt.Errorf("asiancammodels play grant: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return antmedia.Grant{}, fmt.Errorf("asiancammodels play grant: status %d", resp.StatusCode)
	}
	var r grantReply
	if err := json.Unmarshal(body, &r); err != nil {
		return antmedia.Grant{}, fmt.Errorf("asiancammodels play grant: %w", err)
	}
	if len(r.Error) > 0 && string(r.Error) != "null" {
		return antmedia.Grant{}, fmt.Errorf("asiancammodels play grant refused: %s", r.Error)
	}
	ws, err := url.Parse(r.WSURL)
	if err != nil || ws.Scheme != "wss" || !strings.HasSuffix(ws.Hostname(), ".asiancammodels.com") {
		return antmedia.Grant{}, fmt.Errorf("asiancammodels play grant: unexpected signalling url %q", r.WSURL)
	}
	stream := b.Stream
	if hashPattern.MatchString(r.Stream.Hash) {
		stream = r.Stream.Hash
	}
	return antmedia.Grant{
		WebSocketURL:   r.WSURL,
		StreamID:       stream,
		Token:          r.Token,
		SubscriberID:   r.Subscriber.ID,
		SubscriberCode: r.Subscriber.Code,
	}, nil
}
