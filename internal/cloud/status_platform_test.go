package cloud

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nkeys"

	"github.com/vodlabs-io/recorder/internal/streamer"
	"github.com/vodlabs-io/recorder/internal/vlconfig"
	"github.com/vodlabs-io/recorder/internal/vllog"
	"github.com/vodlabs-io/recorder/internal/vlstate"
)

// hitLog records what reached the stand-in cloud, so a test can ask which
// platform was served and whether the request was a re-sign.
type hitLog struct {
	mu   sync.Mutex
	hits []string // "<platform>" or "<platform>:streams"
	seen chan string
}

func (h *hitLog) add(platform string, streams bool) {
	key := platform
	if streams {
		key += ":streams"
	}
	h.mu.Lock()
	h.hits = append(h.hits, key)
	h.mu.Unlock()
	select {
	case h.seen <- key:
	default:
	}
}

func (h *hitLog) count(key string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, k := range h.hits {
		if k == key {
			n++
		}
	}
	return n
}

// await waits for a request matching key to reach the cloud, reporting how long
// it took. It returns false if none arrived inside the window.
func (h *hitLog) await(key string, within time.Duration) (time.Duration, bool) {
	start := time.Now()
	deadline := time.After(within)
	for {
		if h.count(key) > 0 {
			return time.Since(start), true
		}
		select {
		case <-h.seen:
		case <-deadline:
			return time.Since(start), h.count(key) > 0
		}
	}
}

// newLimiterTestClient wires a Client to a stand-in cloud that answers every
// single-streamer request with a live streamer, and starts the real dispatcher.
func newLimiterTestClient(t *testing.T, platforms []vlconfig.Platform) (*Client, *hitLog) {
	t.Helper()

	hits := &hitLog{seen: make(chan string, 1)}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// /api/v1/streamers/{platform}/{username}
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		platform := parts[len(parts)-2]
		username := parts[len(parts)-1]
		hits.add(platform, r.URL.Query().Get("streams") == "1")

		_ = json.NewEncoder(w).Encode(streamersResponse{
			Streamers: streamer.List{{
				StreamerRef: streamer.StreamerRef{
					StreamerKey: streamer.StreamerKey{Platform: platform, Username: username},
				},
				Status: streamer.StatusLive,
			}},
			AsOf: time.Now().Unix(),
		})
	}))
	t.Cleanup(srv.Close)

	orig := apiBaseURL
	apiBaseURL = srv.URL
	t.Cleanup(func() { apiBaseURL = orig })

	origPlatforms := vlconfig.Platforms()
	vlconfig.SetPlatforms(platforms)
	t.Cleanup(func() { vlconfig.SetPlatforms(origPlatforms) })

	logger, err := vllog.NewFileLogger(vlconfig.LoggerConfig{Mode: "off", Level: "error"}, "test")
	if err != nil {
		t.Fatalf("logger: %v", err)
	}
	kp, err := nkeys.CreateUser()
	if err != nil {
		t.Fatalf("nkey: %v", err)
	}
	seed, err := kp.Seed()
	if err != nil {
		t.Fatalf("nkey seed: %v", err)
	}
	nc, kv := newTestKV(t)

	c := &Client{
		logger:          logger,
		localNC:         nc,
		httpClient:      srv.Client(),
		kv:              kv,
		store:           vlstate.NewStore(kv),
		nkeySeed:        string(seed),
		isSubscribed:    true,
		statusReqCh:     make(chan streamer.StreamerRef, 100),
		StatusReqPriCh:  make(chan streamer.StreamerRef, 100),
		authStatusReqCh: make(chan streamer.StreamerRef, 100),
		StreamReqCh:     make(chan streamer.StreamerRef, 100),
		StreamerCh:      make(chan streamer.StreamerState, 1000),
	}
	c.cond = sync.NewCond(&c.mu)
	c.ceiling = newTokenBucket(t.Context(), vlconfig.RateLimit{Burst: 64, Interval: time.Millisecond})

	// Cleanups run last-registered-first, so this one runs before the two above
	// put apiBaseURL and the platform table back: a fetch still unwinding reads
	// both, and restoring them under it is a data race that surfaces in
	// whichever test happens to run next.
	t.Cleanup(c.waitFetches)

	// Drain dispatched streamers so a full channel never becomes the thing
	// under test.
	go func() {
		for {
			select {
			case <-c.StreamerCh:
			case <-t.Context().Done():
				return
			}
		}
	}()
	go c.rateLimitStatusRequests(t.Context())
	return c, hits
}

func ref(platform, user string) streamer.StreamerRef {
	return streamer.StreamerRef{StreamerKey: streamer.StreamerKey{Platform: platform, Username: user}}
}

// spent is a budget of one token an hour: enough for a single request, and
// nothing more for the rest of the test.
func spent() *vlconfig.RateLimit {
	return &vlconfig.RateLimit{Burst: 1, Interval: time.Hour}
}

// The reason the buckets are per platform: a platform's budget is about what
// THAT platform tolerates, so a backlog against one must not decide when
// another is checked.
func TestOneBusyPlatformDoesNotStarveAnother(t *testing.T) {
	c, hits := newLimiterTestClient(t, []vlconfig.Platform{
		{Name: "busy", StatusLimit: spent()},
		{Name: "quiet", StatusLimit: &vlconfig.RateLimit{Burst: 5, Interval: time.Millisecond}},
	})

	// Far more work than "busy" can ever get through.
	for i := range 50 {
		c.statusReqCh <- ref("busy", string(rune('a'+i%26)))
	}
	// The one request that matters, queued behind all of it.
	c.statusReqCh <- ref("quiet", "alice")

	elapsed, ok := hits.await("quiet", 5*time.Second)
	if !ok {
		t.Fatalf("quiet was never checked in %s: a backlog on another platform starved it", elapsed)
	}
	if got := hits.count("busy"); got > 2 {
		t.Errorf("busy was checked %d times, want at most its 1-token budget (+1 in flight)", got)
	}
}

// A re-sign is asked for by a recording already running, on the cadence the
// platform signs media URLs for. If it waited behind that platform's status
// checks it would arrive after the grant it was replacing had died — a hole in
// the recording. It draws on its own budget for exactly that reason.
func TestReSignIsNotDelayedByStatusChecks(t *testing.T) {
	c, hits := newLimiterTestClient(t, []vlconfig.Platform{{
		Name:        "youtube",
		StatusLimit: spent(),
		StreamLimit: &vlconfig.RateLimit{Burst: 4, Interval: 10 * time.Millisecond},
	}})

	// Saturate the platform's status budget several times over.
	for i := range 50 {
		c.StatusReqPriCh <- ref("youtube", string(rune('a'+i%26)))
	}
	c.StreamReqCh <- ref("youtube", "live")

	// The recorder waits streamRefreshWait (10s) for a re-sign; well inside it.
	elapsed, ok := hits.await("youtube:streams", 3*time.Second)
	if !ok {
		t.Fatalf("no re-sign reached the cloud in %s: status checks starved the stream lane", elapsed)
	}
}

// The flag is what tells the cloud to skip a status check's bookkeeping. A
// re-sign sent as a plain check would have the cloud judge a running recording
// on one lookup.
func TestReSignCarriesTheStreamsFlag(t *testing.T) {
	c, hits := newLimiterTestClient(t, []vlconfig.Platform{{Name: "twitch"}})

	c.StreamReqCh <- ref("twitch", "alice")
	if _, ok := hits.await("twitch:streams", 3*time.Second); !ok {
		t.Fatal("re-sign did not carry streams=1")
	}
	if got := hits.count("twitch"); got != 0 {
		t.Errorf("%d re-signs were sent as plain status checks", got)
	}
}

// Platforms arrive from the heartbeat, so a limiter is built on first use from
// whatever pace is configured then — including for a platform the daemon has
// never seen and for a URL-only lookup, whose platform the cloud works out.
func TestLimiterForUsesConfiguredPace(t *testing.T) {
	c, _ := newLimiterTestClient(t, []vlconfig.Platform{
		{Name: "chzzk", StatusLimit: &vlconfig.RateLimit{Burst: 7, Interval: 250 * time.Millisecond}},
	})
	ctx := t.Context()

	known := c.limiterFor(ctx, "chzzk")
	if got := cap(known.bucket(kindStatus).tokens); got != 7 {
		t.Errorf("chzzk status burst = %d, want its configured 7", got)
	}
	if c.limiterFor(ctx, "chzzk") != known {
		t.Error("limiterFor built a second limiter for a platform it already had")
	}

	// An unknown platform and a URL-only lookup both fall back to the defaults
	// rather than to no budget at all.
	for _, name := range []string{"neverheardof", ""} {
		l := c.limiterFor(ctx, name)
		if cap(l.bucket(kindStatus).tokens) == 0 || cap(l.bucket(kindStream).tokens) == 0 {
			t.Errorf("limiter for %q has an empty bucket: it would never fetch", name)
		}
	}
}

// The dispatcher stands between the daemon-wide queues and every platform's
// pace. Blocking there on one platform is the starvation the split removes, so
// a full lane drops instead — the same thing the producers do.
func TestDispatcherDropsRatherThanBlock(t *testing.T) {
	c, hits := newLimiterTestClient(t, []vlconfig.Platform{
		{Name: "busy", StatusLimit: spent()},
		{Name: "quiet", StatusLimit: &vlconfig.RateLimit{Burst: 5, Interval: time.Millisecond}},
	})

	// More than one platform's lane can hold (laneDepth), all for that platform.
	go func() {
		for i := range laneDepth * 3 {
			select {
			case c.statusReqCh <- ref("busy", string(rune('a'+i%26))):
			case <-t.Context().Done():
				return
			}
		}
		c.statusReqCh <- ref("quiet", "alice")
	}()

	if _, ok := hits.await("quiet", 5*time.Second); !ok {
		t.Fatal("the dispatcher blocked on a full lane, stalling every other platform")
	}
}

// A limit the cloud sends is used as given, but only within bounds: it decides
// how hard this machine knocks on a platform's door.
func TestBackendLimitsAreClamped(t *testing.T) {
	c, _ := newLimiterTestClient(t, []vlconfig.Platform{{
		Name:        "wild",
		StatusLimit: &vlconfig.RateLimit{Burst: 10_000, Interval: time.Microsecond},
	}})

	l := c.limiterFor(t.Context(), "wild")
	if got := cap(l.bucket(kindStatus).tokens); got != 20 {
		t.Errorf("status burst = %d, want it clamped to 20", got)
	}
}

// A retune from the heartbeat must reach a daemon that is already running,
// since daemons stay up for days.
func TestRetuneAppliesToARunningLimiter(t *testing.T) {
	c, _ := newLimiterTestClient(t, []vlconfig.Platform{
		{Name: "kick", StatusLimit: &vlconfig.RateLimit{Burst: 3, Interval: time.Second}},
	})
	ctx := t.Context()

	l := c.limiterFor(ctx, "kick")
	before := l.bucket(kindStatus)
	if cap(before.tokens) != 3 {
		t.Fatalf("initial burst = %d, want 3", cap(before.tokens))
	}

	// What a heartbeat does: a new platform list, then the next dispatch.
	vlconfig.SetPlatforms([]vlconfig.Platform{
		{Name: "kick", StatusLimit: &vlconfig.RateLimit{Burst: 9, Interval: time.Second}},
	})
	if l != c.limiterFor(ctx, "kick") {
		t.Fatal("a retune replaced the limiter, discarding everything queued in its lanes")
	}
	after := l.bucket(kindStatus)
	if cap(after.tokens) != 9 {
		t.Errorf("burst after retune = %d, want the cloud's 9", cap(after.tokens))
	}

	// The bucket it replaced must stop refilling, or every retune leaks a
	// goroutine that outlives what it was pacing.
	drain(before.tokens)
	select {
	case <-before.tokens:
		t.Error("the replaced bucket is still being refilled")
	case <-time.After(1500 * time.Millisecond):
	}

	// An unchanged pace must not churn the bucket: a request waiting on one
	// that keeps being replaced would never be served.
	if c.limiterFor(ctx, "kick").bucket(kindStatus) != after {
		t.Error("an unchanged pace still replaced the bucket")
	}
}

// A retune retires the bucket the lane may already be waiting on. Without a
// way out of that wait the lane would sit on a bucket nothing refills for the
// rest of the daemon's life — the platform silently stops being checked.
func TestRetuneReleasesALaneWaitingOnTheOldBucket(t *testing.T) {
	c, hits := newLimiterTestClient(t, []vlconfig.Platform{
		{Name: "soop", StatusLimit: spent()}, // one token, then an hour of nothing
	})

	// Spend the single token, then queue a second request so the lane is left
	// blocked on a bucket that will not refill for an hour.
	c.statusReqCh <- ref("soop", "first")
	if _, ok := hits.await("soop", 3*time.Second); !ok {
		t.Fatal("the first request never went out")
	}
	c.statusReqCh <- ref("soop", "second")

	// The lane has to actually be blocked on the spent bucket before the
	// retune, or this proves nothing: a lane that had not reached its wait yet
	// would simply resolve the new bucket and pass either way.
	time.Sleep(300 * time.Millisecond)
	if got := hits.count("soop"); got != 1 {
		t.Fatalf("%d requests went out, want the second still waiting on the spent budget", got)
	}

	// What a heartbeat carrying a new pace does.
	vlconfig.SetPlatforms([]vlconfig.Platform{
		{Name: "soop", StatusLimit: &vlconfig.RateLimit{Burst: 5, Interval: time.Millisecond}},
	})
	c.limiterFor(t.Context(), "soop")

	deadline := time.After(3 * time.Second)
	for hits.count("soop") < 2 {
		select {
		case <-hits.seen:
		case <-deadline:
			t.Fatal("the waiting lane never picked up the retuned budget")
		}
	}
}

func drain(tokens chan time.Time) {
	for {
		select {
		case <-tokens:
		default:
			return
		}
	}
}
