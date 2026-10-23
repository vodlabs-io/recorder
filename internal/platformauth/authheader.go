package platformauth

import (
	"crypto/md5"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Request-authentication schemes derived from a stored jar.
//
// PRIVACY: everything here reads cookie VALUES, so it belongs on the machine
// that holds them and nowhere else. The embedded login descriptor names the
// scheme; the cloud never sees the input or the output, and only the
// platform's own host receives the header, behind the same AllowsHost guard as
// the cookies.

// SchemeSAPISIDHASH is Google's InnerTube scheme. Cookies alone do not
// authenticate an InnerTube call: it also wants
//
//	Authorization: SAPISIDHASH <unix>_<sha1(unix + " " + SAPISID + " " + origin)>
//
// which is why a YouTube login needs more than a Cookie header to unlock
// members-only and age-restricted streams.
const SchemeSAPISIDHASH = "sapisidhash"

// SchemeOAuthCookie replays a cookie's value as an OAuth bearer credential:
//
//	Authorization: OAuth <cookie value>
//
// It is Twitch's scheme: Twitch's GraphQL API ignores the Cookie header, so
// only this header signs a request in as the user (which is what makes a
// subscriber-only stream resolvable). Unlike SAPISIDHASH it sends the secret
// itself, under the same AllowsHost guard as the cookies.
const SchemeOAuthCookie = "oauth-cookie"

// SchemeTiLiveChecksum is TiLive's scheme. Its API ignores the Cookie header:
// it reads the login from headers, and refuses a request whose checksum does
// not bind it to that login:
//
//	userId: <user id>
//	token: <token>
//	timestamp: <unix ms>
//	CheckSum: hex(md5(url + md5hex(body) + timestamp + userId + token[:6]))
//
// url is the request URL exactly as sent; md5hex(body) is empty for a request
// without one. Cookies names the token cookie, then the user-id cookie.
const SchemeTiLiveChecksum = "tilive-checksum"

// SchemeXCSRF is X's scheme. X's web API refuses a signed-in request whose
// CSRF header does not repeat the jar's ct0 cookie:
//
//	x-csrf-token: <ct0>
//	x-twitter-auth-type: OAuth2Session
//
// Cookies names the ct0 cookie. The app bearer token every request also
// carries is public, and comes with the request.
const SchemeXCSRF = "x-csrf"

// SchemeAuthorizationCookie replays a cookie's value as the whole
// Authorization header:
//
//	Authorization: <cookie value>
//
// It is Fansly's scheme: its API ignores cookies and reads the session token
// the site keeps in local storage, which the desktop's login capture copies
// into a cookie of the jar (see loginFlow.StorageCookies).
const SchemeAuthorizationCookie = "authorization-cookie"

// SignedRequest is the request a request-bound scheme signs.
type SignedRequest struct {
	URL  string
	Body []byte
}

// AuthHeaders returns the extra request headers a scheme contributes for this
// session and request, or nil when the scheme is unknown or a cookie it needs
// is absent.
//
// An unknown scheme is not an error: the cookies are sent alone.
func (s Session) AuthHeaders(scheme string, cookieNames []string, origin string, req SignedRequest) map[string]string {
	secret := s.firstCookie(cookieNames)
	if secret == "" {
		return nil
	}
	switch {
	case strings.EqualFold(scheme, SchemeSAPISIDHASH):
		if origin == "" {
			return nil
		}
		return map[string]string{
			"Authorization": sapisidHash(secret, origin, time.Now()),
			// InnerTube checks the declared origin against the signature.
			"X-Origin": origin,
		}
	case strings.EqualFold(scheme, SchemeOAuthCookie):
		return map[string]string{"Authorization": "OAuth " + secret}
	case strings.EqualFold(scheme, SchemeAuthorizationCookie):
		return map[string]string{"Authorization": secret}
	case strings.EqualFold(scheme, SchemeTiLiveChecksum):
		if len(cookieNames) < 2 {
			return nil
		}
		token, userID := s.firstCookie(cookieNames[:1]), s.firstCookie(cookieNames[1:2])
		if token == "" || userID == "" {
			return nil
		}
		return tiliveHeaders(userID, token, req, time.Now())
	case strings.EqualFold(scheme, SchemeXCSRF):
		return map[string]string{
			"x-csrf-token":        secret,
			"x-twitter-auth-type": "OAuth2Session",
		}
	default:
		return nil
	}
}

// HasAuthSecret reports whether the jar carries the cookie a scheme needs. It
// exists so a caller can tell "this login cannot authenticate" (a capture that
// dropped the cookie) from "this login was refused" — without reading values.
func (s Session) HasAuthSecret(cookieNames []string) bool {
	return s.firstCookie(cookieNames) != ""
}

// firstCookie returns the value of the first named cookie present in the jar.
func (s Session) firstCookie(names []string) string {
	for _, name := range names {
		for _, c := range s.Cookies {
			if c.Name == name && c.Value != "" {
				return c.Value
			}
		}
	}
	return ""
}

// sapisidHash builds the Authorization value. The timestamp is part of both the
// signature and the header, so the two must come from the same instant.
func sapisidHash(sapisid, origin string, now time.Time) string {
	ts := strconv.FormatInt(now.Unix(), 10)
	sum := sha1.Sum([]byte(ts + " " + sapisid + " " + origin))
	return fmt.Sprintf("SAPISIDHASH %s_%x", ts, sum)
}

// tiliveHeaders signs req for SchemeTiLiveChecksum. The timestamp is part of
// both the checksum and the headers, so the two must come from the same instant.
func tiliveHeaders(userID, token string, req SignedRequest, now time.Time) map[string]string {
	ts := strconv.FormatInt(now.UnixMilli(), 10)
	bodySum := ""
	if len(req.Body) > 0 {
		bodySum = md5Hex(req.Body)
	}
	prefix := token
	if len(prefix) > 6 {
		prefix = prefix[:6]
	}
	return map[string]string{
		"userId":    userID,
		"token":     token,
		"timestamp": ts,
		"CheckSum":  md5Hex([]byte(req.URL + bodySum + ts + userID + prefix)),
	}
}

func md5Hex(b []byte) string {
	sum := md5.Sum(b)
	return hex.EncodeToString(sum[:])
}
