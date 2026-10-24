package webui

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/vodlabs-io/recorder/internal/webtoken"
)

const (
	// SecretFileName holds the random secret session cookies are keyed by,
	// with the token. Replacing it signs every browser out.
	SecretFileName = "web-session.key"
	// RevokedFileName lists signed-out session ids until they expire.
	RevokedFileName = "web-revoked.json"

	sessionTTL     = 30 * 24 * time.Hour
	sessionRenewAt = 15 * 24 * time.Hour

	payloadVersion = 0x01
	payloadLen     = 1 + 16 + 8 + 8

	keyLabel = "vlrec-web-session-key-v1\x00"
	tagLabel = "vlrec-web-session-v1\x00"
)

type sessionID [16]byte

// claims is a verified session cookie's payload.
type claims struct {
	sid sessionID
	iat time.Time
	exp time.Time
}

// codec mints and verifies session cookies under one key, derived from the
// session secret and the token, so that changing either invalidates every
// cookie and knowing the token alone forges none.
type codec struct{ key []byte }

func newCodec(secret []byte, token string) codec {
	m := hmac.New(sha256.New, secret)
	m.Write([]byte(keyLabel))
	m.Write([]byte(token))
	return codec{key: m.Sum(nil)}
}

func (c codec) tag(payload []byte) []byte {
	m := hmac.New(sha256.New, c.key)
	m.Write([]byte(tagLabel))
	m.Write(payload)
	return m.Sum(nil)
}

// encode renders cl as b64url(payload).b64url(tag).
func (c codec) encode(cl claims) string {
	p := make([]byte, payloadLen)
	p[0] = payloadVersion
	copy(p[1:17], cl.sid[:])
	binary.BigEndian.PutUint64(p[17:25], uint64(cl.iat.Unix()))
	binary.BigEndian.PutUint64(p[25:33], uint64(cl.exp.Unix()))
	enc := base64.RawURLEncoding
	return enc.EncodeToString(p) + "." + enc.EncodeToString(c.tag(p))
}

var errBadSession = errors.New("invalid session")

func (c codec) decode(v string, now time.Time) (claims, error) {
	ps, ts, ok := strings.Cut(v, ".")
	if !ok {
		return claims{}, errBadSession
	}
	enc := base64.RawURLEncoding
	p, err := enc.DecodeString(ps)
	if err != nil || len(p) != payloadLen || p[0] != payloadVersion {
		return claims{}, errBadSession
	}
	t, err := enc.DecodeString(ts)
	if err != nil || !hmac.Equal(t, c.tag(p)) {
		return claims{}, errBadSession
	}
	var cl claims
	copy(cl.sid[:], p[1:17])
	cl.iat = time.Unix(int64(binary.BigEndian.Uint64(p[17:25])), 0)
	cl.exp = time.Unix(int64(binary.BigEndian.Uint64(p[25:33])), 0)
	if !now.Before(cl.exp) {
		return claims{}, errBadSession
	}
	return cl, nil
}

func newClaims(now time.Time) (claims, error) {
	var cl claims
	if _, err := rand.Read(cl.sid[:]); err != nil {
		return claims{}, err
	}
	cl.iat = now
	cl.exp = now.Add(sessionTTL)
	return cl, nil
}

// readSecret returns the session secret in dir, creating it when missing.
func readSecret(dir string) ([]byte, error) {
	path := filepath.Join(dir, SecretFileName)
	b, err := os.ReadFile(path)
	if err == nil {
		if s, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(b))); err == nil && len(s) >= 32 {
			return s, nil
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	s := make([]byte, 32)
	if _, err := rand.Read(s); err != nil {
		return nil, err
	}
	if err := webtoken.WriteSecret(path, []byte(base64.StdEncoding.EncodeToString(s)+"\n")); err != nil {
		return nil, fmt.Errorf("write %s: %w", path, err)
	}
	return s, nil
}

// revocations are signed-out session ids, kept until the latest moment a
// cookie carrying them could still be valid.
type revocations struct {
	mu   sync.Mutex
	path string
	ids  map[sessionID]time.Time
}

func loadRevocations(dir string) *revocations {
	r := &revocations{path: filepath.Join(dir, RevokedFileName), ids: map[sessionID]time.Time{}}
	b, err := os.ReadFile(r.path)
	if err != nil {
		return r
	}
	var raw map[string]int64
	if json.Unmarshal(b, &raw) != nil {
		return r
	}
	for k, exp := range raw {
		var id sessionID
		if d, err := hex.DecodeString(k); err == nil && len(d) == len(id) {
			copy(id[:], d)
			r.ids[id] = time.Unix(exp, 0)
		}
	}
	return r
}

func (r *revocations) revoked(id sessionID) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.ids[id]
	return ok
}

// revoke records id and prunes expired entries.
func (r *revocations) revoke(id sessionID, now time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ids[id] = now.Add(sessionTTL)
	raw := make(map[string]int64, len(r.ids))
	for k, exp := range r.ids {
		if !now.Before(exp) {
			delete(r.ids, k)
			continue
		}
		raw[hex.EncodeToString(k[:])] = exp.Unix()
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return err
	}
	return webtoken.WriteSecret(r.path, b)
}

// clear forgets every entry, once the key they were minted under is gone.
func (r *revocations) clear() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.ids) == 0 {
		return
	}
	r.ids = map[sessionID]time.Time{}
	os.Remove(r.path)
}
