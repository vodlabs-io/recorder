package cloud

import (
	"encoding/json"
	"errors"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/vodlabs-io/recorder/internal/browserfetch"
	"github.com/vodlabs-io/recorder/internal/vlstate"
)

const (
	// sniffTimeout bounds a page load when the request names none.
	sniffTimeout = 20 * time.Second
	// maxSniffTimeout caps what the request may ask for.
	maxSniffTimeout = 30 * time.Second
	// sniffCacheTTL is how long a sniff report answers for its page.
	sniffCacheTTL = 60 * time.Second
)

// errSniffNotPermitted refuses a sniff of a page this machine's user did not
// ask to record.
var errSniffNotPermitted = errors.New("sniff not permitted: the user did not ask to record this page")

// checkSniffTarget reports whether the service may have this machine load u in
// a browser: an http(s) page on a public host that the user asked to record.
func (h *scrapeHandler) checkSniffTarget(u *url.URL) error {
	if u == nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return errors.New("invalid URL")
	}
	host := strings.ToLower(u.Hostname())
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return errPrivateAddress
	}
	if addr, err := netip.ParseAddr(host); err == nil && blockedAddr(addr) {
		return errPrivateAddress
	}
	if h.sniffPermitted == nil || !h.sniffPermitted(u.String()) {
		return errSniffNotPermitted
	}
	return nil
}

// handleSniff asks a co-located desktop app over LOCAL NATS to load a page in
// a throwaway browser and report the streams its player requested, and relays
// that report. With no desktop listening it fails, and the service reads the
// page without a browser instead.
//
// PRIVACY: the browser shares nothing with the user's platform logins, and the
// report carries only the stream addresses, the frames that requested them
// and the page title.
func (h *scrapeHandler) handleSniff(msg *nats.Msg, req scrapeRequest) {
	log := h.logger.With("func", "handleSniff")

	if !req.NoCache {
		if reply, ok := h.sniffs.get(req.URL); ok {
			log.Info("Sniff answered from cache", "url", req.URL, "candidates", len(reply.Candidates))
			h.respondSniff(msg, reply)
			return
		}
	}
	if h.localNC == nil {
		h.respondWithError(msg, "browser context unavailable")
		return
	}

	timeout := sniffTimeout
	if t := time.Duration(req.TimeoutMS) * time.Millisecond; t > 0 && t <= maxSniffTimeout {
		timeout = t
	}
	data, err := json.Marshal(browserfetch.SniffRequest{URL: req.URL, TimeoutMS: int(timeout / time.Millisecond)})
	if err != nil {
		h.respondWithError(msg, "failed to marshal sniff request")
		return
	}
	log.Info("Sniffing page", "url", req.URL)
	start := time.Now()
	// The desktop's own deadline is the page load's; this one only covers the
	// hop and a desktop that never answers.
	reply, err := h.localNC.Request(browserfetch.SniffSubject, data, timeout+5*time.Second)
	if err != nil {
		log.Info("Browser context unavailable for sniff", "error", err)
		h.respondWithError(msg, "browser context unavailable: "+err.Error())
		return
	}
	var resp browserfetch.SniffResponse
	if err := json.Unmarshal(reply.Data, &resp); err != nil {
		h.respondWithError(msg, "invalid sniff response")
		return
	}
	if resp.Error != "" {
		log.Info("Sniff failed", "url", req.URL, "error", resp.Error)
		h.respondWithError(msg, "sniff failed: "+resp.Error)
		return
	}
	resp.Candidates = publicCandidates(resp.Candidates)
	log.Info("Sniffed page", "url", req.URL, "candidates", len(resp.Candidates),
		"elapsed", time.Since(start).Round(time.Millisecond))
	h.sniffs.put(req.URL, resp)
	h.respondSniff(msg, resp)
}

// maxSniffCandidates bounds a report, keeping it well inside a reply.
const maxSniffCandidates = 10

// publicCandidates drops candidates on literal private addresses, which the
// service would refuse anyway, and caps the list.
func publicCandidates(in []browserfetch.SniffCandidate) []browserfetch.SniffCandidate {
	out := make([]browserfetch.SniffCandidate, 0, len(in))
	for _, c := range in {
		u, err := url.Parse(c.URL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
			continue
		}
		host := strings.ToLower(u.Hostname())
		if addr, err := netip.ParseAddr(host); (err == nil && blockedAddr(addr)) || host == "localhost" {
			continue
		}
		out = append(out, c)
		if len(out) == maxSniffCandidates {
			break
		}
	}
	return out
}

// respondSniff relays a sniff report as a scrape reply the service can tell
// from a fetched page.
func (h *scrapeHandler) respondSniff(msg *nats.Msg, resp browserfetch.SniffResponse) {
	body, err := json.Marshal(struct {
		Title      string                        `json:"title,omitempty"`
		Candidates []browserfetch.SniffCandidate `json:"candidates"`
	}{resp.Title, resp.Candidates})
	if err != nil {
		h.respondWithError(msg, "failed to marshal sniff report")
		return
	}
	h.respond(msg, scrapeResponse{
		Status: 200,
		Headers: map[string]string{
			"Content-Type":   "application/json",
			"X-Scrape-Sniff": "true",
		},
		Body: string(body),
	})
}

// sniffCache holds recent sniff reports by canonical page URL.
type sniffCache struct {
	mu      sync.Mutex
	entries map[string]sniffEntry
}

type sniffEntry struct {
	resp browserfetch.SniffResponse
	at   time.Time
}

func newSniffCache() *sniffCache {
	return &sniffCache{entries: map[string]sniffEntry{}}
}

func (c *sniffCache) get(pageURL string) (browserfetch.SniffResponse, bool) {
	key, ok := vlstate.CanonicalPageURL(pageURL)
	if !ok || c == nil {
		return browserfetch.SniffResponse{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok || time.Since(e.at) > sniffCacheTTL {
		return browserfetch.SniffResponse{}, false
	}
	return e.resp, true
}

// put caches a report that found something. One that found nothing is not
// kept, so a user retrying once the stream starts is not told it again.
func (c *sniffCache) put(pageURL string, resp browserfetch.SniffResponse) {
	key, ok := vlstate.CanonicalPageURL(pageURL)
	if !ok || c == nil || len(resp.Candidates) == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, e := range c.entries {
		if time.Since(e.at) > sniffCacheTTL {
			delete(c.entries, k)
		}
	}
	c.entries[key] = sniffEntry{resp: resp, at: time.Now()}
}
