package daemon

import (
	"context"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/knadh/koanf/v2"
	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/vodlabs-io/recorder/internal/cloud"
	"github.com/vodlabs-io/recorder/internal/encoding"
	"github.com/vodlabs-io/recorder/internal/platformauth"
	"github.com/vodlabs-io/recorder/internal/processing"
	"github.com/vodlabs-io/recorder/internal/session"
	"github.com/vodlabs-io/recorder/internal/streamerdb"
	"github.com/vodlabs-io/recorder/internal/thumbcache"
	"github.com/vodlabs-io/recorder/internal/vlapi"
	"github.com/vodlabs-io/recorder/internal/vlconfig"
	"github.com/vodlabs-io/recorder/internal/vllog"
	"github.com/vodlabs-io/recorder/internal/vlstate"
	"github.com/vodlabs-io/recorder/internal/vodindex"
	"github.com/vodlabs-io/recorder/internal/webpage"
	"github.com/vodlabs-io/recorder/internal/webui"
)

type App struct {
	k           *koanf.Koanf
	cfgMu       sync.RWMutex // Guards the user-facing appCfg fields that config.set live-updates.
	cfgFileMu   sync.Mutex   // Serializes rewrites of config.toml.
	appCfg      vlconfig.AppConfig
	log         *vllog.FileLogger
	natsConn    *nats.Conn
	natsServer  *server.Server // embedded NATS server
	jsDataPath  string         // JetStream data directory path
	natsKV      jetstream.KeyValue
	store       *vlstate.Store // Authoritative per-streamer view docs
	apiSvc      *vlapi.Service
	procSvc     *processing.Service
	encSvc      *encoding.Service    // Merge/re-encode job queue
	streamerDB  *streamerdb.DB       // Per-streamer facts that outlive the daemon
	vodIndex    *vodindex.Index      // Cached VOD Archive scans
	vodReady    atomic.Bool          // vodIndex is set; the Web UI starts before it
	revMu       sync.Mutex           // Guards the vods-rev coalescing fields below
	revTimer    *time.Timer          // Armed to publish revPending when the window closes
	revPending  int64                // Newest rev not yet published, 0 = none
	revSentAt   time.Time            // When a rev was last published
	thumbCache  *thumbcache.Cache    // VOD screenshots, kept off the archive volume
	sheets      sheetFlight          // one contact sheet build per session
	migrating   atomic.Bool          // a pass moving flat sidecars into previews/ is running
	migrated    map[string]time.Time // folders that pass tried, by their DirMod then
	cloudClient *cloud.Client
	pages       *webpage.Resolver // finds what plays on pages no platform claims, on this machine
	webSrv      *webui.Server     // Web UI listener, when [web] is enabled
	mediaMu     sync.Mutex        // Guards localMedia
	localMedia  *webui.LocalMedia // Desktop player's loopback listener, once asked for
	sessionReg  *session.Registry
	strFile     *vlconfig.StreamersFile // streamers.toml
	cookieStore *platformauth.Store     // Local, encrypted third-party platform logins
	errCh       chan error              // Fatal error channel
	// flushReports sends queued error reports; set by startErrorReports.
	flushReports func()
}

func New(ctx context.Context) (*App, error) {
	k := koanf.New(".")

	// Before loading the config, which writes a default one on first run.
	for _, dir := range []string{configDir(), cacheDir()} {
		if err := checkContainerDir(dir); err != nil {
			return nil, err
		}
	}

	fs, err := parseFlags()
	if err != nil {
		return nil, err
	}

	cfg, err := loadConfig(k, fs)
	if err != nil {
		return nil, err
	}
	if err := checkContainerDir(cfg.Session.DownloadDir); err != nil {
		return nil, err
	}

	return &App{
		k:      k,
		appCfg: cfg,
		errCh:  make(chan error, 1),
	}, nil
}

func (a *App) cleanup() {
	if a.flushReports != nil {
		defer a.flushReports()
	}
	if a.log != nil {
		a.log.Info("Shutting down")
		defer a.log.Close()
		defer a.log.Info("Daemon stopped")
	}
	if a.webSrv != nil {
		a.webSrv.Close()
	}
	a.mediaMu.Lock()
	if a.localMedia != nil {
		a.localMedia.Close()
	}
	a.mediaMu.Unlock()
	if a.sessionReg != nil {
		a.sessionReg.Close()
	}
	if a.cloudClient != nil {
		a.cloudClient.Close()
	}
	if a.encSvc != nil {
		// Cancels any in-flight encode; the job requeues itself and resumes
		// (staged output preserved) on the next daemon start.
		a.encSvc.Close()
	}
	if a.procSvc != nil {
		a.procSvc.Close()
	}
	if a.streamerDB != nil {
		a.streamerDB.Close()
	}
	if a.natsConn != nil {
		a.natsConn.Drain()
	}
	if a.natsServer != nil {
		a.natsServer.Shutdown()
	}
	if a.jsDataPath != "" {
		os.RemoveAll(a.jsDataPath)
	}
}
