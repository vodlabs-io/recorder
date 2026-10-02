package flv

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// renewLead is the fraction of a URL's TTL at which the next URL is fetched.
// The grant can start when the URL is minted or when a connection opens on it,
// and the first is earlier than the renewer can see, so the margin is
// generous; fetching early costs one extra resolve.
const renewLead = 0.6

// URLRenewer hands out the URL for each connection to an FLV stream whose URL
// expires. An Origin that bounds an FLV URL's life (Douyu) closes a connection
// when the grant ends and serves each URL to one connection only, so the next
// URL is fetched in the background ahead of the cut and is ready for the
// reconnect. Without a TTL or a refresh func every connection gets the same
// URL.
//
// Close must be called before the part's Record returns: the refresh func
// belongs to the session, which may only be asked while Record runs.
type URLRenewer struct {
	ttl     time.Duration
	refresh func(context.Context) (string, error)
	logger  *slog.Logger

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu sync.Mutex
	// url is the URL connections are opened on, issued when it was obtained,
	// and used whether a connection has been opened on it.
	url    string
	issued time.Time
	used   bool
	// next is a fetched URL no connection has used yet.
	next   string
	nextAt time.Time
	// inflight is closed when the running fetch finishes; nil when none runs.
	inflight chan struct{}
	timer    *time.Timer
	closed   bool
}

// NewURLRenewer starts handing out url. ttl and refresh come from the Session
// (Config.URLTTL, Config.RefreshURL).
func NewURLRenewer(
	ctx context.Context,
	url string,
	ttl time.Duration,
	refresh func(context.Context) (string, error),
	logger *slog.Logger,
) *URLRenewer {
	ctx, cancel := context.WithCancel(ctx)
	return &URLRenewer{
		ttl:     ttl,
		refresh: refresh,
		logger:  logger,
		ctx:     ctx,
		cancel:  cancel,
		url:     url,
		issued:  time.Now(),
	}
}

func (r *URLRenewer) renews() bool { return r.ttl > 0 && r.refresh != nil }

// Next returns the URL to open the next connection on: the current one if no
// connection has used it, else the one fetched ahead, else one fetched now. If
// no fresh URL can be had, the current one is returned and the connection is
// left to fail on it.
func (r *URLRenewer) Next(ctx context.Context) string {
	if !r.renews() {
		return r.url
	}
	r.mu.Lock()
	if r.used {
		// At most: wait out the background fetch, fetch once more, and wait
		// out a background fetch that started in between.
		for range 3 {
			if r.next != "" || ctx.Err() != nil {
				break
			}
			ch := r.inflight
			r.mu.Unlock()
			if ch != nil {
				select {
				case <-ch:
				case <-ctx.Done():
				}
			} else {
				r.fetch(ctx)
			}
			r.mu.Lock()
		}
		if r.next != "" {
			r.url, r.issued, r.next = r.next, r.nextAt, ""
			r.logger.Info("Switched to a fresh stream URL")
		} else {
			r.logger.Warn("No fresh stream URL; reconnecting on the spent one")
		}
	}
	r.used = true
	r.schedule()
	u := r.url
	r.mu.Unlock()
	return u
}

// MarkUsed records that the current URL already carries a connection — one a
// part resumed rather than opened.
func (r *URLRenewer) MarkUsed() {
	if !r.renews() {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.used = true
	r.schedule()
}

// schedule arms the background fetch for the current URL. Called with mu held.
func (r *URLRenewer) schedule() {
	if r.closed {
		return
	}
	if r.timer != nil && r.timer.Stop() {
		r.wg.Done()
	}
	d := time.Until(r.issued.Add(time.Duration(float64(r.ttl) * renewLead)))
	r.wg.Add(1)
	r.timer = time.AfterFunc(max(d, 0), func() {
		defer r.wg.Done()
		r.fetch(r.ctx)
	})
}

// fetch obtains the next URL unless one is already held or being fetched.
func (r *URLRenewer) fetch(ctx context.Context) {
	r.mu.Lock()
	if r.next != "" || r.inflight != nil || r.closed {
		r.mu.Unlock()
		return
	}
	done := make(chan struct{})
	r.inflight = done
	current := r.url
	r.mu.Unlock()

	u, err := r.refresh(ctx)

	r.mu.Lock()
	switch {
	case err != nil:
		r.logger.Warn("Failed to fetch a fresh stream URL", "error", err)
	case u == current:
		// The re-resolve answered with the URL in use, which is spent.
		r.logger.Warn("Re-resolve returned the stream URL already in use")
	default:
		r.next, r.nextAt = u, time.Now()
		r.logger.Debug("Fetched the next stream URL")
	}
	r.inflight = nil
	close(done)
	r.mu.Unlock()
}

// Close stops the background fetch and waits for one that is running.
func (r *URLRenewer) Close() {
	r.cancel()
	r.mu.Lock()
	r.closed = true
	if r.timer != nil && r.timer.Stop() {
		r.wg.Done()
	}
	r.mu.Unlock()
	r.wg.Wait()
}
