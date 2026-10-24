package daemon

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/vodlabs-io/recorder/internal/cloud"
	"github.com/vodlabs-io/recorder/internal/encoding"
	"github.com/vodlabs-io/recorder/internal/platformauth"
	"github.com/vodlabs-io/recorder/internal/processing"
	"github.com/vodlabs-io/recorder/internal/session"
	"github.com/vodlabs-io/recorder/internal/streamerdb"
	"github.com/vodlabs-io/recorder/internal/version"
	"github.com/vodlabs-io/recorder/internal/vlapi"
	"github.com/vodlabs-io/recorder/internal/vlconfig"
	"github.com/vodlabs-io/recorder/internal/vllog"
	"github.com/vodlabs-io/recorder/internal/vlstate"
	"github.com/vodlabs-io/recorder/internal/webpage"
	"github.com/vodlabs-io/recorder/internal/writer/flv"
)

func (a *App) start(ctx context.Context) (err error) {
	a.log, err = vllog.NewFileLogger(a.appCfg.Daemon.Logger, "daemon")
	if err != nil {
		return fmt.Errorf("failed to initialize main logger: %w", err)
	}
	a.log.Info("Starting", "version", version.Version)
	a.log.Info("Config loaded", "path", a.appCfg.Daemon.ConfigFilePath)

	// Validate global split settings; invalid values are ignored rather than fatal.
	if a.appCfg.Session.SplitSize != "" {
		if _, err := vlconfig.ParseSplitSize(a.appCfg.Session.SplitSize); err != nil {
			a.log.Warn("Ignoring invalid [session] split_size", "value", a.appCfg.Session.SplitSize, "error", err)
			a.appCfg.Session.SplitSize = ""
		}
	}
	if a.appCfg.Session.SplitDuration != "" {
		if _, err := vlconfig.ParseSplitDuration(a.appCfg.Session.SplitDuration); err != nil {
			a.log.Warn("Ignoring invalid [session] split_duration", "value", a.appCfg.Session.SplitDuration, "error", err)
			a.appCfg.Session.SplitDuration = ""
		}
	}

	// Validate the global archive folder; an invalid or overlapping path is
	// disabled rather than fatal.
	if a.appCfg.Processing.ArchiveDir != "" {
		if err := vlconfig.ValidateArchiveDir(a.appCfg.Processing.ArchiveDir); err != nil {
			a.log.Warn("Ignoring invalid [processing] archive_dir", "value", a.appCfg.Processing.ArchiveDir, "error", err)
			a.appCfg.Processing.ArchiveDir = ""
		} else if err := vlconfig.ValidateArchiveDirAgainstDownloadDir(a.appCfg.Processing.ArchiveDir, a.appCfg.Session.DownloadDir); err != nil {
			a.log.Warn("Ignoring [processing] archive_dir overlapping download folder", "value", a.appCfg.Processing.ArchiveDir, "error", err)
			a.appCfg.Processing.ArchiveDir = ""
		}
	}

	// Create the configured folders now rather than leaving them to the first
	// recording and the first move, so a fresh install doesn't report its own
	// folders as unreachable Storage roots. Best effort: if a path really is
	// bad, the disk status says so, which is the honest answer.
	if err := vlconfig.EnsureStorageDir(a.appCfg.Session.DownloadDir); err != nil {
		a.log.Warn("Could not create the download folder",
			"path", a.appCfg.Session.DownloadDir, "error", err)
	}
	if a.appCfg.Processing.ArchiveDir != "" {
		if err := vlconfig.EnsureStorageDir(a.appCfg.Processing.ArchiveDir); err != nil {
			// A hand-edited config file skips the validation config.set runs, so
			// this is the first time an archive folder set that way is checked.
			// Report it the same way: the reason a folder is unwritable, and
			// what to do about it, differ by deployment.
			if werr := vlapi.CheckArchiveDirWritable(a.appCfg.Processing.ArchiveDir); werr != nil {
				err = werr
			}
			// Kept rather than disabled: an archive folder is often on a drive
			// or a NAS that is simply not up yet, and dropping the setting would
			// silently leave those recordings in the download folder. Moves
			// retry until it appears (see internal/vodmove).
			a.log.Warn("The archive folder is not usable yet; moves will retry until it is",
				"path", a.appCfg.Processing.ArchiveDir, "error", err)
		}
	}

	a.natsConn, a.natsServer, a.jsDataPath, err = initNATS(
		a.appCfg.Daemon.Host, a.appCfg.Daemon.Port)
	if err != nil {
		return fmt.Errorf("failed to initialize NATS: %w", err)
	}
	a.log.Info("NATS server started", "host", a.appCfg.Daemon.Host,
		"port", a.appCfg.Daemon.Port)

	a.natsKV, err = initKV(ctx, a.natsConn)
	if err != nil {
		return fmt.Errorf("failed to initialize KV: %w", err)
	}

	// Before anything that reaches the network: startup can stall on a slow or
	// unreachable API, which is exactly when the Admin needs a UI to look at.
	if err := a.startWebUI(); err != nil {
		return err
	}

	if _, err := a.natsKV.PutString(ctx, "daemon.start_time",
		strconv.FormatInt(time.Now().Unix(), 10)); err != nil {
		a.log.Warn("Failed to write daemon.start_time", "error", err)
	}

	if err := vlstate.PutDaemonVersion(ctx, a.natsKV, version.Version); err != nil {
		a.log.Warn("Failed to write daemon version to KV", "error", err)
	}

	a.store = vlstate.NewStore(a.natsKV)

	a.streamerDB, err = streamerdb.Open(ctx, a.log, a.appCfg.Processing.DBPath)
	if err != nil {
		return fmt.Errorf("failed to open streamer database: %w", err)
	}
	a.store.SetAddedTracker(a.streamerDB)

	a.procSvc, err = processing.Init(ctx, a.appCfg.Processing, a.store, a.onVodProcessed, a.resolveDeletePartFiles, a.resolveArchiveDir)
	if err != nil {
		return fmt.Errorf("failed to initialize processing service: %w", err)
	}

	a.encSvc, err = encoding.Init(ctx, a.appCfg.Processing, a.store)
	if err != nil {
		return fmt.Errorf("failed to initialize encoding service: %w", err)
	}

	// Load the last-known-good supported-platform list (seeded from the embedded
	// default on first run), so there is a sensible list before anything reads
	// one. The heartbeat refreshes it shortly after startup.
	if err := vlconfig.EnsurePlatformsCacheExists(); err != nil {
		a.log.Warn("Failed to ensure platforms cache", "error", err)
	}
	if cached, err := vlconfig.LoadPlatformsCache(); err != nil {
		a.log.Warn("Failed to load platforms cache", "error", err)
	} else if len(cached) > 0 {
		vlconfig.SetPlatforms(cached)
	}

	a.strFile, err = vlconfig.NewStreamersFile(a.appCfg.Daemon.StreamersFilePath)
	if err != nil {
		return fmt.Errorf("failed to load streamers: %w", err)
	}

	httpClient := &http.Client{Timeout: 10 * time.Second}

	// The cookie store holds the user's third-party platform logins locally.
	// PRIVACY: this data never leaves the machine (see internal/platformauth).
	a.cookieStore = platformauth.NewStore()

	// Pages no platform claims are checked here, never by the cloud; the
	// desktop app, when running, loads them over local NATS.
	a.pages = webpage.New(a.natsConn, a.log.With("component", "webpage"))

	a.cloudClient, err = cloud.Init(
		ctx,
		a.natsConn,
		a.natsKV,
		a.store,
		a.strFile,
		httpClient,
		a.cookieStore,
		a.appCfg.Cloud,
		a.appCfg.Scraper,
		a.pages,
		a.onVersionChecked)
	if err != nil {
		return fmt.Errorf("failed to initialize cloud client: %w", err)
	}

	a.sessionReg, err = session.Init(
		ctx,
		a.appCfg.Session,
		a.natsKV,
		a.store,
		httpClient,
		flv.Client(),
		a.cloudClient.LagReportCh,
		a.cloudClient.StreamReqCh,
		a.cloudClient.StreamerCh,
		a.procSvc.PendingRecordings,
		a.errCh,
		a.strFile,
		a.cookieStore)
	if err != nil {
		return fmt.Errorf("failed to initialize session registry: %w", err)
	}
	a.cloudClient.OnConfigReloaded = a.sessionReg.ApplyRecordState

	// The VOD Archive index needs procSvc (change events, active work) and
	// sessionReg (suppressing still-recording sessions), both live by now.
	a.initVODIndex()
	a.vodReady.Store(true)

	// Off the startup path: the roots can be network volumes, and nothing waits
	// on reclaiming space from a delete that was already reported done.
	go a.sweepVODTrash()

	a.apiSvc, err = vlapi.Init(
		a.natsConn,
		a.appCfg,
		a.strFile,
		a,
		a,
		a,
		a.log,
		vlapi.ConfigSignals{
			Resubscribe: a.cloudClient.ResubscribeCh,
			Reload:      a.cloudClient.ReloadCh,
		},
		a.cloudClient.StatusReqPriCh,
		a.natsKV,
		a.cloudIntervals,
		a.cloudStreamerFuncs(),
		a.cookieStore,
		a.streamerDB)
	if err != nil {
		return fmt.Errorf("failed to initialize API service: %w", err)
	}

	// Before the first checks run, so one refused for want of a browser context
	// is already parked against a desktop app that may connect later.
	a.cloudClient.WatchBrowserContext(ctx)

	// Not fatal, and retried in the background: see InitStatusRetrying.
	a.cloudClient.InitStatusRetrying(ctx)

	go a.watchStreamerConfigFile(ctx)

	// Storage-root free space: published for every client over the existing
	// "daemon.>" KV watch, and sampled on demand for `vod info`.
	a.apiSvc.SetDiskStatusProvider(a.diskRoots)
	go a.runDiskMonitor(ctx)

	return nil
}

func initNATS(host string, port int) (*nats.Conn, *server.Server, string, error) {
	// Single instance check: try to bind the port first
	ln, err := net.Listen("tcp", fmt.Sprintf("%s:%d", host, port))
	if err != nil {
		return nil, nil, "", err
	}
	ln.Close()

	// Create temp dir for JetStream storage
	storeDir, err := os.MkdirTemp("", "vlrec-nats-*")
	if err != nil {
		return nil, nil, "", fmt.Errorf("failed to create temp dir: %w", err)
	}

	// Start embedded NATS server with JetStream
	opts := &server.Options{
		Host:      host,
		Port:      port,
		JetStream: true,
		StoreDir:  filepath.Join(storeDir, "jetstream"),
		NoLog:     true,
		NoSigs:    true,
	}
	ns, err := server.NewServer(opts)
	if err != nil {
		return nil, nil, "", fmt.Errorf("failed to create NATS server: %w", err)
	}
	ns.Start()
	if !ns.ReadyForConnections(5 * time.Second) {
		return nil, nil, "", fmt.Errorf("NATS server failed to start")
	}

	nc, err := nats.Connect(fmt.Sprintf("nats://%s:%d", host, port))
	if err != nil {
		return nil, nil, "", fmt.Errorf("failed to connect to NATS: %w", err)
	}
	return nc, ns, storeDir, nil
}

func initKV(ctx context.Context, nc *nats.Conn) (jetstream.KeyValue, error) {
	js, err := jetstream.New(nc)
	if err != nil {
		return nil, fmt.Errorf("failed to create JetStream context: %w", err)
	}

	// Create or get KV bucket for streamers
	kv, err := js.CreateOrUpdateKeyValue(ctx, jetstream.KeyValueConfig{
		Bucket:         "streamers",
		History:        1,
		LimitMarkerTTL: 1 * time.Minute,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create KV bucket: %w", err)
	}

	return kv, nil
}
