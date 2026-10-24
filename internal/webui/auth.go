package webui

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
)

const (
	cookieHTTP  = "vlrec_session"
	cookieHTTPS = "__Host-vlrec_session"

	maxSignInBody = 4 << 10
)

type ctxKey struct{}

// sessionFrom returns the request's verified session.
func sessionFrom(ctx context.Context) claims {
	cl, _ := ctx.Value(ctxKey{}).(claims)
	return cl
}

func (s *Server) cookieName(r *http.Request) string {
	if s.proxies.isHTTPS(r) {
		return cookieHTTPS
	}
	return cookieHTTP
}

// session verifies the request's cookie.
func (s *Server) session(r *http.Request) (claims, bool) {
	c, err := r.Cookie(s.cookieName(r))
	if err != nil {
		return claims{}, false
	}
	_, cd := s.current()
	cl, err := cd.decode(c.Value, s.now())
	if err != nil || s.revoked.revoked(cl.sid) {
		return claims{}, false
	}
	return cl, true
}

func (s *Server) setCookie(w http.ResponseWriter, r *http.Request, cl claims) {
	_, cd := s.current()
	https := s.proxies.isHTTPS(r)
	http.SetCookie(w, &http.Cookie{
		Name:     s.cookieName(r),
		Value:    cd.encode(cl),
		Path:     "/",
		Expires:  cl.exp,
		MaxAge:   int(cl.exp.Sub(s.now()).Seconds()),
		HttpOnly: true,
		Secure:   https,
		SameSite: http.SameSiteLaxMode,
	})
}

func (s *Server) clearCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     s.cookieName(r),
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   s.proxies.isHTTPS(r),
		SameSite: http.SameSiteLaxMode,
	})
}

// requireSession answers 401 without a session.
func (s *Server) requireSession(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cl, ok := s.session(r)
		if !ok {
			writeStatus(w, http.StatusUnauthorized)
			return
		}
		h(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, cl)))
	}
}

// requirePage redirects to the sign-in page without a session, and renews a
// session half way through its life.
func (s *Server) requirePage(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cl, ok := s.session(r)
		if !ok {
			// Set by hand: http.Redirect would make the path absolute.
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("Location", signinPath(r.URL.Path))
			w.WriteHeader(http.StatusSeeOther)
			return
		}
		if now := s.now(); now.Sub(cl.iat) >= sessionRenewAt {
			cl.iat, cl.exp = now, now.Add(sessionTTL)
			s.setCookie(w, r, cl)
		}
		h(w, r)
	}
}

// signinPath is the sign-in page relative to path, so a redirect stays under
// a reverse proxy's subpath.
func signinPath(path string) string {
	depth := strings.Count(strings.TrimPrefix(path, "/"), "/")
	return strings.Repeat("../", depth) + "signin"
}

// tokenMatches compares in constant time, whatever the lengths.
func tokenMatches(got, want string) bool {
	g, w := sha256.Sum256([]byte(got)), sha256.Sum256([]byte(want))
	return subtle.ConstantTimeCompare(g[:], w[:]) == 1 && want != ""
}

func (s *Server) handleSignIn(w http.ResponseWriter, r *http.Request) {
	ip := s.proxies.clientIP(r)
	if wait := s.limit.blocked(ip); wait > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(wait.Seconds()))))
		writeStatus(w, http.StatusTooManyRequests)
		return
	}
	var body struct {
		Token string `json:"token"`
	}
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxSignInBody))
	if err != nil {
		writeStatus(w, http.StatusRequestEntityTooLarge)
		return
	}
	if err := json.Unmarshal(b, &body); err != nil {
		writeStatus(w, http.StatusBadRequest)
		return
	}
	token, _ := s.current()
	if !tokenMatches(strings.TrimSpace(body.Token), token) {
		s.limit.fail(ip)
		s.clientLog(r).Warn("Web UI sign-in failed")
		writeStatus(w, http.StatusUnauthorized)
		return
	}
	cl, err := newClaims(s.now())
	if err != nil {
		writeStatus(w, http.StatusInternalServerError)
		return
	}
	s.setCookie(w, r, cl)
	s.clientLog(r).Info("Web UI sign-in")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleSessionStatus(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.session(r); !ok {
		writeStatus(w, http.StatusUnauthorized)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleSignOut(w http.ResponseWriter, r *http.Request) {
	cl := sessionFrom(r.Context())
	if err := s.revoked.revoke(cl.sid, s.now()); err != nil {
		s.log.Warn("Web UI could not persist a sign-out", "error", err)
	}
	s.bridges.closeSession(cl.sid)
	s.clearCookie(w, r)
	s.clientLog(r).Info("Web UI sign-out")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}
