package webpage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/vodlabs-io/recorder/internal/browserfetch"
	"github.com/vodlabs-io/recorder/internal/netguard"
	"github.com/vodlabs-io/recorder/internal/stream"
	"github.com/vodlabs-io/recorder/internal/streamer"
)

const (
	// sniffTimeout bounds a page load in the desktop app.
	sniffTimeout = 20 * time.Second
	// cacheTTL is how long a finding answers for its page, so an add's
	// preflight and the recording's first check cost one page load. A re-sign
	// never reads it.
	cacheTTL = 60 * time.Second
	// maxSniffCandidates bounds how much of a sniff report is read.
	maxSniffCandidates = 10
	// maxPages bounds how many pages are remembered at once.
	maxPages = 100
)

// ErrNotLive refuses a page with nothing live on it.
var ErrNotLive = errors.New("nothing is live there right now")

// Resolver finds what plays on the pages this machine's user asked to record.
// It implements cloud.LocalSource.
type Resolver struct {
	nc     *nats.Conn   // the daemon's LOCAL NATS: sniffs go to the desktop app over it
	client *http.Client // reads pages and probes streams; public addresses only
	logger *slog.Logger

	mu    sync.Mutex
	pages map[string]*url.URL // username -> the page it was added from
	cache map[string]cached   // canonical page URL -> recent finding
	// sniffMu runs one sniff at a time: the desktop has one browser for it.
	sniffMu sync.Mutex
}

type cached struct {
	f  finding
	at time.Time
}

// New returns a Resolver that sniffs through the desktop app reachable on nc,
// when one is.
func New(nc *nats.Conn, logger *slog.Logger) *Resolver {
	return &Resolver{
		nc:     nc,
		client: guardedClient(),
		logger: logger,
		pages:  map[string]*url.URL{},
		cache:  map[string]cached{},
	}
}

// guardedClient reads pages and probes streams, refusing every address that
// is not public internet, including on a redirect.
func guardedClient() *http.Client {
	t := http.DefaultTransport.(*http.Transport).Clone()
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second, Control: netguard.DialControl}
	t.DialContext = dialer.DialContext
	return &http.Client{
		Transport: t,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("stopped after 5 redirects")
			}
			if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
				return fmt.Errorf("redirect to %s refused", req.URL.Scheme)
			}
			return nil
		},
	}
}

// Preflight checks a URL the user typed and returns the streamer it names,
// remembering the page for the recording's checks. Anything short of a live
// stream refuses it: ErrNotLive when the page plays nothing now, another error
// when it could not be read.
func (r *Resolver) Preflight(ctx context.Context, rawURL string) (streamer.StreamerKey, error) {
	page, err := Canonical(rawURL)
	if err != nil {
		return streamer.StreamerKey{}, err
	}
	f, err := r.find(ctx, page, true)
	if err != nil {
		return streamer.StreamerKey{}, err
	}
	if !f.live {
		return streamer.StreamerKey{}, ErrNotLive
	}
	key := streamer.StreamerKey{Platform: Platform, Username: slugOf(page)}
	r.mu.Lock()
	if len(r.pages) >= maxPages {
		r.mu.Unlock()
		return streamer.StreamerKey{}, errors.New("too many pages are being recorded already")
	}
	r.pages[key.Username] = page
	r.mu.Unlock()
	return key, nil
}

// KeyFor returns the streamer a URL names, without reading the page.
func KeyFor(rawURL string) (streamer.StreamerKey, bool) {
	slug, err := Slug(rawURL)
	if err != nil {
		return streamer.StreamerKey{}, false
	}
	return streamer.StreamerKey{Platform: Platform, Username: slug}, true
}

// Forget drops a removed streamer's page.
func (r *Resolver) Forget(key streamer.StreamerKey) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if page, ok := r.pages[key.Username]; ok {
		delete(r.cache, page.String())
		delete(r.pages, key.Username)
	}
}

// Check reports what plays on the streamer's page now. A streamer whose page
// this daemon was never given (it was added before a restart) reads offline.
func (r *Resolver) Check(ctx context.Context, key streamer.StreamerKey, fresh bool) (streamer.StreamerState, error) {
	str := streamer.StreamerState{StreamerRef: streamer.StreamerRef{StreamerKey: key}, Status: streamer.StatusOffline}
	str.LiveMetadata.Platform = Platform
	r.mu.Lock()
	page := r.pages[key.Username]
	r.mu.Unlock()
	if page == nil {
		r.logger.Info("No page on record for this streamer; it reads offline", "streamer", key)
		return str, nil
	}
	f, err := r.find(ctx, page, !fresh)
	if err != nil {
		return streamer.StreamerState{}, err
	}
	str.DisplayName = cleanLabel(f.siteName)
	if str.DisplayName == "" {
		str.DisplayName = siteName(page)
	}
	str.LiveMetadata.Title = cleanLabel(f.title)
	if f.live {
		str.Status = streamer.StatusLive
		str.Streams = stream.List{{URL: f.stream, QualityID: "live", Format: f.format}}
		str.Headers = f.headers
	}
	return str, nil
}

// find reads the page: a sniff when the desktop app is there, the page's HTML
// when it is not or the sniff saw nothing.
func (r *Resolver) find(ctx context.Context, page *url.URL, useCache bool) (finding, error) {
	key := page.String()
	if useCache {
		r.mu.Lock()
		c, ok := r.cache[key]
		r.mu.Unlock()
		if ok && time.Since(c.at) < cacheTTL {
			return c.f, nil
		}
	}
	// A sniff that returns at all loaded the page, so finding nothing there is
	// an answer; the HTML is still read, for a player the browser left idle.
	f, err := r.sniff(ctx, page)
	if err != nil {
		r.logger.Info("No browser sniff; reading the page's HTML", "host", page.Host, "reason", err)
	}
	if !f.live {
		static, serr := readStatic(ctx, r.client, page, r.logger)
		switch {
		case serr == nil && (static.live || err != nil):
			if static.title == "" {
				static.title = f.title
			}
			f = static
		case serr != nil && err != nil:
			return finding{}, serr
		}
	}
	r.mu.Lock()
	for k, c := range r.cache {
		if time.Since(c.at) >= cacheTTL {
			delete(r.cache, k)
		}
	}
	r.cache[key] = cached{f: f, at: time.Now()}
	r.mu.Unlock()
	r.logger.Info("Checked page", "host", page.Host, "live", f.live, "format", f.format)
	return f, nil
}

// sniff asks the desktop app to load the page and report what its player
// fetched. The streams are not probed: the browser just played them from this
// same machine.
func (r *Resolver) sniff(ctx context.Context, page *url.URL) (finding, error) {
	if r.nc == nil {
		return finding{}, errors.New("no local connection")
	}
	data, err := json.Marshal(browserfetch.SniffRequest{URL: page.String(), TimeoutMS: int(sniffTimeout / time.Millisecond)})
	if err != nil {
		return finding{}, err
	}
	r.sniffMu.Lock()
	defer r.sniffMu.Unlock()
	// The desktop's own deadline is the page load's; this one also covers the
	// hop and a desktop that never answers.
	ctx, cancel := context.WithTimeout(ctx, sniffTimeout+5*time.Second)
	defer cancel()
	msg, err := r.nc.RequestWithContext(ctx, browserfetch.SniffSubject, data)
	if err != nil {
		return finding{}, err
	}
	var resp browserfetch.SniffResponse
	if err := json.Unmarshal(msg.Data, &resp); err != nil {
		return finding{}, fmt.Errorf("unreadable sniff report: %w", err)
	}
	if resp.Error != "" {
		return finding{}, errors.New(resp.Error)
	}
	return fromSniff(page, resp), nil
}

// fromSniff turns a desktop report into a finding: live on its first usable
// candidate.
func fromSniff(page *url.URL, resp browserfetch.SniffResponse) finding {
	f := finding{title: resp.Title}
	for i, c := range resp.Candidates {
		if i == maxSniffCandidates {
			break
		}
		stream, ok := streamURL(c.URL)
		if !ok {
			continue
		}
		format := c.Format
		if format != formatHLS && format != formatFLV {
			format = formatOf(c.URL)
		}
		if format == "" {
			continue
		}
		frame, err := url.Parse(c.Frame)
		if err != nil || (frame.Scheme != "http" && frame.Scheme != "https") {
			frame = page
		}
		f.live, f.stream, f.format, f.headers = true, stream.String(), format, refererHeaders(frame)
		return f
	}
	return f
}
