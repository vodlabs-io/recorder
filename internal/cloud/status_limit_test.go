package cloud

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/zalando/go-keyring"

	"github.com/vodlabs-io/recorder/internal/platformauth"
	"github.com/vodlabs-io/recorder/internal/platformlogin"
	"github.com/vodlabs-io/recorder/internal/streamer"
	"github.com/vodlabs-io/recorder/internal/vlconfig"
	"github.com/vodlabs-io/recorder/internal/vllog"
	"github.com/vodlabs-io/recorder/internal/vlstate"
)

func TestWaitForRequestPriority(t *testing.T) {
	id := func(u string) streamer.StreamerRef {
		return streamer.StreamerRef{StreamerKey: streamer.StreamerKey{Platform: "tiktok", Username: u}}
	}
	empty := func() chan streamer.StreamerRef { return make(chan streamer.StreamerRef) }

	// Which lane a request came from decides which budget it spends and how it
	// is made, so every lane has to report itself correctly.
	for _, tc := range []struct {
		name string
		lane int // index into the four channels waitForRequest is given
		want requestKind
		// Only the backlog lane reports itself as one: it is what routing keys
		// the per-platform lane off, and mislabelling it there puts a batch in
		// front of the single check the priority lane exists for.
		wantBacklog bool
	}{
		{"high", 0, kindStatus, false},
		{"authed", 1, kindAuthed, false},
		{"stream", 2, kindStream, false},
		{"low", 3, kindStatus, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lanes := [4]chan streamer.StreamerRef{empty(), empty(), empty(), empty()}
			lanes[tc.lane] = make(chan streamer.StreamerRef, 1)
			lanes[tc.lane] <- id(tc.name)

			got, kind, backlog := waitForRequest(context.Background(), lanes[0], lanes[1], lanes[2], lanes[3])
			if got != id(tc.name) || kind != tc.want || backlog != tc.wantBacklog {
				t.Fatalf("got (%v, %v, backlog=%v), want (%v, %v, backlog=%v)",
					got, kind, backlog, id(tc.name), tc.want, tc.wantBacklog)
			}
		})
	}

	// The low lane is a backlog; the other three have something waiting on the
	// answer, so a queued backlog must never be served ahead of them.
	t.Run("priorityLanesBeatTheBacklog", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			lane int
			want requestKind
		}{
			{"high", 0, kindStatus},
			{"authed", 1, kindAuthed},
			{"stream", 2, kindStream},
		} {
			lanes := [4]chan streamer.StreamerRef{empty(), empty(), empty(), make(chan streamer.StreamerRef, 1)}
			lanes[3] <- id("backlog")
			lanes[tc.lane] = make(chan streamer.StreamerRef, 1)
			lanes[tc.lane] <- id(tc.name)

			got, kind, backlog := waitForRequest(context.Background(), lanes[0], lanes[1], lanes[2], lanes[3])
			if got != id(tc.name) || kind != tc.want || backlog {
				t.Errorf("%s: got (%v, %v, backlog=%v), want (%v, %v, backlog=false) — the backlog jumped the queue",
					tc.name, got, kind, backlog, id(tc.name), tc.want)
			}
		}
	})

	// A caller with fewer lanes passes nil, which must simply never be ready
	// rather than panic or be chosen.
	t.Run("nilLaneIsNeverReady", func(t *testing.T) {
		low := make(chan streamer.StreamerRef, 1)
		low <- id("l")
		got, kind, backlog := waitForRequest(context.Background(), empty(), empty(), nil, low)
		if got != id("l") || kind != kindStatus || !backlog {
			t.Fatalf("got (%v, %v, backlog=%v), want (l, status, backlog=true)", got, kind, backlog)
		}
	})

	t.Run("canceledContextReturnsEmpty", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		got, _, _ := waitForRequest(ctx, empty(), empty(), empty(), empty())
		if got != (streamer.StreamerRef{}) {
			t.Fatalf("got %v, want empty", got)
		}
	})
}

// newTestKV spins up an embedded JetStream server with a "streamers" KV bucket
// so dispatchStreamer's view-doc writes have a real backing store. The returned
// connection doubles as the test Client's localNC (refresh-request publishes).
func newTestKV(t *testing.T) (*nats.Conn, jetstream.KeyValue) {
	t.Helper()
	srv, err := server.NewServer(&server.Options{
		Host:      "127.0.0.1",
		Port:      -1,
		JetStream: true,
		StoreDir:  t.TempDir(),
		NoLog:     true,
		NoSigs:    true,
	})
	if err != nil {
		t.Fatalf("nats server: %v", err)
	}
	go srv.Start()
	if !srv.ReadyForConnections(5 * time.Second) {
		t.Fatal("nats server not ready")
	}
	t.Cleanup(srv.Shutdown)

	nc, err := nats.Connect(srv.ClientURL())
	if err != nil {
		t.Fatalf("nats connect: %v", err)
	}
	t.Cleanup(nc.Close)

	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	kv, err := js.CreateKeyValue(context.Background(), jetstream.KeyValueConfig{Bucket: "streamers"})
	if err != nil {
		t.Fatalf("create kv: %v", err)
	}
	return nc, kv
}

// TestHandleFetchResultEscalatesRestricted checks that a "restricted" result
// for a platform with a stored login escalates to an authenticated re-check
// instead of being dispatched.
func TestHandleFetchResultEscalatesRestricted(t *testing.T) {
	keyring.MockInit() // in-process keyring — never touches the real OS keychain
	ctx := context.Background()

	cookies := platformauth.NewStore(
		platformauth.WithFilePath(filepath.Join(t.TempDir(), "cookies.json")),
	)
	if err := cookies.Set(platformauth.Session{
		Platform: "tiktok",
		Cookies:  []platformauth.Cookie{{Name: "sessionid", Value: "test"}},
		Healthy:  true,
	}); err != nil {
		t.Fatalf("seed cookie store: %v", err)
	}

	logger, err := vllog.NewFileLogger(vlconfig.LoggerConfig{Mode: "off", Level: "error"}, "test")
	if err != nil {
		t.Fatalf("logger: %v", err)
	}
	nc, kv := newTestKV(t)
	c := &Client{
		cookies:         cookies,
		kv:              kv,
		store:           vlstate.NewStore(kv),
		logger:          logger,
		localNC:         nc,
		authStatusReqCh: make(chan streamer.StreamerRef, 1),
		StreamerCh:      make(chan streamer.StreamerState, 1),
	}

	restricted := func(platform, user string) streamer.StreamerState {
		return streamer.StreamerState{
			StreamerRef: streamer.StreamerRef{StreamerKey: streamer.StreamerKey{Platform: platform, Username: user}},
			Status:      streamer.StatusRestricted,
		}
	}

	t.Run("plainRestrictedWithLoginEscalates", func(t *testing.T) {
		str := restricted("tiktok", "alice")
		c.handleFetchResult(ctx, str, false)
		select {
		case id := <-c.authStatusReqCh:
			if id != str.StreamerRef {
				t.Fatalf("escalated wrong ID: %v", id)
			}
		default:
			t.Fatal("restricted result with a stored login was not escalated to an authed re-check")
		}
		select {
		case got := <-c.StreamerCh:
			t.Fatalf("escalated result must not also dispatch, got %v", got.StreamerRef)
		default:
		}

		// The escalation may never land (e.g. at startup, before a desktop app
		// is connected), so the wall must still be recorded in the view doc for
		// nudgeRestrictedStreamers to find when the app re-stores its login.
		entry, err := kv.Get(ctx, vlstate.DocKVKey(str.StreamerKey))
		if err != nil {
			t.Fatalf("escalated result must still record the wall in its view doc: %v", err)
		}
		var doc vlstate.StreamerDoc
		if err := json.Unmarshal(entry.Value(), &doc); err != nil {
			t.Fatalf("unmarshal view doc: %v", err)
		}
		if doc.Status != streamer.StatusRestricted {
			t.Fatalf("view doc status = %q, want %q", doc.Status, streamer.StatusRestricted)
		}
	})

	t.Run("plainRestrictedWithoutLoginDispatches", func(t *testing.T) {
		str := restricted("kick", "bob") // no login stored for kick
		c.handleFetchResult(ctx, str, false)
		select {
		case id := <-c.authStatusReqCh:
			t.Fatalf("unexpected escalation without a login: %v", id)
		default:
		}
		select {
		case got := <-c.StreamerCh:
			if got.StreamerRef != str.StreamerRef {
				t.Fatalf("dispatched wrong streamer: %v", got.StreamerRef)
			}
		default:
			t.Fatal("restricted result without a login should dispatch")
		}
	})

	t.Run("authedRestrictedDispatchesWithoutLooping", func(t *testing.T) {
		str := restricted("tiktok", "alice")
		c.handleFetchResult(ctx, str, true)
		select {
		case id := <-c.authStatusReqCh:
			t.Fatalf("authed result must never re-escalate (loop): %v", id)
		default:
		}
		select {
		case got := <-c.StreamerCh:
			if got.StreamerRef != str.StreamerRef {
				t.Fatalf("dispatched wrong streamer: %v", got.StreamerRef)
			}
		default:
			t.Fatal("authed restricted result should dispatch")
		}
	})
}

// TestRequestLoginRefresh asserts an authed check that stays restricted asks a
// running desktop app to refresh the jar (the dominant cause is a stale jar,
// not a dead login): a request is published on local NATS carrying ONLY the
// platform name (PRIVACY: never cookie material), and the per-platform cooldown
// suppresses repeats.
func TestRequestLoginRefresh(t *testing.T) {
	keyring.MockInit()
	ctx := context.Background()

	const secret = "SUPERSECRET_SESSIONID_XYZ789"
	cookies := platformauth.NewStore(
		platformauth.WithFilePath(filepath.Join(t.TempDir(), "cookies.json")),
	)
	if err := cookies.Set(platformauth.Session{
		Platform: "tiktok",
		Cookies:  []platformauth.Cookie{{Name: "sessionid", Value: secret}},
		Healthy:  true,
	}); err != nil {
		t.Fatalf("seed cookie store: %v", err)
	}

	logger, err := vllog.NewFileLogger(vlconfig.LoggerConfig{Mode: "off", Level: "error"}, "test")
	if err != nil {
		t.Fatalf("logger: %v", err)
	}
	nc, kv := newTestKV(t)
	c := &Client{
		cookies:         cookies,
		kv:              kv,
		store:           vlstate.NewStore(kv),
		logger:          logger,
		localNC:         nc,
		authStatusReqCh: make(chan streamer.StreamerRef, 1),
		StreamerCh:      make(chan streamer.StreamerState, 1),
	}

	sub, err := nc.SubscribeSync(platformlogin.RefreshRequestSubject)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	str := streamer.StreamerState{
		StreamerRef: streamer.StreamerRef{StreamerKey: streamer.StreamerKey{Platform: "tiktok", Username: "alice"}},
		Status:      streamer.StatusRestricted,
	}
	c.handleFetchResult(ctx, str, true)
	<-c.StreamerCh // drain the dispatch so the next call has room

	msg, err := sub.NextMsg(2 * time.Second)
	if err != nil {
		t.Fatal("authed still-restricted result did not publish a refresh request")
	}
	var req platformlogin.RefreshRequest
	if err := json.Unmarshal(msg.Data, &req); err != nil {
		t.Fatalf("unmarshal refresh request: %v", err)
	}
	if req.Platform != "tiktok" {
		t.Fatalf("refresh request for wrong platform: %q", req.Platform)
	}
	// PRIVACY: the request must carry only the platform name.
	if strings.Contains(string(msg.Data), secret) {
		t.Fatalf("SECRET COOKIE VALUE LEAKED into refresh request: %s", msg.Data)
	}

	// A second still-restricted result inside the cooldown window is suppressed.
	c.handleFetchResult(ctx, str, true)
	<-c.StreamerCh
	if msg, err := sub.NextMsg(300 * time.Millisecond); err == nil {
		t.Fatalf("cooldown violated: second refresh request published: %s", msg.Data)
	}

	// The still-restricted outcome must not have touched the login's health.
	sess, ok := cookies.Get("tiktok")
	if !ok || !sess.Status().Healthy {
		t.Fatal("still-restricted authed check must not mark the login unhealthy (stale jar ≠ dead login)")
	}

	// After sign-out, an in-flight authed check completing late must NOT
	// request a refresh — that would resurrect the removed login. (Zero out the
	// cooldown so only the has-login guard is in play.)
	if err := cookies.Remove("tiktok"); err != nil {
		t.Fatalf("remove login: %v", err)
	}
	c.refreshReqMu.Lock()
	c.refreshReqAt = nil
	c.refreshReqMu.Unlock()
	c.handleFetchResult(ctx, str, true)
	<-c.StreamerCh
	if msg, err := sub.NextMsg(300 * time.Millisecond); err == nil {
		t.Fatalf("refresh requested for a signed-out login: %s", msg.Data)
	}
}

// A check that fails while its platform is holding off must run again after
// the hold, not be dropped: the cloud pushes an update only when a status
// changes, so a streamer that stays live would never be checked again.
func TestServeRequeuesCheckFailedDuringHold(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	origURL, origBackoff := apiBaseURL, statusFetchRetryBackoff
	apiBaseURL = srv.URL
	statusFetchRetryBackoff = time.Millisecond
	t.Cleanup(func() { apiBaseURL, statusFetchRetryBackoff = origURL, origBackoff })

	logger, err := vllog.NewFileLogger(vlconfig.LoggerConfig{Mode: "off", Level: "error"}, "test")
	if err != nil {
		t.Fatalf("logger: %v", err)
	}
	c := &Client{
		logger:     logger,
		httpClient: srv.Client(),
		ceiling:    newTokenBucket(t.Context(), vlconfig.RateLimit{Burst: 64, Interval: time.Millisecond}),
	}
	log := logger.With("func", "test")
	id := streamer.StreamerRef{
		StreamerKey: streamer.StreamerKey{Platform: "niconico", Username: "595347"},
	}
	newLimiter := func() *platformLimiter {
		return &platformLimiter{
			platform: "niconico",
			highPri:  make(chan streamer.StreamerRef, laneDepth),
			authPri:  make(chan streamer.StreamerRef, laneDepth),
			lowPri:   make(chan streamer.StreamerRef, laneDepth),
			stream:   make(chan streamer.StreamerRef, laneDepth),
			status:   newTokenBucket(t.Context(), vlconfig.RateLimit{Burst: 64, Interval: time.Hour}),
			streams:  newTokenBucket(t.Context(), vlconfig.RateLimit{Burst: 64, Interval: time.Hour}),
			resumed:  make(chan struct{}),
		}
	}
	// requeued reports what landed back in the lane for this kind, and checks
	// the hold is still in force so the lane cannot run it straight away.
	requeued := func(t *testing.T, l *platformLimiter, lane chan streamer.StreamerRef) {
		t.Helper()
		select {
		case got := <-lane:
			if got != id {
				t.Fatalf("re-queued %v, want %v", got, id)
			}
		default:
			t.Fatal("a check that failed during a hold was dropped instead of re-queued")
		}
		if wait, _ := l.hold(time.Now()); wait == 0 {
			t.Fatal("the platform is not holding off, so the re-queued check would run at once")
		}
	}

	t.Run("failureThatStartsTheHold", func(t *testing.T) {
		l := newLimiter()
		// One failure short of a hold, so the served check is the one that trips it.
		for range laneFailureThreshold - 1 {
			l.noteFailure(time.Now())
		}
		c.serve(t.Context(), log, l, id, kindStatus)
		c.waitFetches()
		requeued(t, l, l.highPri)
	})

	t.Run("retryThatHitsTheHold", func(t *testing.T) {
		l := newLimiter()
		// The first attempt failed outside a hold; a retry is the one that trips it.
		for range laneFailureThreshold - 1 {
			l.noteFailure(time.Now())
		}
		c.retryStatusFetch(t.Context(), log, l, id, kindAuthed)
		requeued(t, l, l.authPri)
	})

}
