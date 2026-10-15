package cloud

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"time"

	"github.com/vodlabs-io/recorder/internal/streamer"
	"github.com/vodlabs-io/recorder/internal/vlconfig"
	"github.com/vodlabs-io/recorder/internal/vlstate"
)

// definitiveRejections picks out the URLs the cloud settled, which are the only
// ones safe to delete. A rejection it could not settle (the resolver was down,
// the platform did not answer) leaves the entry in place to be asked about
// again — the alternative is an outage quietly emptying someone's config.
func definitiveRejections(rejected []subscribeRejection) []string {
	var urls []string
	for _, r := range rejected {
		if r.URL != "" && r.Definitive() {
			urls = append(urls, r.URL)
		}
	}
	return urls
}

// subscribeRequest is the request body for POST /api/v1/auth/subscribe
type subscribeRequest struct {
	StreamerIDs  streamer.StreamerRefs `json:"streamer_ids"`
	SharedChecks bool                  `json:"shared_checks"`
}

// subscribeResponse is the response for POST /api/v1/auth/subscribe
type subscribeResponse struct {
	UserJWT        string                `json:"user_jwt"`        // JWT for authenticating with NATS and REST API
	ExpiresAt      int64                 `json:"expires_at"`      // Unix timestamp for when the JWT expires
	ConsumerName   string                `json:"consumer_name"`   // JetStream consumer name for notifications
	StreamerIDs    streamer.StreamerRefs `json:"streamer_ids"`    // List of streamers the user is subscribed to
	SubscriptionID string                `json:"subscription_id"` // Unique ID for this subscription
	// Rejected explains what is missing from StreamerIDs.
	Rejected []subscribeRejection `json:"rejected,omitempty"`
}

// A subscribeRejection is the cloud declining one entry of a subscription:
// usually a hand-written URL entry that names no streamer it can find.
type subscribeRejection struct {
	URL      string `json:"url,omitempty"`
	Platform string `json:"platform,omitempty"`
	// Username names the streamer for a rejection that is not about a URL,
	// such as an entry dropped for want of room.
	Username string `json:"username,omitempty"`
	Error    string `json:"error"`
	Message  string `json:"message"`
}

// Definitive reports whether the cloud judged the entry itself. Only a
// definitive rejection may delete the entry, so neither an outage nor a full
// subscription can delete a streamer.
func (r subscribeRejection) Definitive() bool {
	switch r.Error {
	case codeUnavailable, codeOverCapacity:
		return false
	}
	return true
}

// name identifies the rejected entry for a log line: the streamer if the cloud
// named one, the URL otherwise.
func (r subscribeRejection) name() string {
	if r.Platform != "" && r.Username != "" {
		return r.Platform + "." + r.Username
	}
	return r.URL
}

// subscribe gets a new JWT and consumer for the current streamer IDs and updates client state
func (c *Client) subscribe(ctx context.Context, strIDs streamer.StreamerRefs) (streamer.StreamerRefs, []subscribeRejection, error) {
	log := c.logger.With("func", "subscribe")

	reqBody := subscribeRequest{
		StreamerIDs:  strIDs,
		SharedChecks: c.sharedChecks,
	}

	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return nil, nil, err
	}

	log.Debug("Subscribing to streamers", "ids", strIDs)

	req, err := http.NewRequestWithContext(ctx,
		http.MethodPost,
		apiBaseURL+subscribeEndpoint,
		bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Timestamp", fmt.Sprintf("%d", time.Now().Unix()))
	c.setClientHeaders(req)
	req.Header.Set("X-Client-ID", c.clientID)

	if err := c.signRequest(req, bodyBytes); err != nil {
		return nil, nil, fmt.Errorf("failed to sign request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("subscribe request failed: %w", err)
	}
	defer resp.Body.Close()
	c.noteResponse(ctx, resp)

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, nil, &subscribeRefused{status: resp.StatusCode, reason: resp.Status, body: string(body)}
	}

	var result subscribeResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, nil, fmt.Errorf("failed to parse response: %w", err)
	}

	c.mu.Lock()
	c.userJWT = result.UserJWT
	c.jwtExpiresAt = time.Unix(result.ExpiresAt, 0)
	c.consumerName = result.ConsumerName
	c.subscriptionID = result.SubscriptionID
	c.mu.Unlock()

	c.setSubscribed(true)

	log.Info("Subscribed",
		"count", len(result.StreamerIDs),
		"rejected", len(result.Rejected),
		"consumer", result.ConsumerName,
		"shared_checks", c.sharedChecks,
		"expires", time.Unix(result.ExpiresAt, 0).Format(time.RFC3339),
		"subscription_id", result.SubscriptionID)

	return result.StreamerIDs, result.Rejected, nil
}

// subscribeMinInterval is the minimum spacing between subscribe requests. A burst
// of mutations collapses into a single, rate-safe subscription that reflects
// the final config, instead of two back-to-back calls that trip the rate limit.
const subscribeMinInterval = 1500 * time.Millisecond

// How long the handler holds off before retrying a subscribe that failed,
// doubling with each consecutive failure. The endpoint is rate-limited per
// address, so retrying faster than its budget refills would lock the daemon out.
const (
	subscribeRetryBase = 10 * time.Second
	subscribeRetryMax  = 10 * time.Minute
	// The floor for a refusal the cloud reasoned about. Re-sending an
	// unacceptable request unchanged cannot make it acceptable, and it costs
	// the same budget as a useful call.
	subscribeRefusedFloor = time.Minute
)

// A subscribeRefused is a subscribe the cloud answered and turned down. The
// status is kept apart from the message because the retry policy reads it: a
// refusal and a transport failure deserve different patience.
type subscribeRefused struct {
	status int
	reason string
	body   string
}

func (e *subscribeRefused) Error() string {
	return fmt.Sprintf("subscribe failed: %s - %s", e.reason, e.body)
}

// subscribeRetryHold reports how long to wait before the next automatic retry
// after failures consecutive failures, the last of them err.
//
// Only the ticker's own retries are held back. A trigger from a config change
// carries new information — the streamer that did not fit may have just been
// removed — and waiting one out would make the fix look broken.
func subscribeRetryHold(failures int, err error) time.Duration {
	if failures <= 0 {
		return 0
	}
	hold := subscribeRetryBase
	for range failures - 1 {
		if hold >= subscribeRetryMax/2 {
			hold = subscribeRetryMax
			break
		}
		hold *= 2
	}
	var refused *subscribeRefused
	if errors.As(err, &refused) {
		// The budget itself saying stop. Climbing to the ceiling from here
		// would spend what is left of it on the way up.
		if refused.status == http.StatusTooManyRequests {
			return subscribeRetryMax
		}
		if refused.status >= 400 && refused.status < 500 {
			return max(hold, subscribeRefusedFloor)
		}
	}
	return hold
}

func (c *Client) handleSubscription(ctx context.Context) {
	log := c.logger.With("func", "handleSubscription")
	log.Info("Started subscription handler")
	defer log.Info("Stopped subscription handler")

	resubTicker := time.NewTicker(10 * time.Second)
	defer resubTicker.Stop()
	c.triggerResubscribe() // Trigger initial subscription on startup

	var lastSub time.Time
	// Consecutive failures and the hold they have earned. Kept on the loop
	// rather than the client: they describe this handler's retrying, and
	// nothing outside it retries a subscribe.
	var failures int
	var holdUntil time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.ResubscribeCh:
			// Stay under the subscribe rate limit and coalesce bursts: if the
			// previous subscribe was less than subscribeMinInterval ago, wait out
			// the remainder, folding any triggers that arrive meanwhile into this
			// one call. An isolated trigger after an idle period waits not at all.
			if wait := subscribeMinInterval - time.Since(lastSub); wait > 0 {
				if !c.drainTriggers(ctx, wait) {
					return // context cancelled while waiting
				}
			}
			lastSub = time.Now()
			log.Info("Subscribing...")
			if err := c.newSubscription(ctx); err != nil {
				failures++
				hold := jitter(subscribeRetryHold(failures, err))
				holdUntil = time.Now().Add(hold)
				log.Info("Subscribe failed", "error", err,
					"consecutive_failures", failures, "retrying_in", hold)
			} else {
				failures, holdUntil = 0, time.Time{}
			}
			c.markInitialSubDone()
		case <-c.ReloadCh:
			// Ordering against a pending resubscribe does not matter: a
			// subscription cycle reloads the file itself, so whichever runs
			// second reads the same config the first did.
			if err := c.reloadConfig(ctx); err != nil {
				log.Warn("Config reload failed", "error", err)
			}
		case <-resubTicker.C:
			if time.Now().Before(holdUntil) {
				continue // holding off after a failed subscribe
			}
			if failures > 0 {
				c.triggerResubscribe()
				continue
			}
			ids, _ := c.subscriptions.Load().(streamer.StreamerRefs)
			if _, remote := splitLocal(ids); len(remote) == 0 {
				continue // Nothing keeping the connection alive, skip resubscribe checks
			}
			c.mu.RLock()
			expiresAt := c.jwtExpiresAt
			isSubscribed := c.isSubscribed
			c.mu.RUnlock()
			if !isSubscribed || time.Until(expiresAt) < 30*time.Second {
				c.triggerResubscribe()
			}
		}
	}
}

func (c *Client) newSubscription(ctx context.Context) (err error) {
	log := c.logger.With("func", "newSubscription")
	// A cycle that fails after subscribe() succeeded has no connection or
	// handlers behind it, so it must not read as subscribed.
	defer func() {
		if err != nil {
			c.setSubscribed(false)
		}
	}()

	if c.stopWatchHandler != nil {
		c.stopWatchHandler()
	}
	if c.stopScrapeHandler != nil {
		c.stopScrapeHandler()
	}
	c.natsHandlerWg.Wait() // Wait for NATS handlers to exit
	c.setSubscribed(false)

	// Load streamers + rules from config
	if err := c.streamersFile.Load(); err != nil {
		return fmt.Errorf("failed to load streamers config: %w", err)
	}
	if c.OnConfigReloaded != nil {
		c.OnConfigReloaded()
	}

	// Combine IDs from streamers.toml with IDs from one-shot list. Streamers on
	// a local platform are never named to the cloud: they join the subscribed
	// set without a round trip.
	allIDs := slices.Concat(c.streamersFile.AllIDs(), vlstate.GetOneShotList(ctx, c.kv))
	localIDs, remoteIDs := splitLocal(allIDs)
	var subscribedIDs streamer.StreamerRefs

	// Reflect the config's membership in the view docs before the subscribe
	// round-trip, so an added streamer's row appears at once and survives a
	// failed subscribe. A second reconcile below aligns them with what was
	// actually subscribed.
	c.reconcileDocs(ctx, allIDs)

	var pruned []string
	if len(remoteIDs) > 0 {
		var err error
		var rejected []subscribeRejection
		// Get new user JWT and subscribed streamer IDs
		subscribedIDs, rejected, err = c.subscribe(ctx, remoteIDs)
		if err != nil {
			return fmt.Errorf("failed to get new user JWT: %w", err)
		}
		c.closeConnection()

		// Drop entries the cloud definitively rejected; they would be rejected
		// again on every cycle.
		pruned = c.streamersFile.DropURLs(definitiveRejections(rejected))
		for _, r := range rejected {
			log.Warn("Cloud rejected a streamer entry",
				"url", r.URL, "streamer", r.name(), "code", r.Error, "reason", r.Message,
				"removed", slices.Contains(pruned, r.URL))
		}
	} else {
		log.Info("No cloud streamers to subscribe to, skipping subscription")
		// Nothing needs the cloud connection anymore.
		c.closeConnection()
	}
	remoteSubscribed := len(subscribedIDs) > 0
	subscribedIDs = append(subscribedIDs, localIDs...)

	var purgedOneShots []vlconfig.OneShotPurge
	if len(subscribedIDs) > 0 {
		// Resolve URLs to Key form in both [streamers] and [[rules]]
		c.streamersFile.Normalize(subscribedIDs)

		// Move new one-shot IDs from streamers.toml to KV with TTL,
		// carrying their per-streamer options along (the file forgets
		// them once purged).
		purgedOneShots = c.streamersFile.PurgeOneShots()
		log.Debug("Purged one-shot IDs from streamers.toml", "count", len(purgedOneShots))
	}
	// Also written when nothing was subscribed, so a file whose every entry
	// was rejected is still repaired.
	if len(subscribedIDs) > 0 || len(pruned) > 0 {
		if err := c.streamersFile.Write(); err != nil {
			return fmt.Errorf("failed to write streamer IDs to config: %w", err)
		}
	}
	for _, p := range purgedOneShots {
		vlstate.PutOneShot(ctx, c.kv, p.ID.StreamerKey, vlstate.OneShotOptions{
			Variant:         p.Opts.Variant,
			OutputTemplate:  p.Opts.OutputTemplate,
			DeletePartFiles: p.Opts.DeletePartFiles,
			SplitSize:       p.Opts.SplitSize,
			SplitDuration:   p.Opts.SplitDuration,
			ArchiveDir:      p.Opts.ArchiveDir,
		})
		// A streamer demoted to one-shot mid-recording never passes through
		// worker creation, which normally lifts the entry's TTL, so lift it
		// here before it expires under the running session.
		if doc, err := vlstate.GetStreamerDoc(ctx, c.kv, p.ID.StreamerKey); err == nil && doc.ActiveSessionID != "" {
			vlstate.PersistOneShot(ctx, c.kv, p.ID.StreamerKey)
		}
		log.Debug("Added streamer to one-shot list", "key", p.ID.StreamerKey)
	}

	if remoteSubscribed {
		// Reconnect to NATS with new JWT
		if err := c.connect(); err != nil {
			return fmt.Errorf("failed to connect to NATS: %w", err)
		}

		// Restart handlers with new NATS connection. A zero-streamer
		// subscription has no notification consumer to watch.
		if c.consumerName != "" {
			if err := c.startWatchHandler(ctx); err != nil {
				return fmt.Errorf("failed to start watch handler: %w", err)
			}
		}
		if err := c.startScrapeHandler(ctx); err != nil {
			return fmt.Errorf("failed to start scrape handler: %w", err)
		}
	} else if len(remoteIDs) > 0 {
		// Asked for streamers, got none back: every entry was rejected.
		// There is nothing to connect for, and subscribe() has already
		// flipped the subscribed flag — leaving it set would tell the
		// status limiter and the scrape handler to go ahead against a
		// connection that was never opened.
		c.setSubscribed(false)
	}

	c.applySubscriptionDiff(subscribedIDs)

	// Align the view docs with the new subscription. The streamers file was
	// just (re)loaded above, so this is also how option changes (enable/disable,
	// variant, monitor, ...) reach the docs.
	c.reconcileDocs(ctx, subscribedIDs)

	if err := vlstate.PutCount(ctx, c.kv, "subscription_count", len(subscribedIDs)); err != nil {
		log.Debug("Failed to write subscription_count", "error", err)
	}

	return nil
}

// reloadConfig applies a streamers.toml change that cannot alter the
// subscription (a record switch, or any per-streamer option but monitor)
// without a subscribe round trip. Each subscribe replaces this client's
// notification consumer, losing notifications published meanwhile, and is
// rate-limited.
//
// Runs on the subscription handler's goroutine, like newSubscription, so the
// two never race over prevStopped or the streamers file.
func (c *Client) reloadConfig(ctx context.Context) error {
	log := c.logger.With("func", "reloadConfig")

	if err := c.streamersFile.Load(); err != nil {
		return fmt.Errorf("failed to load streamers config: %w", err)
	}
	if c.OnConfigReloaded != nil {
		c.OnConfigReloaded()
	}
	ids, _ := c.subscriptions.Load().(streamer.StreamerRefs)
	if len(ids) == 0 {
		// Nothing subscribed: reconciling against an empty set would delete
		// every doc, and with no subscription there is nothing this could be
		// about. The cycle that follows rebuilds the docs from what it gets.
		log.Debug("Reloaded streamer config with no subscription")
		return nil
	}
	// Docs carry the effective options, so the switch a client just flipped
	// only reaches its own UI through here.
	c.reconcileDocs(ctx, ids)
	c.applyEnableDiff(ids)
	log.Debug("Reloaded streamer config", "count", len(ids))
	return nil
}

// applyEnableDiff refetches status for streamers whose recording was just
// switched back on, since the cloud sends no event for it, and records the new
// switched-off set as the baseline for the next call. The baseline is taken on
// every call including the first, so streamers already off at startup count.
func (c *Client) applyEnableDiff(subscribedIDs streamer.StreamerRefs) {
	log := c.logger.With("func", "applyEnableDiff")

	currStopped := make(map[string]struct{})
	for _, id := range subscribedIDs {
		if !c.streamersFile.IsRecording(id) {
			currStopped[id.String()] = struct{}{}
		}
	}

	c.mu.Lock()
	initialSubDone := c.initialSubDone
	c.mu.Unlock()

	if initialSubDone {
		for _, id := range subscribedIDs {
			s := id.String()
			if _, wasStopped := c.prevStopped[s]; !wasStopped {
				continue
			}
			if _, stillStopped := currStopped[s]; stillStopped {
				continue
			}
			log.Info("Enabled streamer, refetching status", "id", id)
			c.statusReqCh <- id
		}
	}
	c.prevStopped = currStopped
}

// applySubscriptionDiff reacts to how the new subscription set differs from the
// previous one — status fetches for streamers that were added or re-enabled, a
// removal notice for those that went away — and then records the new set as the
// baseline for the next cycle.
func (c *Client) applySubscriptionDiff(subscribedIDs streamer.StreamerRefs) {
	log := c.logger.With("func", "applySubscriptionDiff")

	c.mu.Lock()
	initialSubDone := c.initialSubDone
	c.mu.Unlock()

	if initialSubDone {
		prevSubscribedIDs, _ := c.subscriptions.Load().(streamer.StreamerRefs)
		added, removed := prevSubscribedIDs.Diff(subscribedIDs)
		// A lone add has someone waiting on it, so it jumps the queue; a batch
		// goes to the backlog so it can't delay the next lone add.
		addCh := c.statusReqCh
		if len(added) == 1 {
			addCh = c.StatusReqPriCh
		}
		for _, id := range added {
			log.Info("Subscribed to streamer", "id", id)
			addCh <- id // Trigger status fetch for new streamers
		}
		for _, id := range removed {
			log.Info("Unsubscribed from streamer", "id", id)
			// Drop any sweep schedule with it, so an untracked streamer does not
			// hold an entry for the life of the daemon.
			c.sweep.forget(id.StreamerKey)
			str := streamer.StreamerState{
				StreamerRef: id,
				Status:      streamer.StatusRemoved,
			}
			c.StreamerCh <- str // Notify session worker that streamer was removed
		}
	}

	c.applyEnableDiff(subscribedIDs)
	c.subscriptions.Store(subscribedIDs)
}

// reconcileDocs rebuilds the view-doc membership from the subscribed IDs,
// their effective options from streamers.toml, and the KV one-shot list (a
// purged one-shot has no rules left in the file, so OptionsFor alone would
// report it as monitored with global-default options).
func (c *Client) reconcileDocs(ctx context.Context, subscribedIDs streamer.StreamerRefs) {
	oneShots := make(map[streamer.StreamerKey]bool)
	for _, id := range vlstate.GetOneShotList(ctx, c.kv) {
		oneShots[id.StreamerKey] = true
	}
	seeds := make([]vlstate.DocSeed, 0, len(subscribedIDs))
	for _, id := range subscribedIDs {
		o := c.streamersFile.OptionsFor(id)
		if oneShots[id.StreamerKey] {
			// A purged one-shot's option overrides live in KV; fields the
			// file still sets (a fresher edit, not yet re-purged) win.
			if osOpts, ok := vlstate.GetOneShotOptions(ctx, c.kv, id.StreamerKey); ok {
				if o.Variant == "" {
					o.Variant = osOpts.Variant
				}
				if o.OutputTemplate == "" {
					o.OutputTemplate = osOpts.OutputTemplate
				}
				if o.DeletePartFiles == nil {
					o.DeletePartFiles = osOpts.DeletePartFiles
				}
				if o.SplitSize == "" {
					o.SplitSize = osOpts.SplitSize
				}
				if o.SplitDuration == "" {
					o.SplitDuration = osOpts.SplitDuration
				}
				if o.ArchiveDir == "" {
					o.ArchiveDir = osOpts.ArchiveDir
				}
			}
		}
		seeds = append(seeds, vlstate.DocSeed{
			ID: id,
			Opts: vlstate.DocOptions{
				Monitor:         o.Monitor && !oneShots[id.StreamerKey],
				Record:          o.Record,
				Variant:         o.Variant,
				OutputTemplate:  o.OutputTemplate,
				DeletePartFiles: o.DeletePartFiles,
				SplitSize:       o.SplitSize,
				SplitDuration:   o.SplitDuration,
				ArchiveDir:      o.ArchiveDir,
			},
		})
	}
	c.store.Reconcile(ctx, seeds)
}

// IsSubscribed reports whether id is in the current subscription set. Callers
// use this to avoid writing KV state for a streamer that has already been
// unsubscribed (e.g. a one-shot whose recording was cancelled and whose KV was
// purged) — such a write would otherwise resurrect its dashboard row.
func (c *Client) IsSubscribed(id streamer.StreamerRef) bool {
	ids, _ := c.subscriptions.Load().(streamer.StreamerRefs)
	return ids.Contains(id)
}

func (c *Client) setSubscribed(subscribed bool) {
	c.mu.Lock()
	changed := c.isSubscribed != subscribed
	c.isSubscribed = subscribed
	c.mu.Unlock()

	if changed && subscribed {
		c.cond.Broadcast() // Wake all waiters
	}
}

func (c *Client) waitSubscribe() {
	c.mu.Lock()
	defer c.mu.Unlock()

	for !c.isSubscribed {
		c.cond.Wait()
	}
}

// waitSubscribeCtx is waitSubscribe for a caller that has to give up when its
// context ends, reporting false when it did. Waking it relies on the broadcast
// rateLimitStatusRequests fires at shutdown: a sync.Cond has no way to select
// on a context of its own.
func (c *Client) waitSubscribeCtx(ctx context.Context) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	for !c.isSubscribed {
		if ctx.Err() != nil {
			return false
		}
		c.cond.Wait()
	}
	return ctx.Err() == nil
}

// markInitialSubDone records that the first subscription cycle has run to a
// conclusion — whether or not it produced an active subscription, and whether
// or not it failed — and wakes waitInitialSub. Called by handleSubscription
// once per cycle rather than by newSubscription, so the many error returns on
// the way through a cycle cannot each become a way to leave startup waiting
// forever.
func (c *Client) markInitialSubDone() {
	c.mu.Lock()
	c.initialSubDone = true
	c.mu.Unlock()
	c.cond.Broadcast()
}

// waitInitialSub blocks until the first subscription cycle completes. Unlike
// waitSubscribe it returns even when there are no streamers to subscribe to,
// and even when the subscribe was refused, so daemon startup is gated neither
// on having subscribers nor on the cloud answering.
func (c *Client) waitInitialSub() {
	c.mu.Lock()
	defer c.mu.Unlock()

	for !c.initialSubDone {
		c.cond.Wait()
	}
}

// triggerResubscribe signals the client to initiate a new subscription
func (c *Client) triggerResubscribe() {
	select {
	case c.ResubscribeCh <- struct{}{}:
	default:
	}
}

// TriggerReload signals the client to re-read streamers.toml without touching
// the subscription. Dropping the signal when one is already queued loses
// nothing: the queued reload reads the file, not the signal.
func (c *Client) TriggerReload() {
	select {
	case c.ReloadCh <- struct{}{}:
	default:
	}
}

// drainTriggers waits for d, discarding any resubscribe or reload triggers that
// arrive in the meantime so a burst of them collapses into the single
// subscription that follows it (whose fresh config load reflects whatever they
// were about). It returns false if ctx is cancelled before the wait completes.
func (c *Client) drainTriggers(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return false
		case <-c.ResubscribeCh:
			// Coalesced into the upcoming subscribe; nothing else to do.
		case <-c.ReloadCh:
			// Likewise: the subscribe reloads the file, which is all a reload
			// would have done.
		case <-timer.C:
			return true
		}
	}
}
