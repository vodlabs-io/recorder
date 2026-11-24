package agora

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// swag mints SWAG viewer tokens. A token is issued per stream session to the
// user account the request names in X-Session-ID and admits it for a minute,
// so every recording takes its own account and renews the token while joined.
var swag = &swagMinter{base: "https://" + SwagAPIHost + "/"}

// SwagAPIHost is the host that issues SWAG's tokens, and the only one a
// SWAG login is sent to.
const SwagAPIHost = "api.swag.live"

// swagWatchInterval is how often a joined SWAG stream's state is re-read.
const swagWatchInterval = 20 * time.Second

// swagRequestTimeout bounds one SWAG API read.
const swagRequestTimeout = 15 * time.Second

type swagMinter struct {
	base string
}

func (m *swagMinter) mint(ctx context.Context, client *http.Client, ch Channel) (Channel, error) {
	if ch.Room == "" {
		return Channel{}, errors.New("swag mint needs the stream session id")
	}
	account := randomHex(12)
	token, expires, err := m.token(ctx, client, ch, account)
	if err != nil {
		return Channel{}, err
	}
	ch.Token, ch.Account, ch.UID, ch.Expires = token, account, 0, expires
	return ch, nil
}

// renew mints a fresh token for the account ch joined with.
func (m *swagMinter) renew(ctx context.Context, client *http.Client, ch Channel) (string, time.Time, error) {
	return m.token(ctx, client, ch, ch.Account)
}

func (m *swagMinter) token(ctx context.Context, client *http.Client, ch Channel, account string) (string, time.Time, error) {
	ctx, cancel := context.WithTimeout(ctx, swagRequestTimeout)
	defer cancel()
	req, err := m.request(ctx, "streams/"+url.PathEscape(ch.Room)+"/token")
	if err != nil {
		return "", time.Time{}, err
	}
	req.Header.Set("X-Session-ID", account)
	ch.signIn(req)
	resp, err := client.Do(req)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("swag token: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusForbidden {
		// A show the viewer has not paid for, a private show and a finished
		// session all answer 403; the stream's state says which.
		reason := m.withheld(ctx, client, ch)
		if reason == "" && ch.signedIn() {
			reason = "not paid for"
		}
		if reason != "" {
			return "", time.Time{}, &RefusedError{Platform: "swag", Reason: reason}
		}
	}
	if resp.StatusCode != http.StatusOK {
		return "", time.Time{}, fmt.Errorf("swag token: status %d", resp.StatusCode)
	}
	var reply struct {
		Token string `json:"agora_token_session_id"`
		Exp   int64  `json:"agora_exp"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&reply); err != nil {
		return "", time.Time{}, fmt.Errorf("swag token: decode: %w", err)
	}
	if reply.Token == "" || reply.Exp == 0 {
		return "", time.Time{}, errors.New("swag returned no token")
	}
	return reply.Token, time.Unix(reply.Exp, 0), nil
}

// withheld returns why SWAG no longer offers ch's stream session to this
// viewer: "finished" once the streamer is offline or in another session; for
// a viewer not signed in, the stream's mode (a paid show) once it is not free.
// Empty while it is offered, or when the state cannot be read. A signed-in
// viewer may have paid for a show, so only the token endpoint refuses one.
func (m *swagMinter) withheld(ctx context.Context, client *http.Client, ch Channel) string {
	if ch.User == "" {
		return ""
	}
	channel := "private-stream-rt@" + ch.User
	ctx, cancel := context.WithTimeout(ctx, swagRequestTimeout)
	defer cancel()
	req, err := m.request(ctx, "realtime/state?channels="+url.QueryEscape(channel))
	if err != nil {
		return ""
	}
	req.Header.Set("X-Client-ID", randomUUID())
	resp, err := client.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	var reply struct {
		Channels map[string]struct {
			State struct {
				Session *struct {
					SessionID string `json:"sessionId"`
					Mode      string `json:"mode"`
				} `json:"session"`
			} `json:"state"`
		} `json:"channels"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&reply); err != nil {
		return ""
	}
	st, ok := reply.Channels[channel]
	if !ok {
		return ""
	}
	s := st.State.Session
	switch {
	case s == nil || s.SessionID != ch.Room:
		return "finished"
	case s.Mode != "" && s.Mode != "free" && !ch.signedIn():
		return s.Mode
	}
	return ""
}

// watch re-reads ch's stream state until SWAG stops offering it for free.
func (m *swagMinter) watch(ctx context.Context, client *http.Client, ch Channel, every time.Duration) error {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
		switch reason := m.withheld(ctx, client, ch); reason {
		case "":
		case "finished":
			return ErrChannelEnded
		default:
			return &RefusedError{Platform: "swag", Reason: reason}
		}
	}
}

func (m *swagMinter) request(ctx context.Context, path string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.base+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Origin", "https://swag.live")
	req.Header.Set("Referer", "https://swag.live/")
	return req, nil
}

func randomUUID() string {
	h := randomHex(16)
	return h[:8] + "-" + h[8:12] + "-4" + h[13:16] + "-a" + h[17:20] + "-" + h[20:]
}
