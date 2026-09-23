package platformauth

import (
	"crypto/sha1"
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

// AuthHeaders returns the extra request headers a scheme contributes for this
// session, or nil when the scheme is unknown or its secret cookie is absent.
//
// An unknown scheme is not an error: the cookies are sent alone.
func (s Session) AuthHeaders(scheme string, cookieNames []string, origin string) map[string]string {
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
