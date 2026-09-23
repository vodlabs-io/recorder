// Package platformauth stores a user's third-party streaming-platform login
// sessions (cookies) on the local machine so the daemon can record
// login-restricted live streams (e.g. age-gated 18+ TikTok rooms).
//
// PRIVACY — READ THIS BEFORE CHANGING ANYTHING HERE:
//
// The cookies handled by this package are the user's private login credentials
// for third-party platforms. They MUST NEVER leave the user's device:
//
//   - They are NEVER uploaded to, proxied through, or made readable by the
//     vodlabs cloud. The cloud only ever sends the daemon a platform NAME
//     (scrapeRequest.AuthPlatform); the daemon reads the cookies from this
//     store and attaches them to the outbound request itself, and only ever to
//     the platform's own domains (see internal/cloud/scraper.go).
//   - They are NEVER written to the daemon's NATS KV store, and therefore never
//     reach the Dashboard bridge (which only mirrors streamer.*/daemon.*).
//     Only non-secret status (Status, below — platform/account/expiry, no cookie
//     values) is ever surfaced outside this package.
//   - They are NEVER logged. Log cookie counts, never names or values.
//
// At rest, cookies are encrypted with AES-256-GCM. The encryption key lives in
// the OS keychain when one is available; on a headless host with no keychain it
// falls back to a key derived from stable machine identifiers. Cookies are never
// written to disk in plaintext.
package platformauth

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/zalando/go-keyring"

	"github.com/vodlabs-io/recorder/internal/vlconfig"
)

const (
	// keyringCookieService and keyEncKeyItem name the keychain entry that holds
	// the AES key (not the cookies — those stay in the encrypted file, which
	// also dodges small per-credential size limits on some OS keychains).
	keyringCookieService = "vodlabs-cookies"
	keyEncKeyItem        = "enc_key_v1"

	// cookieFileName is the encrypted jar, resolved against the config dir.
	cookieFileName = "cookies.json"
	// legacyCookieFileName is the jar in $HOME that NewStore migrates.
	legacyCookieFileName = ".vodlabs-cookies.json"

	// machineKeySalt domain-separates the fallback machine-derived key.
	machineKeySalt = "vodlabs-cookie-store-v1"

	fileVersion = 1
)

// Cookie is a single stored cookie. Value is secret.
type Cookie struct {
	Name     string `json:"name"`
	Value    string `json:"value"`
	Domain   string `json:"domain,omitempty"`
	Path     string `json:"path,omitempty"`
	Secure   bool   `json:"secure,omitempty"`
	HTTPOnly bool   `json:"http_only,omitempty"`
	// Expires is the cookie's expiry as a unix timestamp (seconds); 0 means a
	// session cookie with no explicit expiry.
	Expires int64 `json:"expires,omitempty"`
}

// Session is a stored login for one platform. It contains secret cookie values
// and must never be serialized anywhere but the encrypted store.
type Session struct {
	Platform   string   `json:"platform"`
	Account    string   `json:"account,omitempty"` // display label, best-effort (e.g. @user)
	Cookies    []Cookie `json:"cookies"`
	CapturedAt int64    `json:"captured_at"` // unix seconds
	// Healthy is false once an authenticated check found this login expired or
	// rejected. It defaults to true on capture. LastCheckedAt is the unix time of
	// the last authenticated check. Both are non-secret (surfaced in Status).
	Healthy       bool  `json:"healthy"`
	LastCheckedAt int64 `json:"last_checked_at,omitempty"`
}

// Header returns the value for an outbound "Cookie" request header: the full jar
// replayed as "name=value; name2=value2". PRIVACY: the result is secret and is
// only ever sent to the platform's own domains.
func (s Session) Header() string {
	parts := make([]string, 0, len(s.Cookies))
	for _, c := range s.Cookies {
		if c.Name == "" {
			continue
		}
		parts = append(parts, c.Name+"="+c.Value)
	}
	return strings.Join(parts, "; ")
}

// ExpiresHint returns the earliest non-session cookie expiry (unix seconds), or
// 0 when unknown. It is the expiry of the platform's designated session cookie
// (the login descriptor's SessionCookie, e.g. TikTok's "sessionid") when
// present, since that is what actually bounds the login's lifetime.
//
// It deliberately does NOT use the earliest expiry across the whole jar: jars
// carry short-lived auxiliary tokens (e.g. TikTok's msToken) whose expiry says
// nothing about the login and would show a misleading countdown. Platforms can
// still invalidate sessions server-side before this, so it remains a hint.
func (s Session) ExpiresHint() int64 {
	sessionCookie := ""
	if login := vlconfig.LoginFlowFor(s.Platform); login != nil {
		sessionCookie = login.SessionCookie
	}
	var earliest int64
	for _, c := range s.Cookies {
		if c.Expires <= 0 {
			continue
		}
		if c.Name == sessionCookie {
			return c.Expires
		}
		if earliest == 0 || c.Expires < earliest {
			earliest = c.Expires
		}
	}
	// No designated session cookie in the jar — fall back to the earliest.
	return earliest
}

// Status is the NON-SECRET projection of a session, safe to surface to the UI
// (and to write to the local platformlogin.* KV). It carries NO cookie values.
type Status struct {
	Platform      string `json:"platform"`
	Account       string `json:"account,omitempty"`
	ExpiresHint   int64  `json:"expires_hint,omitempty"` // unix seconds, 0 = unknown
	CapturedAt    int64  `json:"captured_at"`            // unix seconds
	CookieCount   int    `json:"cookie_count"`
	Healthy       bool   `json:"healthy"`
	LastCheckedAt int64  `json:"last_checked_at,omitempty"`
	// CanAuthenticate reports whether this jar can satisfy the platform's
	// request-authentication scheme. False means the capture dropped the cookie
	// the signature is derived from, so the login would show Active yet
	// authenticate nothing. Always true for platforms with no scheme.
	CanAuthenticate bool `json:"can_authenticate"`
}

func (s Session) Status() Status {
	hint := s.ExpiresHint()
	// Effective health: a login whose session cookie's expiry has passed is
	// expired regardless of the stored Healthy flag. This matters because some
	// platforms (TikTok, as of 2026-07) expose no reliable rejected-login
	// signal on the endpoints we check, so nothing else would ever flip the
	// Accounts row to "Expired".
	healthy := s.Healthy && (hint == 0 || time.Now().Unix() < hint)
	// A platform that declares no auth scheme needs nothing beyond the jar.
	canAuth := true
	if login := vlconfig.LoginFlowFor(s.Platform); login != nil && login.Auth != nil {
		canAuth = s.HasAuthSecret(login.Auth.Cookies)
	}
	return Status{
		Platform:        s.Platform,
		Account:         s.Account,
		ExpiresHint:     hint,
		CapturedAt:      s.CapturedAt,
		CookieCount:     len(s.Cookies),
		Healthy:         healthy,
		LastCheckedAt:   s.LastCheckedAt,
		CanAuthenticate: canAuth,
	}
}

// Store is the on-disk, encrypted cookie store. Safe for concurrent use.
type Store struct {
	mu       sync.Mutex
	filePath string
	// keyOverride, when set, is used instead of the keychain/machine key. Tests
	// set this to avoid touching the real keychain.
	keyOverride []byte
}

// Option customizes a Store (used by tests to isolate the file path).
type Option func(*Store)

// WithFilePath overrides where the encrypted store is written. Intended for
// tests; production uses cookies.json in the config dir.
func WithFilePath(path string) Option {
	return func(s *Store) { s.filePath = path }
}

// NewStore returns a Store backed by cookies.json in the config dir
// (encrypted), so VLREC_CONFIG_DIR moves the logins with the rest of the
// daemon's state.
func NewStore(opts ...Option) *Store {
	s := &Store{filePath: vlconfig.DefaultFilePath(cookieFileName)}
	for _, o := range opts {
		o(s)
	}
	if s.filePath == vlconfig.DefaultFilePath(cookieFileName) {
		migrateLegacyCookieFile(s.filePath)
	}
	return s
}

// migrateLegacyCookieFile moves a pre-existing ~/.vodlabs-cookies.json into the
// config dir once, so stored logins survive the move. Skipped when
// VLREC_CONFIG_DIR is set: a redirected profile inherits nothing.
func migrateLegacyCookieFile(dst string) {
	if vlconfig.ConfigDirOverridden() {
		return
	}
	if _, err := os.Stat(dst); err == nil {
		return
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	legacy := filepath.Join(home, legacyCookieFileName)
	if _, err := os.Stat(legacy); err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return
	}
	// Best effort: a failed migration leaves the legacy file untouched and the
	// user signs in again, which is recoverable. Losing it is not.
	_ = os.Rename(legacy, dst)
}

// Set upserts one platform's session (replacing any existing login for it).
func (s *Store) Set(sess Session) error {
	if sess.Platform == "" {
		return fmt.Errorf("session platform is empty")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sessions, err := s.load()
	if err != nil {
		return err
	}
	sessions[sess.Platform] = sess
	return s.save(sessions)
}

// Get returns the stored session for a platform, if any. PRIVACY: callers get
// the secret cookie values here; only the cookie-injection path should call it.
func (s *Store) Get(platform string) (Session, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sessions, err := s.load()
	if err != nil {
		return Session{}, false
	}
	sess, ok := sessions[platform]
	return sess, ok
}

// Has reports whether a session exists for the platform (no secrets returned).
func (s *Store) Has(platform string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	sessions, err := s.load()
	if err != nil {
		return false
	}
	_, ok := sessions[platform]
	return ok
}

// SetHealth records the outcome of an authenticated check for a platform: a
// still-valid login (healthy=true) or one the platform rejected (healthy=false),
// stamped with checkedAt (unix seconds). No-ops if the platform isn't stored.
func (s *Store) SetHealth(platform string, healthy bool, checkedAt int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	sessions, err := s.load()
	if err != nil {
		return err
	}
	sess, ok := sessions[platform]
	if !ok {
		return nil
	}
	sess.Healthy = healthy
	sess.LastCheckedAt = checkedAt
	sessions[platform] = sess
	return s.save(sessions)
}

// Remove deletes the stored session for a platform (sign out). Removing a
// platform that isn't stored is not an error.
func (s *Store) Remove(platform string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	sessions, err := s.load()
	if err != nil {
		return err
	}
	if _, ok := sessions[platform]; !ok {
		return nil
	}
	delete(sessions, platform)
	return s.save(sessions)
}

// List returns the NON-SECRET status of every stored session, sorted by
// platform. Safe to surface to the UI and KV.
func (s *Store) List() []Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	sessions, err := s.load()
	if err != nil {
		return nil
	}
	out := make([]Status, 0, len(sessions))
	for _, sess := range sessions {
		out = append(out, sess.Status())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Platform < out[j].Platform })
	return out
}

// --- persistence (caller holds s.mu) ---

type fileEnvelope struct {
	Version int    `json:"v"`
	Data    string `json:"data"` // base64(nonce || AES-256-GCM ciphertext)
}

// load reads and decrypts the store. A missing file yields an empty map. A file
// that fails to decrypt (e.g. key rotated because a keychain appeared where
// there was none) is treated as empty so the user can simply sign in again,
// rather than being wedged.
func (s *Store) load() (map[string]Session, error) {
	raw, err := os.ReadFile(s.filePath)
	if os.IsNotExist(err) {
		return map[string]Session{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read cookie store: %w", err)
	}
	var env fileEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return map[string]Session{}, nil
	}
	blob, err := base64.StdEncoding.DecodeString(env.Data)
	if err != nil {
		return map[string]Session{}, nil
	}
	key, err := s.encryptionKey()
	if err != nil {
		return nil, err
	}
	plain, err := decrypt(key, blob)
	if err != nil {
		return map[string]Session{}, nil
	}
	var sessions map[string]Session
	if err := json.Unmarshal(plain, &sessions); err != nil {
		return map[string]Session{}, nil
	}
	if sessions == nil {
		sessions = map[string]Session{}
	}
	return sessions, nil
}

// save encrypts and writes the store atomically with 0600 permissions.
func (s *Store) save(sessions map[string]Session) error {
	if len(sessions) == 0 {
		// Nothing to store — remove the file so an empty store leaves no trace.
		if err := os.Remove(s.filePath); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove cookie store: %w", err)
		}
		return nil
	}
	plain, err := json.Marshal(sessions)
	if err != nil {
		return fmt.Errorf("marshal sessions: %w", err)
	}
	key, err := s.encryptionKey()
	if err != nil {
		return err
	}
	blob, err := encrypt(key, plain)
	if err != nil {
		return err
	}
	env := fileEnvelope{Version: fileVersion, Data: base64.StdEncoding.EncodeToString(blob)}
	out, err := json.MarshalIndent(env, "", "  ")
	if err != nil {
		return err
	}
	// The config dir may not exist yet.
	if err := os.MkdirAll(filepath.Dir(s.filePath), 0o700); err != nil {
		return fmt.Errorf("create cookie store dir: %w", err)
	}
	tmp := s.filePath + ".tmp"
	if err := os.WriteFile(tmp, out, 0600); err != nil {
		return fmt.Errorf("write cookie store: %w", err)
	}
	if err := os.Rename(tmp, s.filePath); err != nil {
		return fmt.Errorf("replace cookie store: %w", err)
	}
	return nil
}

// encryptionKey returns the 32-byte AES key: the keychain-held key when a
// keychain is available (created on first use), else a machine-derived fallback.
func (s *Store) encryptionKey() ([]byte, error) {
	if s.keyOverride != nil {
		return s.keyOverride, nil
	}
	// Existing keychain key.
	if b64, err := keyring.Get(keyringCookieService, keyEncKeyItem); err == nil {
		if key, err := base64.StdEncoding.DecodeString(b64); err == nil && len(key) == 32 {
			return key, nil
		}
	}
	// Try to create and persist a fresh random key in the keychain.
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("generate key: %w", err)
	}
	if err := keyring.Set(keyringCookieService, keyEncKeyItem,
		base64.StdEncoding.EncodeToString(key)); err == nil {
		return key, nil
	}
	// No keychain (headless): derive a key from stable machine identifiers.
	// Weaker than a keychain-held key, but never plaintext — it defends against
	// casual disk/backup harvesting, not a local attacker running as this user.
	return machineKey(), nil
}

// machineKey derives a 32-byte key from stable, non-portable machine identifiers
// so the encrypted file can't be decrypted by simply copying it elsewhere.
func machineKey() []byte {
	h := sha256.New()
	h.Write([]byte(machineKeySalt))
	if id := machineID(); id != "" {
		h.Write([]byte(id))
	}
	if home, err := os.UserHomeDir(); err == nil {
		h.Write([]byte(home))
	}
	return h.Sum(nil)
}

// machineID returns a best-effort stable per-machine identifier. It is not
// required to be secret — it just anchors the fallback key to this host.
func machineID() string {
	for _, p := range []string{"/etc/machine-id", "/var/lib/dbus/machine-id"} {
		if b, err := os.ReadFile(p); err == nil {
			if id := strings.TrimSpace(string(b)); id != "" {
				return id
			}
		}
	}
	if host, err := os.Hostname(); err == nil {
		return host
	}
	return ""
}

func encrypt(key, plaintext []byte) ([]byte, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	return gcm.Seal(nonce, nonce, plaintext, nil), nil
}

func decrypt(key, blob []byte) ([]byte, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	ns := gcm.NonceSize()
	if len(blob) < ns {
		return nil, fmt.Errorf("ciphertext too short")
	}
	nonce, ct := blob[:ns], blob[ns:]
	return gcm.Open(nil, nonce, ct, nil)
}

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
