// Package cookiemint mints the short-lived cookies some platforms' stream
// requests must carry. Such a platform's playlist answers only to cookies that
// expire seconds after they are minted, too soon to ask the service for a new
// set; the service instead describes how to mint them (a Mint), and the
// stream's client mints a fresh set itself as each one ages.
package cookiemint

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

// Mint describes how to mint a platform's stream cookies: a GET of URL, with
// Headers, answered by a JSON object whose top-level fields hold the cookie
// values.
type Mint struct {
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers,omitempty"`
	// Cookies maps each cookie's name to the reply field holding its value.
	Cookies map[string]string `json:"cookies"`
	// TTL is how many seconds a minted set stays valid.
	TTL int64 `json:"ttl"`
}

// refreshLead is the fraction of a set's TTL after which the next request
// mints a new one. A set minted early costs one request; one used late costs a
// refused playlist.
const refreshLead = 0.6

// mintTimeout bounds one mint, so a stalled mint fails the request it was for
// rather than every request queued behind it.
const mintTimeout = 10 * time.Second

// maxReply bounds a mint's reply.
const maxReply = 64 << 10

// Client returns a client that sends base's requests with freshly minted
// cookies, to the mint URL's own site only. It returns base when m is nil or
// is not a mint it can use.
func Client(base *http.Client, m *Mint) *http.Client {
	if m == nil || base == nil {
		return base
	}
	site, err := m.site()
	if err != nil {
		return base
	}
	inner := base.Transport
	if inner == nil {
		inner = http.DefaultTransport
	}
	c := *base
	c.Transport = &transport{
		base: inner,
		site: site,
		minter: &minter{
			mint:   m,
			client: &http.Client{Transport: inner, Timeout: mintTimeout},
		},
	}
	return &c
}

// site returns the domain the minted cookies may be sent to: the mint host's
// parent (tango.me for gateway.tango.me), or the host itself when it has no
// subdomain to drop.
func (m *Mint) site() (string, error) {
	u, err := url.Parse(m.URL)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" {
		return "", fmt.Errorf("mint URL %q is not an HTTPS URL", m.URL)
	}
	if len(m.Cookies) == 0 || m.TTL <= 0 {
		return "", errors.New("mint names no cookies or no lifetime")
	}
	host := strings.ToLower(u.Hostname())
	if labels := strings.Split(host, "."); len(labels) > 2 {
		return strings.Join(labels[1:], "."), nil
	}
	return host, nil
}

type transport struct {
	base   http.RoundTripper
	site   string
	minter *minter
}

func (t *transport) RoundTrip(req *http.Request) (*http.Response, error) {
	host := strings.ToLower(req.URL.Hostname())
	if host != t.site && !strings.HasSuffix(host, "."+t.site) {
		return t.base.RoundTrip(req)
	}
	cookie, err := t.minter.current(req.Context())
	if err != nil {
		return nil, fmt.Errorf("mint stream cookies: %w", err)
	}
	r := req.Clone(req.Context())
	if prev := r.Header.Get("Cookie"); prev != "" {
		cookie = prev + "; " + cookie
	}
	r.Header.Set("Cookie", cookie)
	return t.base.RoundTrip(r)
}

// minter holds the current set and mints the next when it ages.
type minter struct {
	mint   *Mint
	client *http.Client

	mu     sync.Mutex
	cookie string
	at     time.Time
}

// current returns the Cookie header value to send now.
func (m *minter) current(ctx context.Context) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	lead := time.Duration(float64(m.mint.TTL) * float64(time.Second) * refreshLead)
	if m.cookie != "" && time.Since(m.at) < lead {
		return m.cookie, nil
	}
	// Dated from before the request, so the set is never taken to be younger
	// than it is.
	at := time.Now()
	cookie, err := m.fetch(ctx)
	if err != nil {
		return "", err
	}
	m.cookie, m.at = cookie, at
	return cookie, nil
}

func (m *minter) fetch(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.mint.URL, nil)
	if err != nil {
		return "", err
	}
	for k, v := range m.mint.Headers {
		req.Header.Set(k, v)
	}
	resp, err := m.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxReply))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("mint status %d", resp.StatusCode)
	}
	return render(body, m.mint.Cookies)
}

// render builds a Cookie header value from a mint's reply.
func render(body []byte, cookies map[string]string) (string, error) {
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.UseNumber() // an expiry must come back as written, not as a float
	var reply map[string]any
	if err := dec.Decode(&reply); err != nil {
		return "", fmt.Errorf("decode mint reply: %w", err)
	}
	names := make([]string, 0, len(cookies))
	for name := range cookies {
		names = append(names, name)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, name := range names {
		var value string
		switch v := reply[cookies[name]].(type) {
		case string:
			value = v
		case json.Number:
			value = v.String()
		}
		if value == "" {
			return "", fmt.Errorf("mint reply has no %q", cookies[name])
		}
		parts = append(parts, name+"="+value)
	}
	return strings.Join(parts, "; "), nil
}
