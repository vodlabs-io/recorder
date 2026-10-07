package cloud

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/vodlabs-io/recorder/internal/platformlogin"
	"github.com/vodlabs-io/recorder/internal/streamer"
	"github.com/vodlabs-io/recorder/internal/vlconfig"
	"github.com/vodlabs-io/recorder/internal/vlstate"
)

// platformLimiter paces one platform's requests, so a burst against one
// platform never spends another's allowance; a daemon-wide ceiling still bounds
// the total. Status checks and mid-recording re-signs draw on separate budgets,
// so a re-sign never waits behind status checks until its grant has expired.
type platformLimiter struct {
	platform string
	local    bool                      // a local platform's checks run on this machine, so its lanes never wait for the cloud
	highPri  chan streamer.StreamerRef // status: something is waiting on this one
	authPri  chan streamer.StreamerRef // status: authenticated re-check of a restricted streamer
	lowPri   chan streamer.StreamerRef // status: backlog
	stream   chan streamer.StreamerRef // re-signs for live recordings

	mu      sync.Mutex   // Guards the buckets, which are replaced when a pace is retuned
	status  *tokenBucket // status checks
	streams *tokenBucket // re-signs

	// What the lanes have learned about the path to the cloud, and how long
	// they are holding off while it is broken. Guarded apart from the buckets:
	// a lane holding off must not stand in the way of a retune.
	pauseMu  sync.Mutex
	fails    int           // consecutive failed fetches, counted across both lanes
	cooldown time.Duration // the current hold, doubled each time it is extended
	until    time.Time     // lanes hold until this
	resumed  chan struct{} // closed to release a hold early
}

// A platform's lanes hold off once a first attempt and all its retries have
// failed: by then the path to the cloud is broken, and since this daemon serves
// the scrapes behind its own checks, retrying would only add load to an outage.
var laneFailureThreshold = statusFetchRetries + 1

// The hold doubles while the failures continue, so a blip costs one short wait
// and an outage settles at one probe every few minutes. The cap is only what an
// outage costs when nothing announces its end: a reconnect releases the hold.
const (
	laneHoldBase = 30 * time.Second
	laneHoldMax  = 5 * time.Minute
)

// noteFailure records a failed fetch, reporting the hold it started or extended
// and whether the platform is holding off at all. A caller that is must not
// retry — the retries are the storm.
func (l *platformLimiter) noteFailure(now time.Time) (started time.Duration, holding bool) {
	l.pauseMu.Lock()
	defer l.pauseMu.Unlock()

	l.fails++
	switch {
	case l.fails < laneFailureThreshold:
		return 0, false
	case now.Before(l.until):
		return 0, true // the other lane's probe already extended the hold
	}
	l.cooldown = min(max(2*l.cooldown, laneHoldBase), laneHoldMax)
	l.until = now.Add(l.cooldown)
	return l.cooldown, true
}

// noteSuccess ends a run of failures, reporting whether it released a hold so
// the recovery is logged once instead of by every fetch behind it.
func (l *platformLimiter) noteSuccess() (recovered bool) {
	l.pauseMu.Lock()
	defer l.pauseMu.Unlock()
	recovered = l.fails >= laneFailureThreshold
	l.fails, l.cooldown, l.until = 0, 0, time.Time{}
	return recovered
}

// hold reports how long this lane waits before its next fetch, along with the
// channel that releases it early.
func (l *platformLimiter) hold(now time.Time) (time.Duration, <-chan struct{}) {
	l.pauseMu.Lock()
	defer l.pauseMu.Unlock()
	if now.Before(l.until) {
		return l.until.Sub(now), l.resumed
	}
	return 0, l.resumed
}

// resume ends a hold and wakes the lanes waiting it out. The channel is
// replaced rather than reused: it is the wake-up for one hold, and a later one
// needs its own.
func (l *platformLimiter) resume() {
	l.pauseMu.Lock()
	defer l.pauseMu.Unlock()
	if l.until.IsZero() {
		return
	}
	l.fails, l.cooldown, l.until = 0, 0, time.Time{}
	close(l.resumed)
	l.resumed = make(chan struct{})
}

// tokenBucket paces one class of request: burst tokens available at once, one
// more every interval. Retiring one closes retired as well as stopping its
// refiller, so anything already waiting on it is released rather than left
// waiting on a bucket nothing will ever put a token into again.
type tokenBucket struct {
	tokens  chan time.Time
	cfg     vlconfig.RateLimit
	retired chan struct{}
	cancel  context.CancelFunc
}

func (b *tokenBucket) retire() {
	b.cancel()
	close(b.retired)
}

// bucket returns the budget a request of this kind draws on.
func (l *platformLimiter) bucket(kind requestKind) *tokenBucket {
	l.mu.Lock()
	defer l.mu.Unlock()
	if kind == kindStream {
		return l.streams
	}
	return l.status
}

// retune replaces any bucket whose configured pace has changed, so a limit the
// cloud retunes takes effect on a running daemon rather than at the next
// restart. The lanes and everything queued in them are kept: only the budget
// they are served from is swapped.
func (l *platformLimiter) retune(ctx context.Context, log *slog.Logger, status, stream vlconfig.RateLimit) {
	l.mu.Lock()
	defer l.mu.Unlock()

	for _, swap := range []struct {
		name string
		into **tokenBucket
		cfg  vlconfig.RateLimit
	}{
		{"status", &l.status, status},
		{"stream", &l.streams, stream},
	} {
		if (*swap.into).cfg == swap.cfg {
			continue
		}
		log.Info("Retuned platform rate limit", "platform", l.platform, "budget", swap.name,
			"burst", swap.cfg.Burst, "interval", swap.cfg.Interval)
		(*swap.into).retire()
		*swap.into = newTokenBucket(ctx, swap.cfg)
	}
}

// laneDepth is how many pending requests one of a platform's lanes holds,
// matching the daemon-wide channels that feed it.
const laneDepth = streamer.MaxSubscribed

// ceilingLimit bounds all outgoing requests after each platform's own token,
// since per-platform budgets sum to more than the cloud should see at once.
var ceilingLimit = vlconfig.RateLimit{Burst: 10, Interval: 500 * time.Millisecond}

// scrapeBudgetPerMinute is what the scrape handler admits in a minute. Each
// check the lanes send comes back as a scrape, so the budget matches the most
// the ceiling can emit; any lower would refuse the daemon's own checks.
var scrapeBudgetPerMinute = ceilingLimit.Burst + int(time.Minute/ceilingLimit.Interval)

// requestKind names which of a platform's budgets a request draws on, and how
// the request is made.
type requestKind int

const (
	kindStatus requestKind = iota // a plain status check
	kindAuthed                    // an authenticated re-check of a restricted streamer
	kindStream                    // a re-sign of a live recording's stream URLs
)

func (k requestKind) String() string {
	switch k {
	case kindAuthed:
		return "authed"
	case kindStream:
		return "stream"
	default:
		return "status"
	}
}

// rateLimitStatusRequests routes outgoing streamer requests to their platform's
// limiter. It never blocks, neither on a platform (which would stall all the
// others) nor on a live subscription (which would stop the only drain of the
// daemon-wide queues); the lane servers wait for the subscription instead.
func (c *Client) rateLimitStatusRequests(ctx context.Context) {
	log := c.logger.With("func", "rateLimitStatusRequests")
	log.Info("Started rate limiter", "ceiling", ceilingLimit)
	defer log.Info("Stopped rate limiter")

	for {
		id, kind, backlog := waitForRequest(ctx, c.StatusReqPriCh, c.authStatusReqCh, c.StreamReqCh, c.statusReqCh)
		if ctx.Err() != nil || id == (streamer.StreamerRef{}) {
			return // Context canceled
		}

		l := c.limiterFor(ctx, id.Platform)
		lane := l.highPri
		switch {
		case kind == kindAuthed:
			lane = l.authPri
		case kind == kindStream:
			lane = l.stream
		case backlog:
			// Keep the daemon-wide split intact per platform: a batch that
			// arrived as backlog must not land in the lane a single urgent
			// check is served from, or it waits behind the whole batch.
			lane = l.lowPri
		}
		select {
		case lane <- id:
		default:
			// The lane already holds a full backlog for this one platform.
			// Dropping is what the producers do when their own queues fill
			// (see requestFreshStreams, handleFetchResult); blocking here
			// would hold up every other platform behind this one.
			log.Warn("Platform request lane is full, dropping request",
				"platform", id.Platform, "id", id, "kind", kind)
		}
	}
}

// limiterFor returns the platform's limiter, starting it on first use and
// retuning it when the pace from the heartbeat has changed. A URL-only lookup
// has no platform yet and shares the empty-name limiter at the default pace.
func (c *Client) limiterFor(ctx context.Context, platform string) *platformLimiter {
	c.limiterMu.Lock()
	defer c.limiterMu.Unlock()

	status := vlconfig.StatusLimitFor(platform)
	stream := vlconfig.StreamLimitFor(platform)

	if l, ok := c.limiters[platform]; ok {
		l.retune(ctx, c.logger.With("func", "limiterFor"), status, stream)
		return l
	}
	l := &platformLimiter{
		platform: platform,
		local:    vlconfig.IsLocal(platform),
		highPri:  make(chan streamer.StreamerRef, laneDepth),
		authPri:  make(chan streamer.StreamerRef, laneDepth),
		lowPri:   make(chan streamer.StreamerRef, laneDepth),
		stream:   make(chan streamer.StreamerRef, laneDepth),
		status:   newTokenBucket(ctx, status),
		streams:  newTokenBucket(ctx, stream),
		resumed:  make(chan struct{}),
	}
	if c.limiters == nil {
		c.limiters = make(map[string]*platformLimiter)
	}
	c.limiters[platform] = l

	c.logger.Info("Started platform rate limiter", "platform", platform,
		"status_burst", status.Burst, "status_interval", status.Interval,
		"stream_burst", stream.Burst, "stream_interval", stream.Interval)

	// Both lanes wait on the subscribed flag through a sync.Cond, which cannot
	// select on a context. Waking them belongs to whatever started them: one
	// broadcast when this limiter's context ends lets them re-check and return
	// rather than sleep on a flag no resubscribe is left to set.
	go func() {
		<-ctx.Done()
		c.cond.Broadcast()
	}()

	// Two goroutines, so the two budgets are genuinely independent: a status
	// check waiting for a token must not hold up a re-sign that has one.
	go c.runStatusLane(ctx, l)
	go c.runStreamLane(ctx, l)
	return l
}

// runStatusLane serves a platform's status checks, keeping the priority order
// between them: a check someone is waiting on, then an authed re-check, then
// the backlog.
func (c *Client) runStatusLane(ctx context.Context, l *platformLimiter) {
	log := c.logger.With("func", "runStatusLane", "platform", l.platform)
	for {
		// Gate before taking rather than after: a request held back stays in
		// its lane, so an urgent check arriving during the wait is still served
		// ahead of a backlog that was queued first.
		if !l.local && !c.waitSubscribeCtx(ctx) {
			return
		}
		id, kind, _ := waitForRequest(ctx, l.highPri, l.authPri, nil, l.lowPri)
		if ctx.Err() != nil || id == (streamer.StreamerRef{}) {
			return
		}
		c.serve(ctx, log, l, id, kind)
	}
}

// runStreamLane serves a platform's mid-recording re-signs from their own
// budget.
func (c *Client) runStreamLane(ctx context.Context, l *platformLimiter) {
	log := c.logger.With("func", "runStreamLane", "platform", l.platform)
	for {
		if !l.local && !c.waitSubscribeCtx(ctx) {
			return
		}
		select {
		case id := <-l.stream:
			c.serve(ctx, log, l, id, kindStream)
		case <-ctx.Done():
			return
		}
	}
}

// serve spends a token from the platform's bucket and one from the daemon-wide
// ceiling, then makes the request. Taking them in that order means a platform
// held up by the ceiling delays only itself.
func (c *Client) serve(
	ctx context.Context,
	log *slog.Logger,
	l *platformLimiter,
	id streamer.StreamerRef,
	kind requestKind,
) {
	if !c.waitHold(ctx, l) {
		return
	}
	if !c.takeTokens(ctx, l, kind) {
		return
	}
	// Detached so the lane can move on to the next request, but tracked: a
	// fetch carries on writing view docs and dispatching after it returns, and
	// nothing could otherwise tell whether any were still doing so.
	c.fetchWg.Go(func() {
		var str streamer.StreamerState
		var err error
		if l.local {
			str, err = c.checkLocal(ctx, id, kind == kindStream)
		} else {
			str, err = c.fetchSingleStatus(ctx, id, kind == kindAuthed, kind == kindStream)
		}
		if err != nil {
			if started, holding := l.noteFailure(time.Now()); holding {
				logHold(log, id, kind, started, err)
				return
			}
			if errors.Is(err, ErrCheckNotRunnable) {
				// Park it for a desktop app to release, and set it aside for
				// the login sweep so it doesn't spend the platform's allowance.
				c.parked.park(id)
				c.sweep.noteUnrunnable(id.StreamerKey, time.Now())
				log.Info("Cloud cannot run this check yet; parked for a browser context",
					"id", id, "kind", kind, "reason", err)
				return
			}
			log.Warn("Failed to fetch status", "id", id, "kind", kind, "error", err)
			c.retryStatusFetch(ctx, log, l, id, kind)
			return
		}
		if l.noteSuccess() {
			log.Info("Platform checks recovered", "id", id, "kind", kind)
		}
		c.logStreamers(streamer.List{str})
		c.handleFetchResult(ctx, str, kind == kindAuthed)
	})
}

// waitHold keeps a lane back for as long as its platform is holding off,
// reporting false when the daemon stopped first.
func (c *Client) waitHold(ctx context.Context, l *platformLimiter) bool {
	for {
		wait, resumed := l.hold(time.Now())
		if wait == 0 {
			return true
		}
		timer := time.NewTimer(wait)
		select {
		case <-timer.C:
		case <-resumed:
			timer.Stop()
		case <-ctx.Done():
			timer.Stop()
			return false
		}
	}
}

// logHold warns once as a hold starts or is extended, logging the failures
// behind it at debug.
func logHold(log *slog.Logger, id streamer.StreamerRef, kind requestKind, started time.Duration, err error) {
	if started == 0 {
		log.Debug("Fetch failed while holding off", "id", id, "kind", kind, "error", err)
		return
	}
	log.Warn("Holding off platform checks after repeated failures",
		"id", id, "kind", kind, "hold", started, "error", err)
}

// resumeLanes releases every platform's hold, on a reconnect: the path to the
// cloud is back.
func (c *Client) resumeLanes() {
	c.limiterMu.Lock()
	defer c.limiterMu.Unlock()
	for _, l := range c.limiters {
		l.resume()
	}
}

// takeTokens waits for a token from the platform's budget for this kind of
// request and then from the daemon-wide ceiling, reporting false when the
// context was canceled before both were in hand.
//
// Waiting on the bucket's retirement as well lets a caller pick up the
// replacement bucket when a retune happens mid-wait.
func (c *Client) takeTokens(ctx context.Context, l *platformLimiter, kind requestKind) bool {
	for {
		b := l.bucket(kind)
		select {
		case <-b.tokens:
		case <-b.retired:
			continue // retuned mid-wait; take the replacement's budget instead
		case <-ctx.Done():
			return false
		}
		break
	}
	select {
	case <-c.ceiling.tokens:
	case <-ctx.Done():
		return false
	}
	return true
}

// waitFetches blocks until every status fetch serve has detached has finished,
// which can be after their context ends. Anything that rewrites the state they
// read (e.g. a test swapping the cloud endpoint) must wait here first.
func (c *Client) waitFetches() { c.fetchWg.Wait() }

// statusFetchRetries and statusFetchRetryBackoff bound the recovery below:
// attempt N waits N*backoff, so a fetch is given up on after roughly 30s.
var (
	statusFetchRetries      = 3
	statusFetchRetryBackoff = 5 * time.Second
)

// retryStatusFetch re-attempts a status fetch that failed in transit. The cloud
// pushes an update only when a status changes, so a dropped fetch would strand
// an already-live streamer without a recording. Retries spend tokens like first
// attempts and stop once the platform starts holding off.
func (c *Client) retryStatusFetch(
	ctx context.Context,
	log *slog.Logger,
	l *platformLimiter,
	id streamer.StreamerRef,
	kind requestKind,
) {
	for attempt := 1; attempt <= statusFetchRetries; attempt++ {
		select {
		case <-time.After(time.Duration(attempt) * statusFetchRetryBackoff):
		case <-ctx.Done():
			return
		}
		if !c.takeTokens(ctx, l, kind) {
			return
		}

		str, err := c.fetchSingleStatus(ctx, id, kind == kindAuthed, kind == kindStream)
		if err != nil {
			if started, holding := l.noteFailure(time.Now()); holding {
				logHold(log, id, kind, started, err)
				return
			}
			log.Debug("Status fetch retry failed",
				"id", id, "kind", kind, "attempt", attempt, "error", err)
			continue
		}
		l.noteSuccess()
		log.Info("Status fetch recovered", "id", id, "kind", kind, "attempt", attempt)
		c.logStreamers(streamer.List{str})
		c.handleFetchResult(ctx, str, kind == kindAuthed)
		return
	}
	log.Error("Giving up on status fetch",
		"id", id, "kind", kind, "attempts", statusFetchRetries)
}

// handleFetchResult routes a completed on-demand status fetch. A plain fetch
// that hit a login wall while a login for the platform is stored is escalated
// to an authenticated re-check instead of being dispatched. Only non-authed
// results escalate, so a still-restricted authed result cannot loop.
func (c *Client) handleFetchResult(ctx context.Context, str streamer.StreamerState, authed bool) {
	if authed {
		// Every authenticated check, whoever asked for it, counts against the
		// login sweep's schedule, computed from what the check found.
		c.sweep.noteChecked(str.StreamerKey, vlstate.StreamerDoc{
			Status:           str.Status,
			LastLiveStart:    str.LastLiveStart,
			LastLiveDuration: str.LastLiveDuration,
		}, time.Now())
		c.applyAuthedResult(ctx, str)
	} else if str.Status == streamer.StatusRestricted && c.cookies != nil && c.cookies.Has(str.Platform) {
		select {
		case c.authStatusReqCh <- str.StreamerRef:
			// Record the wall (doc only, no dispatch) so a re-check that never
			// lands still leaves nudgeRestrictedStreamers something to find.
			c.recordWall(ctx, str)
			return // the authed re-check's result will dispatch instead
		default:
			// Queue full — dispatch the restricted state rather than block.
		}
	}
	c.dispatchStreamer(ctx, str)
}

// applyAuthedResult updates a stored login's health from the outcome of an
// authenticated re-check and republishes its account status doc. A rejected
// login (StatusAuthRequired) flips it to expired ("sign in again"); a successful
// live check confirms it healthy. A still-restricted result leaves health
// untouched (the usual cause is a stale jar, not a dead login) but asks a
// running desktop app to refresh the jar. Other outcomes (errors) change
// nothing.
func (c *Client) applyAuthedResult(ctx context.Context, str streamer.StreamerState) {
	if c.cookies == nil {
		return
	}
	log := c.logger.With("func", "applyAuthedResult", "platform", str.Platform, "status", str.Status)
	now := time.Now().Unix()
	switch str.Status {
	case streamer.StatusAuthRequired:
		if err := c.cookies.SetHealth(str.Platform, false, now); err != nil {
			log.Warn("Failed to mark login expired", "error", err)
		}
	case streamer.StatusLive:
		if err := c.cookies.SetHealth(str.Platform, true, now); err != nil {
			log.Warn("Failed to mark login healthy", "error", err)
		}
	case streamer.StatusRestricted:
		// The check ran with cookies attached and the room stayed locked. The
		// dominant cause is a stale jar (short-lived tokens stopped rotating
		// since capture) — ask a running desktop app to refresh it. The
		// desktop's re-store nudges walled streamers, closing the loop.
		c.requestLoginRefresh(str.Platform)
		return
	default:
		return
	}
	// Republish the non-secret account status so the Accounts UI updates.
	if sess, ok := c.cookies.Get(str.Platform); ok {
		if err := platformlogin.Put(ctx, c.kv, sess.Status()); err != nil {
			log.Warn("Failed to publish account status", "error", err)
		}
	}
}

// loginRefreshCooldown bounds how often the daemon asks the desktop to refresh
// one platform's login, no matter how many restricted streamers are retrying.
const loginRefreshCooldown = 3 * time.Minute

// requestLoginRefresh publishes a fire-and-forget refresh request for the
// platform's login on LOCAL NATS (see platformlogin.RefreshRequestSubject). A desktop
// app picks it up if one is running; headless daemons simply have no listener.
// No-op when no login is stored: an authed check completing after a sign-out
// must not ask the desktop to refresh (and thereby re-store) a removed login.
func (c *Client) requestLoginRefresh(platform string) {
	if c.cookies == nil || !c.cookies.Has(platform) {
		return
	}
	c.refreshReqMu.Lock()
	if t, ok := c.refreshReqAt[platform]; ok && time.Since(t) < loginRefreshCooldown {
		c.refreshReqMu.Unlock()
		return
	}
	if c.refreshReqAt == nil {
		c.refreshReqAt = make(map[string]time.Time)
	}
	c.refreshReqAt[platform] = time.Now()
	c.refreshReqMu.Unlock()

	payload, err := json.Marshal(platformlogin.RefreshRequest{Platform: platform})
	if err != nil {
		return
	}
	if err := c.localNC.Publish(platformlogin.RefreshRequestSubject, payload); err != nil {
		c.logger.Warn("Failed to publish login refresh request", "platform", platform, "error", err)
		return
	}
	c.logger.Info("Requested login refresh from desktop", "platform", platform)
}

// newTokenBucket returns a bucket paced by cfg with its refiller already
// running. It starts full, so a daemon that has been idle answers the first few
// requests without waiting.
func newTokenBucket(ctx context.Context, cfg vlconfig.RateLimit) *tokenBucket {
	ctx, cancel := context.WithCancel(ctx)
	tokens := make(chan time.Time, cfg.Burst)
	for range cfg.Burst {
		tokens <- time.Now()
	}
	go refillTokenBucket(ctx, tokens, cfg.Interval)
	return &tokenBucket{
		tokens:  tokens,
		cfg:     cfg,
		retired: make(chan struct{}),
		cancel:  cancel,
	}
}

// refillTokenBucket refills the tokenBucket at the specified interval
func refillTokenBucket(ctx context.Context, tokenBucket chan time.Time, interval time.Duration) {
	for {
		select {
		case t := <-time.After(interval):
			select {
			case tokenBucket <- t:
			default:
			}
		case <-ctx.Done():
			return
		}
	}
}

// waitForRequest takes the next request, preferring the high-priority,
// authenticated and re-sign channels over the low-priority backlog. It reports
// the request's kind and whether it came from the backlog. A caller with fewer
// lanes passes nil for the rest. An empty ID means the context was canceled.
func waitForRequest(
	ctx context.Context,
	highPri <-chan streamer.StreamerRef,
	authPri <-chan streamer.StreamerRef,
	streamPri <-chan streamer.StreamerRef,
	lowPri <-chan streamer.StreamerRef,
) (streamer.StreamerRef, requestKind, bool) {
	select {
	case id := <-highPri:
		return id, kindStatus, false
	case id := <-authPri:
		return id, kindAuthed, false
	case id := <-streamPri:
		return id, kindStream, false
	case <-ctx.Done():
		return streamer.StreamerRef{}, kindStatus, false
	default:
		select {
		case id := <-highPri:
			return id, kindStatus, false
		case id := <-authPri:
			return id, kindAuthed, false
		case id := <-streamPri:
			return id, kindStream, false
		case id := <-lowPri:
			return id, kindStatus, true
		case <-ctx.Done():
			return streamer.StreamerRef{}, kindStatus, false
		}
	}
}
