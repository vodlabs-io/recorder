package cloud

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/vodlabs-io/recorder/internal/platformauth"
	"github.com/vodlabs-io/recorder/internal/platformlogin"
	"github.com/vodlabs-io/recorder/internal/streamer"
	"github.com/vodlabs-io/recorder/internal/version"
	"github.com/vodlabs-io/recorder/internal/vlconfig"
	"github.com/vodlabs-io/recorder/internal/vllog"
	"github.com/vodlabs-io/recorder/internal/vlstate"
)

const (
	prodNATSEndpoint = "wss://nats.vodlabs.io"
	prodAPIBaseURL   = "https://api.vodlabs.io"
)

// The active cloud endpoints. Overridable via env for local full-stack
// development (e.g. VLREC_CLOUD_API_URL=http://127.0.0.1:8080
// VLREC_CLOUD_NATS_URL=ws://127.0.0.1:8081).
var (
	natsEndpoint = envOr("VLREC_CLOUD_NATS_URL", prodNATSEndpoint)
	apiBaseURL   = strings.TrimRight(envOr("VLREC_CLOUD_API_URL", prodAPIBaseURL), "/")
)

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// APIBaseURL returns the active cloud REST endpoint.
func APIBaseURL() string { return apiBaseURL }

// NATSEndpoint returns the active cloud NATS WebSocket endpoint.
func NATSEndpoint() string { return natsEndpoint }

type versionCheckedCallback func(ctx context.Context, u version.Upstream)

// Client manages the NATS connection and cloud API interactions.
type Client struct {
	nc              *nats.Conn                  // NATS connection
	localNC         *nats.Conn                  // daemon's local NATS connection (browser-context fetches go through it)
	parked          parkedChecks                // checks held for a browser context that is not up
	httpClient      *http.Client                // shared HTTP client for REST API calls and scrape requests
	streamersFile   *vlconfig.StreamersFile     // streamers.toml
	clientID        string                      // Unique ID for this client
	accountID       string                      // Unique ID for the client's account
	subscriptionID  string                      // Unique ID for the subscription
	clientVersion   string                      // Build version
	consumerName    string                      // JetStream consumer name for receiving status updates
	nkeySeed        string                      // NKey seed for authentication
	userJWT         string                      // JWT for authenticating with NATS
	jwtExpiresAt    time.Time                   // Expiration time for the current JWT
	sharedChecks    bool                        // Whether to participate in shared status checks
	isSubscribed    bool                        // Whether the client is currently subscribed via NATS
	initialSubDone  bool                        // Whether the first subscription cycle has concluded (success, no-op or failure)
	subscriptions   atomic.Value                // Currently subscribed streamer IDs
	logger          *vllog.FileLogger           // Logger for client operations
	scraperConfig   vlconfig.ScraperConfig      // Configuration for the scrape handler
	cond            *sync.Cond                  // Condition variable for waiting on subscription
	mu              sync.RWMutex                // Mutex to protect access to client state
	kv              jetstream.KeyValue          // Local KV bucket for storing per-streamer state
	store           *vlstate.Store              // Authoritative per-streamer view docs
	cookies         *platformauth.Store         // Local, encrypted third-party platform logins (PRIVACY: never leaves this machine)
	statusReqCh     chan streamer.StreamerRef   // Queue for outgoing streamer status requests
	StatusReqPriCh  chan streamer.StreamerRef   // High-priority queue for streamer status requests
	authStatusReqCh chan streamer.StreamerRef   // Queue for authenticated re-checks of restricted streamers we hold a login for
	sweep           *loginSweep                 // Schedule for platforms this daemon must poll itself (see status_sweep.go)
	StreamReqCh     chan streamer.StreamerRef   // Queue for mid-recording stream re-signs (paced apart from status checks)
	limiterMu       sync.Mutex                  // Guards limiters
	limiters        map[string]*platformLimiter // Per-platform outgoing request pacing, created on first use
	ceiling         *tokenBucket                // Daemon-wide ceiling every outgoing request passes after its platform's token
	refreshReqMu    sync.Mutex                  // Guards refreshReqAt
	refreshReqAt    map[string]time.Time        // Last login-refresh request per platform (cooldown, see requestLoginRefresh)
	LagReportCh     chan streamer.StreamerKey   // Queue for lag reports from session workers
	StreamerCh      chan streamer.StreamerState // Queue for sending streamer updates to the session registry
	// reregistering is set while a lost identity is being replaced, so several
	// refused calls start one registration between them. See noteResponse.
	reregistering atomic.Bool
	// upgradeRequired is set once the service has refused a call for being too
	// old, so the daemon reports it once rather than on every refusal.
	upgradeRequired atomic.Bool
	// serviceState is the last reachability verdict published, so a repeat of
	// the same one keeps the time it began. See publishServiceState.
	serviceState atomic.Pointer[serviceState]
	// lastUpstream holds what the last heartbeat reported, so a later 426 can
	// say which version to move to.
	lastUpstream      atomic.Pointer[version.Upstream]
	ResubscribeCh     chan struct{}          // Trigger a new subscription: new JWT, new consumer, NATS reconnect. Only for a change to WHICH streamers are tracked
	ReloadCh          chan struct{}          // Re-read streamers.toml and apply it locally. For a change to HOW a tracked streamer is recorded (see reloadConfig)
	standbyCh         chan chan error        // Ask for a connection with no streamers (see requestStandby); answered on the subscription handler's goroutine
	sniffs            *sniffCache            // Recent sniff reports, kept across the scrape handlers each subscription starts
	natsHandlerWg     sync.WaitGroup         // Tracks NATS handler goroutines for resubscription
	fetchWg           sync.WaitGroup         // Tracks the in-flight status fetches serve detaches (see waitFetches)
	stopScrapeHandler context.CancelFunc     // Cancel function to stop the scrape handler goroutine
	stopWatchHandler  context.CancelFunc     // Cancel function to stop the watch handler goroutine
	stopDeltaStreamer context.CancelFunc     // Cancel function to stop the linked-lifetime KV delta streamer (subscription-handler goroutine only)
	stopLinkPoller    context.CancelFunc     // Cancel function to stop the link-confirmation poller
	linkedEmail       string                 // Email of the linked account (display only; guarded by mu)
	lastBridgeAttach  atomic.Int64           // Unix time of the last dashboard keepalive/snapshot request
	OnConfigReloaded  func()                 // Callback invoked after each streamersFile.Load, by both newSubscription and reloadConfig
	onVersionChecked  versionCheckedCallback // Callback invoked after upstream version info arrives from heartbeat handler
	prevStopped       map[string]struct{}    // Snapshot of switched-off IDs from the previous newSubscription; used to detect off→on transitions
	intervals         intervalsCache         // Short-lived cache for GetIntervals (estimated polling intervals)
}

func Init(
	ctx context.Context,
	localNC *nats.Conn,
	kv jetstream.KeyValue,
	store *vlstate.Store,
	sf *vlconfig.StreamersFile,
	httpClient *http.Client,
	cookies *platformauth.Store,
	cloudCfg vlconfig.CloudConfig,
	scraperCfg vlconfig.ScraperConfig,
	onVersionChecked versionCheckedCallback,
) (*Client, error) {
	logger, err := vllog.NewFileLogger(cloudCfg.Logger, "cloud")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to initialize cloud logger: %v\n", err)
		return nil, err
	}
	log := logger.With("func", "Init")

	client := &Client{
		httpClient:       httpClient,
		localNC:          localNC,
		kv:               kv,
		store:            store,
		cookies:          cookies,
		streamersFile:    sf,
		clientVersion:    version.Version,
		logger:           logger,
		scraperConfig:    scraperCfg,
		sharedChecks:     true,
		onVersionChecked: onVersionChecked,
		ResubscribeCh:    make(chan struct{}, 1),
		ReloadCh:         make(chan struct{}, 1),
		standbyCh:        make(chan chan error),
		sniffs:           newSniffCache(),
		// One slot per subscribed streamer: every one of these carries at most
		// one entry per streamer at a time, and the bursts that fill them
		// (a subscription refresh, an enable-all, every tracked streamer
		// turning out to be live at once) are all that size.
		statusReqCh:     make(chan streamer.StreamerRef, streamer.MaxSubscribed),
		StatusReqPriCh:  make(chan streamer.StreamerRef, streamer.MaxSubscribed),
		authStatusReqCh: make(chan streamer.StreamerRef, streamer.MaxSubscribed),
		sweep:           newLoginSweep(),
		StreamReqCh:     make(chan streamer.StreamerRef, streamer.MaxSubscribed),
		LagReportCh:     make(chan streamer.StreamerKey, streamer.MaxSubscribed),
		StreamerCh:      make(chan streamer.StreamerState, streamer.MaxSubscribed),
	}
	client.cond = sync.NewCond(&client.mu)
	client.subscriptions.Store(streamer.StreamerRefs{})

	// Publish the current platform-login (account) status to the local KV so the
	// Accounts UI is populated on startup. The local KV is recreated each run, so
	// this repopulates it from the persistent encrypted cookie store. Local
	// only, so it happens whether or not this daemon has an identity yet.
	if client.cookies != nil {
		if err := platformlogin.PublishAll(ctx, client.kv, client.cookies); err != nil {
			log.Warn("Failed to publish initial account status", "error", err)
		}
	}

	if err := client.loadCredentials(); err != nil {
		log.Info("No existing credentials, registering...")
		if err := client.register(ctx); err != nil {
			// Not fatal: come up anyway, publish the reason, and keep retrying
			// in the background.
			log.Error("Registration failed; the daemon will keep retrying", "error", err)
			client.publishRegistration(ctx, registrationState{
				Error: err.Error(), Attempts: 1,
			})
			go client.retryRegistration(ctx)
			return client, nil
		}
	}

	client.publishRegistration(ctx, registrationState{Registered: true})
	client.startCloudWork(ctx)
	return client, nil
}

// startCloudWork runs the startup that needs an identity: the link state, the
// outgoing request pumps, and the first subscription cycle. Called from Init
// once registered, or later by the registration retry.
func (c *Client) startCloudWork(ctx context.Context) {
	log := c.logger.With("func", "startCloudWork")

	// The daemon-wide ceiling is built before the rate limiter, so every path
	// that spends a token (including a retry that outlives its request) finds it.
	c.ceiling = newTokenBucket(ctx, ceilingLimit)
	go c.rateLimitStatusRequests(ctx)
	go c.handleLagReports(ctx)
	go c.handleHeartbeat(ctx)
	go c.handleSubscription(ctx)
	// A no-op until a login for a login-gated platform is stored.
	go c.runLoginSweep(ctx)

	// Wait for the first subscription cycle to finish. This completes even when
	// there are no streamers to subscribe to.
	c.waitInitialSub()

	if err := c.sendHeartbeat(ctx); err != nil {
		log.Error("Failed to send heartbeat", "error", err)
	}
}

func (c *Client) Close() {
	c.closeConnection()
	c.logger.Close()
}
