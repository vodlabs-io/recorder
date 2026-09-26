package cloud

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/vodlabs-io/recorder/internal/streamer"
	"github.com/vodlabs-io/recorder/internal/vlstate"
)

const streamersRoute = "/api/v1/streamers"

// streamersResponse is the response from GET /api/v1/streamers
type streamersResponse struct {
	Streamers streamer.List `json:"streamers"`
	AsOf      int64         `json:"as_of"` // Server timestamp when data was fetched
}

// ErrCheckNotRunnable is the cloud saying it could not run a check for a reason
// about this daemon rather than the streamer, most often an authenticated check
// with no desktop app to provide a browser context. Retrying is pointless; the
// check is parked until a browser context appears.
var ErrCheckNotRunnable = errors.New("cloud could not run that check")

// How long the daemon waits on a status check. The cloud answers within 48s,
// so both sit above that: the cloud gives up first and can say why. An authed
// check runs through the desktop webview, so it gets longer.
const (
	statusCheckCeiling = 55 * time.Second
	authedCheckCeiling = 75 * time.Second
)

// statusOpts are the modifiers a single-streamer status check can carry. They
// are ignored on a whole-list fetch.
type statusOpts struct {
	// authed requests an authenticated check (?auth=1): the cloud sends a
	// logged-in platform request that this daemon executes with its local
	// cookies attached (see scraper.go). PRIVACY: the cookies never leave this
	// machine except to the platform.
	authed bool
	// recheck (?recheck=1) also clears any retirement. It is the only way out
	// of StatusDefunct, so set it only for a check the user explicitly asked
	// for.
	recheck bool
	// streamsOnly (?streams=1) asks for a re-sign: fresh media URLs for a
	// recording already in flight, without the side effects of a status check.
	// Never use it to decide whether to start a recording.
	streamsOnly bool
}

// fetchStatus fetches and returns the current state of subscribed streamers from the API.
// If id is non-empty, returns only that single streamer's state.
func (c *Client) fetchStatus(ctx context.Context, opts statusOpts, id ...streamer.StreamerRef) (streamer.List, error) {
	log := c.logger.With("func", "fetchStatus", "streamer_filter", id,
		"authed", opts.authed, "recheck", opts.recheck, "streams_only", opts.streamsOnly)
	timestamp := time.Now().Unix()
	reqURL := apiBaseURL + streamersRoute

	// Add optional single streamer filter if provided
	if len(id) > 0 && !id[0].IsEmpty() {
		id := id[0]
		if id.Platform != "" && id.Username != "" {
			reqURL += "/" + url.PathEscape(id.Platform) + "/" + url.PathEscape(id.Username)
		} else if id.URL != "" {
			reqURL += "/url/" + url.PathEscape(string(id.URL))
		}
		q := url.Values{}
		if opts.authed {
			q.Set("auth", "1")
		}
		if opts.recheck {
			q.Set("recheck", "1")
		}
		if opts.streamsOnly {
			q.Set("streams", "1")
		}
		if len(q) > 0 {
			reqURL += "?" + q.Encode()
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("X-Timestamp", fmt.Sprintf("%d", timestamp))
	c.setClientHeaders(req)
	req.Header.Set("X-Client-ID", c.clientID)

	if err := c.signRequest(req, nil); err != nil {
		return nil, fmt.Errorf("failed to sign request: %w", err)
	}

	// Shares the shared client's transport but not its shorter deadline.
	timeout := statusCheckCeiling
	if opts.authed {
		timeout = authedCheckCeiling
	}
	client := &http.Client{Transport: c.httpClient.Transport, Timeout: timeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("status request failed: %w", err)
	}
	defer resp.Body.Close()
	c.noteResponse(ctx, resp)

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode == http.StatusServiceUnavailable {
			return nil, fmt.Errorf("%w: %s", ErrCheckNotRunnable, strings.TrimSpace(string(body)))
		}
		return nil, fmt.Errorf("status failed: %s - %s", resp.Status, string(body))
	}

	var result streamersResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}
	log.Debug("Fetched streamers", "count", len(result.Streamers), "as_of", result.AsOf)

	return result.Streamers, nil
}

// fetchSingleStatus fetches and returns the current state of a single streamer
// from the API. When authed is true it requests an authenticated check (used to
// record login-restricted streams); when streamsOnly is true it asks for a
// re-sign rather than a status check.
func (c *Client) fetchSingleStatus(
	ctx context.Context,
	id streamer.StreamerRef,
	authed bool,
	streamsOnly bool,
) (streamer.StreamerState, error) {
	emptyStr := streamer.StreamerState{}

	if id.IsEmpty() {
		return emptyStr, fmt.Errorf("no streamer ID provided")
	}
	strList, err := c.fetchStatus(ctx, statusOpts{authed: authed, streamsOnly: streamsOnly}, id)
	if err != nil {
		return emptyStr, err
	}
	if len(strList) == 0 {
		return emptyStr, fmt.Errorf("no streamer found with ID: %s", id)
	}
	return strList[0], nil
}

// logStreamers logs the details of a list of streamers.
func (c *Client) logStreamers(streamers streamer.List) {
	log := c.logger.With("func", "logStreamers")

	for r, str := range streamers {
		log.Info("Streamer", "status", str.Status, "id", str.StreamerRef)
		log.Debug("Full streamer", "object", str)

		// Only log streams if the list contains a single streamer who is live and has streams
		if r > 0 || str.Status != streamer.StatusLive || len(str.Streams) == 0 {
			continue
		}
		for i, stream := range str.Streams {
			quality := stream.QualityID
			if quality == "" {
				quality = "unknown"
			}
			resolution := stream.Resolution()
			if stream.ResWidth == 0 && stream.ResHeight == 0 {
				resolution = "unknown"
			}
			log.Info("Stream",
				"id", str.StreamerRef,
				"index", i+1,
				"quality", quality,
				"resolution", resolution,
				"url", stream.URL)
		}
	}
}

// handleStatusUpdate decides what to do with updated streamer status.
func (c *Client) handleStatusUpdate(ctx context.Context, str streamer.StreamerState) {
	// Carry the identity fields into the doc regardless of status. Live/paused
	// updates go to StatusReqPriCh (a local re-fetch) rather than store.SetStatus,
	// so these would otherwise only land on the next re-fetch; the setters make it
	// prompt and no-op when the streamer isn't tracked yet.
	if str.AvatarURL != "" {
		c.store.SetAvatarURL(ctx, str.StreamerKey, str.AvatarURL)
	}
	if str.DisplayName != "" {
		c.store.SetDisplayName(ctx, str.StreamerKey, str.DisplayName)
	}

	switch str.Status {
	case streamer.StatusLive, streamer.StatusPaused:
		// Re-fetch rather than use the pushed stream URLs, so the platform
		// request runs from this machine and returns URLs for a nearby edge.
		c.StatusReqPriCh <- str.StreamerRef
	case streamer.StatusRestricted:
		// A restricted room is only recordable with a platform login. If we hold
		// one for this platform, re-check it authenticated (attaching our local
		// cookies) to try to unlock it; otherwise fall through to a normal
		// dispatch (which won't record).
		if c.cookies != nil && c.cookies.Has(str.Platform) {
			c.recordWall(ctx, str)
			c.authStatusReqCh <- str.StreamerRef
		} else {
			c.dispatchStreamer(ctx, str)
		}
	case streamer.StatusUnknown:
		c.statusReqCh <- str.StreamerRef
	default:
		c.dispatchStreamer(ctx, str)
	}
}

// recordWall persists a login-walled status to the streamer's view doc without
// dispatching it: nothing is recordable until an authed re-check unlocks the
// room. The doc is what nudgeRestrictedStreamers (internal/vlapi) looks for
// when a login is stored or refreshed, so an escalation that fails (e.g. at
// startup, before a desktop app is connected) is retried then.
func (c *Client) recordWall(ctx context.Context, str streamer.StreamerState) {
	c.store.SetStatus(ctx, str, !vlstate.IsOneShot(ctx, c.kv, str.StreamerKey))
}

// dispatchStreamer sends the streamer to the session registry and updates its view doc.
func (c *Client) dispatchStreamer(ctx context.Context, str streamer.StreamerState) {
	log := c.logger.With("func", "dispatchStreamer")
	log.Debug("Dispatching streamer", "id", str.StreamerRef, "status", str.Status)

	// Update the streamer's view doc. A removed streamer's state is torn down
	// in the registry path instead (worker teardown purges its KV).
	if str.Status != streamer.StatusRemoved {
		c.store.SetStatus(ctx, str, !vlstate.IsOneShot(ctx, c.kv, str.StreamerKey))
	}
	select {
	case c.StreamerCh <- str:
	case <-ctx.Done():
	}
}

// The first status fetch is retried on this schedule when the service cannot be
// reached. It doubles up to the cap, which is what an outage settles at: the
// daemon is running and recording nothing, so it should ask often enough to
// start within a minute or so of the service coming back, and no more often.
const (
	initStatusRetryBase = 5 * time.Second
	initStatusRetryMax  = 2 * time.Minute
)

// InitStatusRetrying runs the first status fetch in the background, retrying
// until it succeeds or ctx ends.
//
// The service being unreachable while the daemon starts is an outage, not a
// misconfiguration — a laptop opened before its Wi-Fi associates is enough —
// and a recorder that exits then is one that was never running when the outage
// cleared. So it comes up either way: streamers show their last known state,
// the UI says the service is unreachable, and recording starts by itself once
// the fetch lands.
func (c *Client) InitStatusRetrying(ctx context.Context) {
	c.initStatusRetrying(ctx, initStatusRetryBase, initStatusRetryMax)
}

// initStatusRetrying is InitStatusRetrying with the schedule spelled out, so a
// test does not have to wait out the real one.
func (c *Client) initStatusRetrying(ctx context.Context, base, max time.Duration) {
	log := c.logger.With("func", "InitStatusRetrying")
	go func() {
		delay := base
		for attempt := 1; ; attempt++ {
			err := c.InitStatus(ctx)
			if err == nil {
				if attempt > 1 {
					log.Info("Fetched the initial status after retrying", "attempts", attempt)
				}
				return
			}
			if ctx.Err() != nil {
				return
			}
			log.Warn("Could not fetch the initial status; the daemon keeps running and will retry",
				"error", err, "attempt", attempt, "retry_in", delay)
			select {
			case <-ctx.Done():
				return
			case <-time.After(jitter(delay)):
			}
			if delay *= 2; delay > max {
				delay = max
			}
		}
	}()
}

// InitStatus fetches the last cached status for all subscribed streamers and calls handleStatusUpdate.
func (c *Client) InitStatus(ctx context.Context) error {
	log := c.logger.With("func", "InitStatus")

	if len(c.subscriptions.Load().(streamer.StreamerRefs)) == 0 {
		log.Info("No subscribed streamers, skipping initial status fetch")
		return nil
	}

	// Fetch status for all streamers (list fetch is never authenticated)
	strs, err := c.fetchStatus(ctx, statusOpts{})
	if err != nil {
		return fmt.Errorf("failed to fetch initial status: %w", err)
	}

	log.Info("Fetched all subscribed streamers", "count", len(strs))
	c.logStreamers(strs)

	for _, str := range strs {
		c.handleStatusUpdate(ctx, str)
	}

	return nil
}
