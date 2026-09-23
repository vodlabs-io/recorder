package vlapi

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/nats-io/nats.go/micro"
	"github.com/vodlabs-io/recorder/internal/platformauth"
	"github.com/vodlabs-io/recorder/internal/platformlogin"
	"github.com/vodlabs-io/recorder/internal/streamer"
	"github.com/vodlabs-io/recorder/internal/vlconfig"
	"github.com/vodlabs-io/recorder/internal/vlstate"
)

// Third-party platform login (Accounts) endpoints.
//
// PRIVACY: logins live ONLY on this machine in an encrypted store
// (internal/platformauth), are never uploaded to the cloud, and these subjects
// are NOT bridged to the Dashboard. platformlogin.list returns non-secret
// status only.

// setLoginRequest stores (or replaces) the login for one platform. Cookies come
// either as structured entries (desktop webview capture) or as a raw Cookie
// header string (the advanced paste path / CLI).
type setLoginRequest struct {
	Platform     string                `json:"platform"`
	Account      string                `json:"account,omitempty"`
	Cookies      []platformauth.Cookie `json:"cookies,omitempty"`
	CookieHeader string                `json:"cookie_header,omitempty"`
}

// removeLoginRequest signs out of one platform (deletes its stored login).
type removeLoginRequest struct {
	Platform string `json:"platform"`
}

// setLoginHealthRequest records the true state of a platform login, sent by
// the desktop's background refresh when the platform has ended the session
// (its session cookie is gone from the login webview).
type setLoginHealthRequest struct {
	Platform string `json:"platform"`
	Healthy  bool   `json:"healthy"`
}

func (s *Service) handlePlatformLoginSet(req micro.Request) {
	if s.cookies == nil {
		respond(req, fail(errInternal, "cookie store unavailable"))
		return
	}
	var payload setLoginRequest
	if err := json.Unmarshal(req.Data(), &payload); err != nil {
		respond(req, fail(errInvalidArg, "invalid JSON"))
		return
	}
	if payload.Platform == "" {
		respond(req, fail(errInvalidArg, "platform is required"))
		return
	}
	// Only accept logins for platforms that actually support one.
	login := vlconfig.LoginFlowFor(payload.Platform)
	if login == nil {
		respond(req, fail(errInvalidArg, fmt.Sprintf("platform %q does not support login", payload.Platform)))
		return
	}

	cookies := payload.Cookies
	if len(cookies) == 0 && payload.CookieHeader != "" {
		cookies = platformauth.ParseCookieHeader(payload.CookieHeader)
	}
	if len(cookies) == 0 {
		respond(req, fail(errInvalidArg, "no cookies provided"))
		return
	}
	// A jar without the platform's session cookie is not a login, e.g. a
	// YouTube Cookie header pasted from a google.com tab (no LOGIN_INFO).
	if name := login.SessionCookie; name != "" && !hasCookie(cookies, name) {
		respond(req, fail(errInvalidArg, fmt.Sprintf(
			"these cookies carry no %q, so they are not a signed-in %s session",
			name, payload.Platform)))
		return
	}

	account := payload.Account
	if account == "" {
		// Label the row with the account the jar belongs to, since what a
		// login unlocks is per-account.
		account = cookieValue(cookies, login.AccountCookie)
	}
	sess := platformauth.Session{
		Platform:   payload.Platform,
		Account:    account,
		Cookies:    cookies,
		CapturedAt: time.Now().Unix(),
		Healthy:    true, // fresh login is assumed valid until a check says otherwise
	}
	if err := s.cookies.Set(sess); err != nil {
		respond(req, fail(errInternal, fmt.Sprintf("failed to store login: %v", err)))
		return
	}
	// Publish the non-secret status so the Accounts UI updates immediately.
	if err := platformlogin.Put(context.Background(), s.kv, sess.Status()); err != nil {
		s.logger.With("func", "handlePlatformLoginSet").Warn("Failed to publish account status", "error", err)
	}
	// PRIVACY: log the platform and cookie count only — never cookie names/values.
	s.logger.With("func", "handlePlatformLoginSet").Info("Stored platform login",
		"platform", payload.Platform, "cookie_count", len(cookies))

	// A fresh login should unlock tracked streamers already stuck at a login
	// wall, without waiting for the next poll or a daemon restart.
	s.nudgeRestrictedStreamers(context.Background(), payload.Platform)

	respond(req, OK(sess.Status()))
}

// hasCookie reports whether the jar contains a non-empty cookie by name.
func hasCookie(cookies []platformauth.Cookie, name string) bool {
	return cookieValue(cookies, name) != ""
}

// cookieValue returns the named cookie's value, or "" when the jar has no such
// cookie. An empty name matches nothing, so a platform that declares no account
// cookie gets no label rather than the first cookie in the jar.
//
// PRIVACY: this reads secret values. The only caller that surfaces the result
// is the account label, which may name ONLY a descriptor's AccountCookie.
func cookieValue(cookies []platformauth.Cookie, name string) string {
	if name == "" {
		return ""
	}
	for _, c := range cookies {
		if c.Name == name {
			return c.Value
		}
	}
	return ""
}

// nudgeRestrictedStreamers enqueues a status re-check for every tracked
// streamer of the platform whose last known status was a login wall
// (restricted / auth_required). The plain re-check escalates to an
// authenticated one now that a login is stored (see
// internal/cloud/status_limit.go), so signing in starts these recordings
// automatically.
func (s *Service) nudgeRestrictedStreamers(ctx context.Context, platform string) {
	log := s.logger.With("func", "nudgeRestrictedStreamers", "platform", platform)
	lister, err := s.kv.ListKeysFiltered(ctx, "streamer."+platform+".>")
	if err != nil {
		log.Warn("Failed to list streamer docs", "error", err)
		return
	}
	for key := range lister.Keys() {
		if !vlstate.IsDocKVKey(key) {
			continue
		}
		entry, err := s.kv.Get(ctx, key)
		if err != nil {
			continue
		}
		var doc vlstate.StreamerDoc
		if err := json.Unmarshal(entry.Value(), &doc); err != nil {
			continue
		}
		if doc.Status != streamer.StatusRestricted && doc.Status != streamer.StatusAuthRequired {
			continue
		}
		id := streamer.StreamerRef{StreamerKey: streamer.StreamerKey{Platform: doc.Platform, Username: doc.Username}}
		select {
		case s.statusReqPriCh <- id:
			log.Info("Re-checking restricted streamer after sign-in", "id", id)
		default:
			log.Warn("Status queue full; skipping re-check", "id", id)
		}
	}
}

func (s *Service) handlePlatformLoginRemove(req micro.Request) {
	if s.cookies == nil {
		respond(req, fail(errInternal, "cookie store unavailable"))
		return
	}
	var payload removeLoginRequest
	if err := json.Unmarshal(req.Data(), &payload); err != nil {
		respond(req, fail(errInvalidArg, "invalid JSON"))
		return
	}
	if payload.Platform == "" {
		respond(req, fail(errInvalidArg, "platform is required"))
		return
	}
	if err := s.cookies.Remove(payload.Platform); err != nil {
		respond(req, fail(errInternal, fmt.Sprintf("failed to remove login: %v", err)))
		return
	}
	if err := platformlogin.Delete(context.Background(), s.kv, payload.Platform); err != nil {
		s.logger.With("func", "handlePlatformLoginRemove").Warn("Failed to delete account status", "error", err)
	}
	s.logger.With("func", "handlePlatformLoginRemove").Info("Removed platform login", "platform", payload.Platform)

	respond(req, OK(true))
}

func (s *Service) handlePlatformLoginHealth(req micro.Request) {
	if s.cookies == nil {
		respond(req, fail(errInternal, "cookie store unavailable"))
		return
	}
	var payload setLoginHealthRequest
	if err := json.Unmarshal(req.Data(), &payload); err != nil {
		respond(req, fail(errInvalidArg, "invalid JSON"))
		return
	}
	if payload.Platform == "" {
		respond(req, fail(errInvalidArg, "platform is required"))
		return
	}
	if err := s.cookies.SetHealth(payload.Platform, payload.Healthy, time.Now().Unix()); err != nil {
		respond(req, fail(errInternal, fmt.Sprintf("failed to set login health: %v", err)))
		return
	}
	// Republish the non-secret status so the Accounts UI updates immediately.
	if sess, ok := s.cookies.Get(payload.Platform); ok {
		if err := platformlogin.Put(context.Background(), s.kv, sess.Status()); err != nil {
			s.logger.With("func", "handlePlatformLoginHealth").Warn("Failed to publish account status", "error", err)
		}
	}
	s.logger.With("func", "handlePlatformLoginHealth").Info("Set platform login health",
		"platform", payload.Platform, "healthy", payload.Healthy)

	respond(req, OK(true))
}

func (s *Service) handlePlatformLoginList(req micro.Request) {
	if s.cookies == nil {
		respond(req, fail(errInternal, "cookie store unavailable"))
		return
	}
	// List returns non-secret status only (no cookie values).
	respond(req, OK(s.cookies.List()))
}
