package platformauth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/vodlabs-io/recorder/internal/useragent"
	"github.com/vodlabs-io/recorder/internal/vlconfig"
)

// renewMargin is how long before its expiry a bearer token is renewed.
const renewMargin = 2 * time.Minute

// renewTimeout bounds one renewal request.
const renewTimeout = 15 * time.Second

// ErrRenewalRefused is a renewal the platform turned down: the long-lived
// token is no longer valid, and only signing in again helps.
var ErrRenewalRefused = errors.New("the platform refused to renew the login; sign in again")

// renewMu serializes renewals, so callers that find the same token expired
// make one request between them.
var renewMu sync.Mutex

// Fresh returns the stored login for platform, first renewing its bearer token
// when the token has expired or is about to and the login descriptor's
// auth.renew says how. A renewed token is stored. When a renewal fails, the
// login comes back as stored, with the error; a refused renewal also marks the
// login unhealthy.
//
// PRIVACY: the renewal goes only to the platform's own host (the descriptor's
// URL must be on its cookie domains), and no token is logged.
func (s *Store) Fresh(ctx context.Context, client *http.Client, platform string) (Session, bool, error) {
	sess, ok := s.Get(platform)
	if !ok {
		return sess, false, nil
	}
	login := vlconfig.LoginFlowFor(platform)
	if login == nil || login.Auth == nil || login.Auth.Renew == nil || len(login.Auth.Cookies) == 0 {
		return sess, true, nil
	}
	name, r := login.Auth.Cookies[0], login.Auth.Renew
	if !needsRenewal(sess.cookie(name), time.Now()) {
		return sess, true, nil
	}

	renewMu.Lock()
	defer renewMu.Unlock()
	// Another caller may have renewed it while this one waited.
	if sess, ok = s.Get(platform); !ok {
		return sess, false, nil
	}
	old := sess.cookie(name)
	if !needsRenewal(old, time.Now()) {
		return sess, true, nil
	}
	refresh := sess.cookie(r.Cookie)
	if refresh == "" {
		return sess, true, fmt.Errorf("%s login holds no token to renew it with", platform)
	}
	u, err := url.Parse(r.URL)
	if err != nil || u.Scheme != "https" || !login.AllowsHost(u.Host) {
		return sess, true, fmt.Errorf("%s renewal URL is not on the platform's domains", platform)
	}
	if client == nil {
		client = http.DefaultClient
	}
	token, err := renewToken(ctx, client, u.String(), login.URL, r.Field, refresh)
	if errors.Is(err, ErrRenewalRefused) {
		_ = s.SetHealth(platform, false, time.Now().Unix())
	}
	if err != nil {
		return sess, true, err
	}
	updated, err := s.replaceCookie(platform, name, old, token)
	if err != nil {
		return sess, true, err
	}
	return updated, true, nil
}

// needsRenewal reports whether token is missing or a JWT whose expiry is
// within renewMargin of now. A token that is not a JWT is never renewed: its
// expiry is unknown.
func needsRenewal(token string, now time.Time) bool {
	if token == "" {
		return true
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return false
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return false
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if json.Unmarshal(raw, &claims) != nil || claims.Exp == 0 {
		return false
	}
	return now.Add(renewMargin).Unix() >= claims.Exp
}

// renewToken POSTs to endpoint with refresh as the bearer credential and
// returns the reply's field.
func renewToken(ctx context.Context, client *http.Client, endpoint, page, field, refresh string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, renewTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, http.NoBody)
	if err != nil {
		return "", err
	}
	req.ContentLength = 0
	req.Header.Set("Authorization", "Bearer "+refresh)
	req.Header.Set("User-Agent", useragent.Fallback)
	if p, err := url.Parse(page); err == nil && p.Host != "" {
		req.Header.Set("Origin", p.Scheme+"://"+p.Host)
		req.Header.Set("Referer", p.Scheme+"://"+p.Host+"/")
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("renew login: %w", err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return "", ErrRenewalRefused
	case resp.StatusCode != http.StatusOK:
		return "", fmt.Errorf("renew login: status %d", resp.StatusCode)
	}
	var reply map[string]any
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&reply); err != nil {
		return "", fmt.Errorf("renew login: decode: %w", err)
	}
	token, _ := reply[field].(string)
	if token == "" {
		return "", fmt.Errorf("renew login: reply has no %s", field)
	}
	return token, nil
}

// replaceCookie sets platform's cookie name to value, unless the stored value
// is no longer old (a new sign-in or refresh stored a jar meanwhile).
func (s *Store) replaceCookie(platform, name, old, value string) (Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sessions, err := s.load()
	if err != nil {
		return Session{}, err
	}
	sess, ok := sessions[platform]
	if !ok {
		return Session{}, fmt.Errorf("%s login was removed", platform)
	}
	if sess.cookie(name) != old {
		return sess, nil
	}
	replaced := false
	for i := range sess.Cookies {
		if sess.Cookies[i].Name == name {
			sess.Cookies[i].Value = value
			replaced = true
			break
		}
	}
	if !replaced {
		c := Cookie{Name: name, Value: value, Path: "/", Secure: true}
		if login := vlconfig.LoginFlowFor(platform); login != nil && len(login.CookieDomains) > 0 {
			c.Domain = "." + strings.TrimPrefix(login.CookieDomains[0], ".")
		}
		sess.Cookies = append(sess.Cookies, c)
	}
	sess.Healthy = true
	sessions[platform] = sess
	return sess, s.save(sessions)
}

// cookie returns the value of the named cookie in the jar, or "".
func (s Session) cookie(name string) string {
	for _, c := range s.Cookies {
		if c.Name == name {
			return c.Value
		}
	}
	return ""
}
