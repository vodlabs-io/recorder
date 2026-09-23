package vlconfig

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

// Platform describes a supported streaming platform, in the shape of the cloud
// API's supported-platform list.
type Platform struct {
	Name string `json:"name"`
	// DisplayName is the stylized, human-facing label for the platform, e.g.
	// "TikTok" for "tiktok". When the cloud doesn't supply it, SetPlatforms fills
	// it from the embedded default, and the UI capitalizes the raw name as a
	// last resort.
	DisplayName string `json:"display_name,omitempty"`
	// ChannelURL is the template for a streamer's live page on this platform,
	// with {username} substituted. It is the only way to link to a streamer's
	// page, since no page URL is stored. Empty means the UI shows no link.
	ChannelURL string `json:"channel_url,omitempty"`
	// Login, when present, means the platform supports recording
	// login-restricted streams (e.g. 18+ TikTok) using a locally stored session.
	// It carries only public login metadata — never any cookie value.
	Login *loginFlow `json:"login,omitempty"`
	// StatusLimit paces outgoing status checks for this platform. Absent means
	// defaultStatusLimit.
	StatusLimit *RateLimit `json:"status_limit,omitempty"`
	// StreamLimit paces mid-recording stream re-signs for this platform, which
	// run on the cadence the platform signs its media URLs for rather than on a
	// polling cadence. Absent means defaultStreamLimit.
	StreamLimit *RateLimit `json:"stream_limit,omitempty"`
}

// RateLimit paces one class of outgoing request: Burst tokens available at
// once, one more every Interval. What one platform tolerates says nothing about
// the next, so each carries its own rather than sharing a single budget.
type RateLimit struct {
	Burst    int
	Interval time.Duration
}

// rawRateLimit is the wire shape: the cloud sends milliseconds, which survives
// JSON round-trips through the platforms cache unambiguously.
type rawRateLimit struct {
	Burst      int `json:"burst"`
	IntervalMS int `json:"interval_ms"`
}

func (r RateLimit) MarshalJSON() ([]byte, error) {
	return json.Marshal(rawRateLimit{Burst: r.Burst, IntervalMS: int(r.Interval.Milliseconds())})
}

func (r *RateLimit) UnmarshalJSON(b []byte) error {
	var raw rawRateLimit
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	r.Burst = raw.Burst
	r.Interval = time.Duration(raw.IntervalMS) * time.Millisecond
	return nil
}

// The limiter defaults, used for every platform the cloud says nothing about.
var (
	defaultStatusLimit = RateLimit{Burst: 3, Interval: 3 * time.Second}
	defaultStreamLimit = RateLimit{Burst: 3, Interval: 2 * time.Second}
)

// The bounds a cloud-supplied limit is clamped into. Unlike Login, limits are
// not client-authoritative, but the bounds keep a bad value from becoming a
// flood or a limiter that never hands out a token.
const (
	minLimitBurst    = 1
	maxLimitBurst    = 20
	minLimitInterval = 100 * time.Millisecond
	maxLimitInterval = 60 * time.Second
)

// clamp returns the limit to actually pace by: the fallback when nothing usable
// was supplied, otherwise the supplied one bounded into the range above.
func (r *RateLimit) clamp(fallback RateLimit) RateLimit {
	if r == nil || r.Burst <= 0 || r.Interval <= 0 {
		return fallback
	}
	return RateLimit{
		Burst:    min(max(r.Burst, minLimitBurst), maxLimitBurst),
		Interval: min(max(r.Interval, minLimitInterval), maxLimitInterval),
	}
}

// StatusLimitFor returns the pace to make status checks for a platform at.
// An unknown platform gets the default, which is also what a URL-only lookup
// (whose platform isn't known until the cloud matches it) is paced by.
func StatusLimitFor(name string) RateLimit {
	return limitFor(name, defaultStatusLimit, func(p Platform) *RateLimit { return p.StatusLimit })
}

// StreamLimitFor returns the pace to re-sign a recording's stream URLs at.
func StreamLimitFor(name string) RateLimit {
	return limitFor(name, defaultStreamLimit, func(p Platform) *RateLimit { return p.StreamLimit })
}

func limitFor(name string, fallback RateLimit, pick func(Platform) *RateLimit) RateLimit {
	platformsMu.RLock()
	defer platformsMu.RUnlock()
	for _, p := range platforms {
		if p.Name == name {
			return pick(p).clamp(fallback)
		}
	}
	return fallback
}

// loginFlow is the public descriptor for signing in to a platform. It tells
// the desktop where to open the login page and which cookies to capture, and
// tells the daemon the exact set of hosts its stored cookies may be attached to.
//
// PRIVACY: this is public metadata only. The cookie VALUES live in the encrypted
// platformauth store and never appear here or anywhere cloud-visible.
type loginFlow struct {
	// URL is the page the desktop opens for the user to sign in.
	URL string `json:"url"`
	// RefreshURL is the page the background refresh reloads to re-mint the
	// stored jar; empty means URL serves both. They differ when sign-in lives
	// elsewhere (YouTube signs in on accounts.google.com, but youtube.com
	// writes its login cookies).
	RefreshURL string `json:"refresh_url,omitempty"`
	// CookieDomains are the registrable domains whose cookies make up the login.
	// The daemon will attach the stored cookies ONLY to requests whose host
	// matches one of these (see AllowsHost) — the hard guard that keeps a login
	// from ever being sent anywhere but the platform's own domains.
	CookieDomains []string `json:"cookie_domains,omitempty"`
	// SuccessURLPattern is a regexp; when the login webview navigates to a URL
	// matching it, the login is treated as complete and cookies are captured.
	SuccessURLPattern string `json:"success_url_pattern,omitempty"`
	// Auth, when set, names an extra request-authentication scheme this
	// platform needs on top of the cookie jar. Cookies alone do not
	// authenticate every API: YouTube's InnerTube wants a header derived from
	// one of them. The scheme is applied by the daemon, which is the only place
	// the cookie VALUES exist.
	Auth *authScheme `json:"auth,omitempty"`
	// Required means the platform refuses every unauthenticated lookup, so the
	// daemon polls it with the user's own login (see
	// internal/cloud/status_sweep.go). Client-authoritative, since it decides
	// whether a user's account is spent on round-the-clock checking. Distinct
	// from a room that is merely restricted (an 18+ TikTok stream).
	Required bool `json:"required,omitempty"`
	// SessionCookie, when set, names the cookie whose presence in the login
	// webview means the user is signed in (e.g. "sessionid"). The desktop uses it
	// as the primary, most reliable capture trigger; SuccessURLPattern is a
	// fallback. Purely a hint — no cookie value ever appears here.
	SessionCookie string `json:"session_cookie,omitempty"`
	// AccountCookie, when set, names the cookie carrying the signed-in account's
	// own name (Twitch's "login"), used to label the Accounts row. The label is
	// shown in the UI, so never name a secret cookie here.
	AccountCookie string `json:"account_cookie,omitempty"`
}

// authScheme describes how to derive an Authorization header from the stored
// jar. Like the rest of loginFlow it is client-authoritative (embedded, never
// taken from the cloud), since it decides what is computed from a user's
// credentials. An unrecognised Scheme is ignored and the cookies go alone.
type authScheme struct {
	// Scheme names the algorithm. Only "sapisidhash" exists today.
	Scheme string `json:"scheme"`
	// Cookies are candidate cookie names, most-preferred first; the first one
	// present in the jar is used as the secret.
	Cookies []string `json:"cookies,omitempty"`
	// Origin is the origin the signature is bound to, sent alongside as
	// X-Origin. Empty means the scheme has no origin to bind.
	Origin string `json:"origin,omitempty"`
}

// AllowsHost reports whether cookies for this platform may be attached to a
// request for the given host. A CookieDomains entry "tiktok.com" matches
// "tiktok.com" and any subdomain ("www.tiktok.com", "webcast.tiktok.com").
func (l *loginFlow) AllowsHost(host string) bool {
	if l == nil {
		return false
	}
	host = strings.ToLower(host)
	if i := strings.IndexByte(host, ':'); i >= 0 {
		host = host[:i] // strip port
	}
	for _, d := range l.CookieDomains {
		d = strings.ToLower(strings.TrimPrefix(d, "."))
		if d == "" {
			continue
		}
		if host == d || strings.HasSuffix(host, "."+d) {
			return true
		}
	}
	return false
}

// PageToRefresh returns the URL a background refresh should load to re-mint
// this platform's jar: RefreshURL when it names one, else the sign-in page.
func (l *loginFlow) PageToRefresh() string {
	if l == nil {
		return ""
	}
	if l.RefreshURL != "" {
		return l.RefreshURL
	}
	return l.URL
}

// LoginFlowFor returns the login descriptor for a platform by name, or nil
// if the platform is unknown or has no login support.
func LoginFlowFor(name string) *loginFlow {
	platformsMu.RLock()
	defer platformsMu.RUnlock()
	for _, p := range platforms {
		if p.Name == name {
			return p.Login
		}
	}
	return nil
}

// LoginRequiredFor reports whether a platform refuses every unauthenticated
// lookup, so nothing but a check carrying the user's own login can observe a
// broadcast. False for an unknown platform and for one with no login descriptor:
// both mean "nothing here asks the daemon to poll with a user's credentials".
func LoginRequiredFor(name string) bool {
	l := LoginFlowFor(name)
	return l != nil && l.Required
}

// platforms is the live supported-platform list. It is seeded from the embedded
// default at startup, overlaid with the on-disk cache (last-known-good), and
// refreshed at runtime from the heartbeat. Access is guarded by platformsMu
// because the heartbeat goroutine writes it while config readers read it.
var (
	platformsMu sync.RWMutex
	platforms   = defaultPlatforms()
)

// defaultPlatforms decodes the embedded default list used as the in-memory seed.
func defaultPlatforms() []Platform {
	var p []Platform
	_ = json.Unmarshal(defaultPlatformsJSON, &p)
	return p
}

// embeddedLogins maps platform name → its embedded login descriptor. It is the
// authoritative source for login support and, critically, for CookieDomains —
// the allowlist that bounds where the daemon may ever attach a user's cookies.
var embeddedLogins = func() map[string]*loginFlow {
	m := make(map[string]*loginFlow)
	for _, p := range defaultPlatforms() {
		if p.Login != nil {
			m[p.Name] = p.Login
		}
	}
	return m
}()

// embeddedDisplayNames maps platform name → its embedded display label, the
// fallback when the incoming list omits one.
var embeddedDisplayNames = func() map[string]string {
	m := make(map[string]string)
	for _, p := range defaultPlatforms() {
		if p.DisplayName != "" {
			m[p.Name] = p.DisplayName
		}
	}
	return m
}()

// embeddedChannelURLs maps platform name → its embedded live-page template, the
// fallback when the incoming list omits one.
var embeddedChannelURLs = func() map[string]string {
	m := make(map[string]string)
	for _, p := range defaultPlatforms() {
		if p.ChannelURL != "" {
			m[p.Name] = p.ChannelURL
		}
	}
	return m
}()

// embeddedLimits maps platform name → its embedded rate limits, the fallback
// when the incoming list omits them.
var embeddedLimits = func() map[string]Platform {
	m := make(map[string]Platform)
	for _, p := range defaultPlatforms() {
		if p.StatusLimit != nil || p.StreamLimit != nil {
			m[p.Name] = p
		}
	}
	return m
}()

// withEmbeddedDefaults stamps each platform's Login from the embedded default
// (matched by name) and backfills DisplayName, ChannelURL and the rate limits
// when the incoming list omits them.
//
// PRIVACY/SECURITY: login metadata, above all CookieDomains, is taken ONLY from
// the embedded default and never from the cloud, so a compromised cloud cannot
// widen the allowlist and leak cookies. The other fields are safe to take from
// the cloud: limits are clamped at use, and the UI checks ChannelURL for https.
func withEmbeddedDefaults(p []Platform) []Platform {
	out := slices.Clone(p)
	for i := range out {
		out[i].Login = embeddedLogins[out[i].Name]
		if out[i].DisplayName == "" {
			out[i].DisplayName = embeddedDisplayNames[out[i].Name]
		}
		if out[i].ChannelURL == "" {
			out[i].ChannelURL = embeddedChannelURLs[out[i].Name]
		}
		if e, ok := embeddedLimits[out[i].Name]; ok {
			if out[i].StatusLimit == nil {
				out[i].StatusLimit = e.StatusLimit
			}
			if out[i].StreamLimit == nil {
				out[i].StreamLimit = e.StreamLimit
			}
		}
	}
	return out
}

// Platforms returns a copy of the current supported-platform list.
func Platforms() []Platform {
	platformsMu.RLock()
	defer platformsMu.RUnlock()
	return slices.Clone(platforms)
}

// PlatformNames returns the names of the current supported platforms. Most
// callers only need names (URL validation, TOML section iteration).
func PlatformNames() []string {
	platformsMu.RLock()
	defer platformsMu.RUnlock()
	names := make([]string, len(platforms))
	for i, p := range platforms {
		names[i] = p.Name
	}
	return names
}

// SetPlatforms replaces the current supported-platform list and reports whether
// it actually changed. Login descriptors are re-stamped from the embedded
// default and missing display names backfilled — see withEmbeddedDefaults for
// why the login source is authoritative. Callers rely on the returned bool
// rather than comparing against the raw input, which would never match the
// stamped list.
func SetPlatforms(p []Platform) bool {
	stamped := withEmbeddedDefaults(p)
	platformsMu.Lock()
	defer platformsMu.Unlock()
	if slices.EqualFunc(platforms, stamped, platformEqual) {
		return false
	}
	platforms = stamped
	return true
}

// platformEqual compares two platforms including their (pointer) login
// descriptors by value, so SetPlatforms's change detection isn't fooled by the
// pointers differing while the content is identical.
func platformEqual(a, b Platform) bool {
	if a.Name != b.Name || a.DisplayName != b.DisplayName ||
		a.ChannelURL != b.ChannelURL {
		return false
	}
	if !rateLimitEqual(a.StatusLimit, b.StatusLimit) ||
		!rateLimitEqual(a.StreamLimit, b.StreamLimit) {
		return false
	}
	switch {
	case a.Login == nil && b.Login == nil:
		return true
	case a.Login == nil || b.Login == nil:
		return false
	default:
		return a.Login.URL == b.Login.URL &&
			a.Login.RefreshURL == b.Login.RefreshURL &&
			a.Login.SuccessURLPattern == b.Login.SuccessURLPattern &&
			a.Login.Required == b.Login.Required &&
			a.Login.SessionCookie == b.Login.SessionCookie &&
			a.Login.AccountCookie == b.Login.AccountCookie &&
			authSchemeEqual(a.Login.Auth, b.Login.Auth) &&
			slices.Equal(a.Login.CookieDomains, b.Login.CookieDomains)
	}
}

// authSchemeEqual compares two optional auth schemes by value.
func authSchemeEqual(a, b *authScheme) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Scheme == b.Scheme && a.Origin == b.Origin && slices.Equal(a.Cookies, b.Cookies)
}

// rateLimitEqual compares two optional rate limits by value.
func rateLimitEqual(a, b *RateLimit) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// platformsCachePath is the on-disk location of the synced platform list.
func platformsCachePath() string {
	return DefaultCacheFilePath("platforms.json")
}

// LoadPlatformsCache reads the cached supported-platform list. A missing file
// returns (nil, nil) so callers can fall back to the in-memory seed.
func LoadPlatformsCache() ([]Platform, error) {
	return loadPlatformsCacheFrom(platformsCachePath())
}

func loadPlatformsCacheFrom(path string) ([]Platform, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var p []Platform
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, err
	}
	return p, nil
}

// SavePlatformsCache persists the supported-platform list. An empty list is a
// no-op so a bad cloud response can't poison the cache.
func SavePlatformsCache(p []Platform) error {
	if len(p) == 0 {
		return nil
	}
	return savePlatformsCacheTo(platformsCachePath(), p)
}

func savePlatformsCacheTo(path string, p []Platform) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	data, err := json.Marshal(p)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "platforms-*.json")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}
