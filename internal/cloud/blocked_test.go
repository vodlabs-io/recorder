package cloud

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nkeys"
	"github.com/vodlabs-io/recorder/internal/streamer"
	"github.com/vodlabs-io/recorder/internal/version"
	"github.com/vodlabs-io/recorder/internal/vlconfig"
	"github.com/vodlabs-io/recorder/internal/vllog"
)

// blockServer is a fake service that answers every call with the blocked
// reply until it is told to stop.
type blockServer struct {
	*httptest.Server
	blocked atomic.Bool
	until   atomic.Int64
	calls   atomic.Int64
}

func newBlockServer(t *testing.T, until int64) *blockServer {
	t.Helper()
	s := &blockServer{}
	s.blocked.Store(true)
	s.until.Store(until)
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if s.blocked.Load() {
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprintf(w, `{"error":"client_blocked","until":%d}`, s.until.Load())
			return
		}
		switch r.URL.Path {
		case heartbeatEndpoint:
			_, _ = w.Write([]byte(`{"minimum_version":"0.0.1","maximum_version":"9.9.9","recommended_version":"1.0.0"}`))
		case registerChallengeEndpoint:
			_, _ = w.Write([]byte(`{"nonce":"n","difficulty":0}`))
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	prev := apiBaseURL
	apiBaseURL = s.URL
	t.Cleanup(func() {
		apiBaseURL = prev
		s.Close()
	})
	return s
}

// upstreamLog records what the client reported through onVersionChecked.
type upstreamLog struct {
	mu   sync.Mutex
	seen []version.Upstream
}

func (l *upstreamLog) record(_ context.Context, u version.Upstream) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.seen = append(l.seen, u)
}

func (l *upstreamLog) last(t *testing.T) version.Upstream {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.seen) == 0 {
		t.Fatal("nothing was reported through onVersionChecked")
	}
	return l.seen[len(l.seen)-1]
}

func blockTestClient(t *testing.T, srv *blockServer) (*Client, *upstreamLog) {
	t.Helper()
	logger, err := vllog.NewFileLogger(vlconfig.LoggerConfig{Mode: "off", Level: "error"}, "test")
	if err != nil {
		t.Fatalf("NewFileLogger: %v", err)
	}
	key, err := nkeys.CreateUser()
	if err != nil {
		t.Fatalf("create user key: %v", err)
	}
	seed, err := key.Seed()
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	// The probe runs for the client's lifetime; end it with the test.
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	ups := &upstreamLog{}
	return &Client{
		runCtx:           ctx,
		logger:           logger,
		clientID:         "client",
		nkeySeed:         string(seed),
		httpClient:       srv.Client(),
		onVersionChecked: ups.record,
		ResubscribeCh:    make(chan struct{}, 1),
		ReloadCh:         make(chan struct{}, 1),
	}, ups
}

func TestBlockedReplyEntersBlockedState(t *testing.T) {
	until := time.Now().Add(2 * time.Hour).Unix()
	srv := newBlockServer(t, until)
	c, ups := blockTestClient(t, srv)

	if err := c.sendHeartbeat(context.Background()); err == nil {
		t.Fatal("heartbeat succeeded against a blocked reply")
	}
	if !c.isBlocked() {
		t.Fatal("a 403 client_blocked did not block the client")
	}
	if got := c.blockedUntil().Unix(); got != until {
		t.Fatalf("blockedUntil = %d, want %d", got, until)
	}
	u := ups.last(t)
	if !u.Blocked || u.BlockedUntil != until {
		t.Fatalf("reported %+v, want Blocked with BlockedUntil %d", u, until)
	}
}

func TestPermanentBlockHasNoEnd(t *testing.T) {
	srv := newBlockServer(t, 0)
	c, ups := blockTestClient(t, srv)

	_, _ = c.fetchStatus(context.Background(), statusOpts{})
	if !c.isBlocked() {
		t.Fatal("a permanent block did not block the client")
	}
	if !c.blockedUntil().IsZero() {
		t.Fatalf("blockedUntil = %v, want zero", c.blockedUntil())
	}
	if u := ups.last(t); !u.Blocked || u.BlockedUntil != 0 {
		t.Fatalf("reported %+v, want Blocked with no end", u)
	}
}

// Only the blocked reply blocks: any other 403 is an ordinary failure, and
// its body still reaches the caller's error.
func TestOtherForbiddenIsNotABlock(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"forbidden"}`))
	}))
	defer srv.Close()
	prev := apiBaseURL
	apiBaseURL = srv.URL
	defer func() { apiBaseURL = prev }()

	c, _ := blockTestClient(t, &blockServer{Server: srv})
	err := c.sendHeartbeat(context.Background())
	if c.isBlocked() {
		t.Fatal("a plain 403 blocked the client")
	}
	if err == nil || !strings.Contains(err.Error(), `"forbidden"`) {
		t.Fatalf("error = %v, want the 403 body in it", err)
	}
}

// While blocked, nothing reaches the service but the probe.
func TestBlockedClientMakesNoCalls(t *testing.T) {
	srv := newBlockServer(t, 0)
	c, _ := blockTestClient(t, srv)
	ctx := context.Background()

	_ = c.sendHeartbeat(ctx)
	if !c.isBlocked() {
		t.Fatal("not blocked")
	}
	before := srv.calls.Load()

	id := streamer.StreamerRef{StreamerKey: streamer.StreamerKey{Platform: "twitch", Username: "someone"}}
	if err := c.sendHeartbeat(ctx); !errors.Is(err, errBlocked) {
		t.Errorf("heartbeat: %v, want errBlocked", err)
	}
	if _, _, err := c.subscribe(ctx, streamer.StreamerRefs{id}); !errors.Is(err, errBlocked) {
		t.Errorf("subscribe: %v, want errBlocked", err)
	}
	if _, err := c.fetchStatus(ctx, statusOpts{}, id); !errors.Is(err, errBlocked) {
		t.Errorf("status check: %v, want errBlocked", err)
	}
	if err := c.reportLag(ctx, id.StreamerKey); !errors.Is(err, errBlocked) {
		t.Errorf("lag report: %v, want errBlocked", err)
	}
	if _, err := c.fetchIntervals(ctx); !errors.Is(err, errBlocked) {
		t.Errorf("intervals: %v, want errBlocked", err)
	}
	_, err := c.ResolveStreamer(ctx, id)
	var rej *rejectionError
	if !errors.As(err, &rej) || rej.Definitive() || !strings.Contains(rej.Message, BlockedMessage) {
		t.Errorf("preflight: %v, want a non-definitive rejection saying the recorder is blocked", err)
	}

	if n := srv.calls.Load() - before; n != 0 {
		t.Fatalf("%d calls reached the service while blocked", n)
	}
}

// A preflight is where a blocked reply can arrive first.
func TestPreflightBlockedReply(t *testing.T) {
	srv := newBlockServer(t, 0)
	c, _ := blockTestClient(t, srv)

	_, err := c.ResolveStreamer(context.Background(),
		streamer.StreamerRef{StreamerKey: streamer.StreamerKey{Platform: "twitch", Username: "someone"}})
	if !c.isBlocked() {
		t.Fatal("a blocked preflight reply did not block the client")
	}
	if !errors.Is(err, errBlocked) {
		t.Fatalf("preflight: %v, want it to wrap errBlocked", err)
	}
}

func TestBlockedClientNeverReregisters(t *testing.T) {
	srv := newBlockServer(t, 0)
	c, _ := blockTestClient(t, srv)
	_ = c.sendHeartbeat(context.Background())

	c.noteResponse(context.Background(), unauthorized(true))
	if c.reregistering.Load() {
		t.Fatal("a blocked client started a registration")
	}
}

// An address blocked before this recorder registered is refused the
// challenge, and the probe asks for the challenge again.
func TestUnregisteredClientBlockedAtRegistration(t *testing.T) {
	srv := newBlockServer(t, 0)
	c, _ := blockTestClient(t, srv)
	c.clientID = ""

	if err := c.register(context.Background()); !errors.Is(err, errBlocked) {
		t.Fatalf("register: %v, want errBlocked", err)
	}
	if !c.isBlocked() {
		t.Fatal("a blocked registration did not block the client")
	}

	srv.blocked.Store(false)
	if !c.probeBlock(context.Background()) {
		t.Fatal("the probe did not lift the block once the challenge was answered")
	}
	if !c.waitUnblocked(context.Background()) {
		t.Fatal("waitUnblocked did not return after the block lifted")
	}
	if resubscribed(c) {
		t.Fatal("an unregistered client resubscribed; it must register first")
	}
}

func TestProbeLiftsBlock(t *testing.T) {
	srv := newBlockServer(t, time.Now().Add(time.Hour).Unix())
	c, ups := blockTestClient(t, srv)
	ctx := context.Background()
	_ = c.sendHeartbeat(ctx)

	// Still blocked: the probe keeps the block, and picks up a new end.
	later := time.Now().Add(3 * time.Hour).Unix()
	srv.until.Store(later)
	if c.probeBlock(ctx) {
		t.Fatal("the probe lifted a block the service still holds")
	}
	if got := c.blockedUntil().Unix(); got != later {
		t.Fatalf("blockedUntil = %d, want the updated %d", got, later)
	}
	if u := ups.last(t); u.BlockedUntil != later {
		t.Fatalf("reported BlockedUntil %d, want %d", u.BlockedUntil, later)
	}

	srv.blocked.Store(false)
	if !c.probeBlock(ctx) {
		t.Fatal("the probe did not lift the block once the service answered")
	}
	if c.isBlocked() {
		t.Fatal("still blocked after a successful probe")
	}
	if u := ups.last(t); u.Blocked || u.RecommendedVersion != "1.0.0" {
		t.Fatalf("reported %+v, want unblocked with the heartbeat's versions", u)
	}
	if !resubscribed(c) {
		t.Fatal("lifting the block did not resubscribe")
	}
	if err := c.sendHeartbeat(ctx); err != nil {
		t.Fatalf("heartbeat after the block lifted: %v", err)
	}
}

func TestNextProbeWait(t *testing.T) {
	now := time.Now()
	if got := nextProbeWait(time.Time{}, now); got != blockedProbeInterval {
		t.Errorf("permanent: %v, want %v", got, blockedProbeInterval)
	}
	if got := nextProbeWait(now.Add(5*time.Hour), now); got != blockedProbeInterval {
		t.Errorf("ends in 5h: %v, want %v", got, blockedProbeInterval)
	}
	got := nextProbeWait(now.Add(10*time.Minute), now)
	if got < 10*time.Minute || got > 11*time.Minute {
		t.Errorf("ends in 10m: %v, want just after the end", got)
	}
	if got := nextProbeWait(now.Add(-time.Hour), now); got != blockedProbeMin {
		t.Errorf("already ended: %v, want the %v floor", got, blockedProbeMin)
	}
}

func TestIsUpgradeRefused(t *testing.T) {
	if !isUpgradeRefused(errors.New("invalid websocket connection")) {
		t.Error("a refused WebSocket upgrade was not recognised")
	}
	for _, err := range []error{nil, errors.New("i/o timeout"), errors.New("nats: authorization violation")} {
		if isUpgradeRefused(err) {
			t.Errorf("%v read as a refused upgrade", err)
		}
	}
}
