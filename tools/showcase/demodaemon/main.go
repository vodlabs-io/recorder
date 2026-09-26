// Command demodaemon is the Demo daemon: a stand-in Daemon that serves a Demo
// dataset over the Daemon's own API and records nothing. It runs the real API
// service (vlapi) and view-state store (vlstate) over controllers backed by the
// dataset, so the desktop app, `vod` and the showcase harness cannot tell it
// from vlrecd.
//
// Every piece of state it keeps lives in a temporary directory: it never reads
// or writes the real daemon's config, streamers, logins or keychain.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math"
	"math/rand/v2"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/vodlabs-io/recorder/internal/diskspace"
	"github.com/vodlabs-io/recorder/internal/platformauth"
	"github.com/vodlabs-io/recorder/internal/recording"
	"github.com/vodlabs-io/recorder/internal/streamer"
	"github.com/vodlabs-io/recorder/internal/version"
	"github.com/vodlabs-io/recorder/internal/vlapi"
	"github.com/vodlabs-io/recorder/internal/vlconfig"
	"github.com/vodlabs-io/recorder/internal/vllog"
	"github.com/vodlabs-io/recorder/internal/vlstate"
	"github.com/vodlabs-io/recorder/internal/vodindex"
)

type options struct {
	dataset  string
	host     string
	port     int
	wsPort   int
	httpPort int
	now      time.Time
	freeze   bool
}

func main() {
	var o options
	var now string
	flag.StringVar(&o.dataset, "dataset", "tools/showcase/datasets/default", "Demo dataset directory")
	flag.StringVar(&o.host, "host", "127.0.0.1", "listen address")
	flag.IntVar(&o.port, "port", 14222, "NATS port: the desktop app's VLREC_NATS_URL, vod's daemon.port")
	flag.IntVar(&o.wsPort, "ws-port", 14223, "NATS WebSocket port for the browser harness (0 disables)")
	flag.IntVar(&o.httpPort, "http-port", 14224, "HTTP port serving avatar images (0 disables avatars)")
	flag.StringVar(&now, "now", "", "RFC 3339 instant the dataset's offsets count back from (default: the real time)")
	flag.BoolVar(&o.freeze, "freeze", false, "hold live recording stats still, for pixel-stable Shots")
	flag.Parse()

	o.now = time.Now()
	if now != "" {
		t, err := time.Parse(time.RFC3339, now)
		if err != nil {
			log.Fatalf("--now: %v", err)
		}
		o.now = t
	}
	if err := run(o); err != nil {
		log.Fatal(err)
	}
}

func run(o options) error {
	tmp, err := os.MkdirTemp("", "vl-demo-daemon-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	// Before anything in vlconfig resolves a path: every default file the API
	// touches (platform cache, cookie store, …) now lands in tmp.
	os.Setenv("VLREC_CONFIG_DIR", filepath.Join(tmp, "config"))
	os.Setenv("VLREC_CACHE_DIR", filepath.Join(tmp, "cache"))

	ds, err := loadDataset(o.dataset, o.now)
	if err != nil {
		return err
	}
	version.Version = ds.Version

	nc, ns, err := startNATS(o, tmp)
	if err != nil {
		return err
	}
	defer ns.Shutdown()
	defer nc.Close()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	js, err := jetstream.New(nc)
	if err != nil {
		return err
	}
	// The daemon's own bucket, configured as the daemon configures it.
	kv, err := js.CreateOrUpdateKeyValue(ctx, jetstream.KeyValueConfig{
		Bucket:         "streamers",
		History:        1,
		LimitMarkerTTL: time.Minute,
	})
	if err != nil {
		return err
	}

	d := &demo{o: o, ds: ds, kv: kv, store: vlstate.NewStore(kv), started: time.Now()}
	d.store.SetAddedTracker(d)
	if err := d.writeStreamersFile(filepath.Join(tmp, "config", "streamers.toml")); err != nil {
		return err
	}
	d.archive = newArchive(ds)
	d.archive.onDirty = func() { d.bumpVodsRev(ctx) }

	logger, err := vllog.NewFileLogger(vlconfig.LoggerConfig{Level: "warn", Mode: "console"}, "demo")
	if err != nil {
		return err
	}
	signals := vlapi.ConfigSignals{Resubscribe: make(chan struct{}, 1), Reload: make(chan struct{}, 1)}
	statusReq := make(chan streamer.StreamerRef, 16)
	api, err := vlapi.Init(
		nc,
		d.appConfig(),
		d.strFile,
		newSettings(ds),
		d.archive,
		d.archive,
		logger,
		signals,
		statusReq,
		kv,
		intervals,
		streamerFuncs(),
		platformauth.NewStore(platformauth.WithFilePath(filepath.Join(tmp, "cookies.json"))),
		nil,
	)
	if err != nil {
		return err
	}
	api.SetDiskStatusProvider(func(context.Context) []diskspace.Root { return d.roots() })

	if o.httpPort != 0 {
		go d.serveImages()
	}
	d.publishDaemon(ctx)
	d.reconcile(ctx)
	d.publishStreamers(ctx)
	d.publishArchive(ctx)

	log.Printf("Demo daemon serving %s: nats://%s:%d, ws://%s:%d, %d Streamers, %d VODs",
		o.dataset, o.host, o.port, o.host, o.wsPort, len(ds.Streamers), len(ds.VODs))

	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-api.ShutdownCh:
			return nil
		case <-api.ForceShutdownCh:
			return nil
		case <-statusReq:
		case <-signals.Resubscribe:
			d.reloadStreamers(ctx)
		case <-signals.Reload:
			d.reloadStreamers(ctx)
		case <-tick.C:
			if !o.freeze {
				d.publishStats(ctx)
			}
		}
	}
}

// startNATS runs the embedded server the way the daemon does, plus a WebSocket
// listener for the browser harness.
func startNATS(o options, tmp string) (*nats.Conn, *server.Server, error) {
	opts := &server.Options{
		Host:      o.host,
		Port:      o.port,
		JetStream: true,
		StoreDir:  filepath.Join(tmp, "jetstream"),
		NoLog:     true,
		NoSigs:    true,
	}
	if o.wsPort != 0 {
		opts.Websocket = server.WebsocketOpts{Host: o.host, Port: o.wsPort, NoTLS: true}
	}
	ns, err := server.NewServer(opts)
	if err != nil {
		return nil, nil, err
	}
	ns.Start()
	if !ns.ReadyForConnections(5 * time.Second) {
		return nil, nil, fmt.Errorf("NATS did not start on %s:%d (is a daemon already listening there?)", o.host, o.port)
	}
	nc, err := nats.Connect(fmt.Sprintf("nats://%s:%d", o.host, o.port))
	if err != nil {
		ns.Shutdown()
		return nil, nil, err
	}
	return nc, ns, nil
}

type demo struct {
	o       options
	ds      *Dataset
	kv      jetstream.KeyValue
	store   *vlstate.Store
	strFile *vlconfig.StreamersFile
	archive *archive
	started time.Time
}

func (d *demo) appConfig() vlconfig.AppConfig {
	var c vlconfig.AppConfig
	c.Daemon.Host = d.o.host
	c.Daemon.Port = d.o.port
	c.Session.DownloadDir = d.ds.DownloadDir
	c.Session.Variant = "best"
	c.Session.OutputTemplate = vlconfig.DefaultOutputTemplate
	c.Processing.ArchiveDir = d.ds.ArchiveDir
	c.Processing.DeletePartFiles = true
	return c
}

// writeStreamersFile lists the dataset's Streamers the way `vod streamer add`
// would, so the API's streamer endpoints read and edit a real file.
func (d *demo) writeStreamersFile(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		return err
	}
	sf, err := vlconfig.NewStreamersFile(path)
	if err != nil {
		return err
	}
	for _, s := range d.ds.Streamers {
		id := streamer.StreamerRef{StreamerKey: s.Key}
		sf.AddID(id)
		if !s.Record {
			if err := sf.AddRecordOffRule(id); err != nil {
				return err
			}
		}
	}
	d.strFile = sf
	return sf.Write()
}

// reconcile seeds the view docs from the streamers file, as the cloud client
// does after each subscription.
func (d *demo) reconcile(ctx context.Context) {
	var seeds []vlstate.DocSeed
	monitored := 0
	for _, id := range d.strFile.AllIDs() {
		o := d.strFile.OptionsFor(id)
		if o.Monitor {
			monitored++
		}
		seeds = append(seeds, vlstate.DocSeed{ID: id, Opts: vlstate.DocOptions{
			Monitor:         o.Monitor,
			Record:          o.Record,
			Variant:         o.Variant,
			OutputTemplate:  o.OutputTemplate,
			DeletePartFiles: o.DeletePartFiles,
			SplitSize:       o.SplitSize,
			SplitDuration:   o.SplitDuration,
			ArchiveDir:      o.ArchiveDir,
		}})
	}
	d.store.Reconcile(ctx, seeds)
	vlstate.PutCount(ctx, d.kv, "subscription_count", monitored)
}

// reloadStreamers applies an edit made through the API. A Streamer added
// during the run is reported offline, as a first check would find it.
func (d *demo) reloadStreamers(ctx context.Context) {
	if err := d.strFile.Load(); err != nil {
		log.Printf("reload streamers: %v", err)
		return
	}
	d.reconcile(ctx)
	known := map[streamer.StreamerKey]bool{}
	for _, s := range d.ds.Streamers {
		known[s.Key] = true
	}
	for _, id := range d.strFile.AllIDs() {
		if !known[id.StreamerKey] {
			d.store.SetStatus(ctx, streamer.StreamerState{StreamerRef: id, Status: streamer.StatusOffline}, true)
		}
	}
}

// Track reports each Streamer's dataset added time, and now for one added
// during the run. It makes demo a vlstate.AddedTracker.
func (d *demo) Track(_ context.Context, keys []streamer.StreamerKey, now time.Time) map[streamer.StreamerKey]int64 {
	out := make(map[streamer.StreamerKey]int64, len(keys))
	for _, k := range keys {
		out[k] = now.Unix()
		for _, s := range d.ds.Streamers {
			if s.Key == k {
				out[k] = s.Added.Unix()
			}
		}
	}
	return out
}

func (d *demo) avatarURL(k streamer.StreamerKey) string {
	if d.o.httpPort == 0 {
		return ""
	}
	name := k.Platform + "-" + k.Username + ".jpg"
	if _, err := os.Stat(filepath.Join(d.ds.Dir, "images", "avatars", name)); err != nil {
		return ""
	}
	return fmt.Sprintf("http://%s:%d/avatars/%s", d.o.host, d.o.httpPort, name)
}

func (d *demo) serveImages() {
	mux := http.NewServeMux()
	mux.Handle("/avatars/", http.StripPrefix("/avatars/",
		http.FileServer(http.Dir(filepath.Join(d.ds.Dir, "images", "avatars")))))
	addr := fmt.Sprintf("%s:%d", d.o.host, d.o.httpPort)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Printf("avatar server on %s: %v", addr, err)
	}
}

func (d *demo) publishStreamers(ctx context.Context) {
	for _, s := range d.ds.Streamers {
		st := streamer.StreamerState{
			StreamerRef: streamer.StreamerRef{StreamerKey: s.Key},
			DisplayName: s.DisplayName,
			Status:      s.Status,
			LiveMetadata: streamer.LiveMetadata{
				Platform:    s.Key.Platform,
				Title:       s.Title,
				DisplayName: s.DisplayName,
				Category:    s.Category,
			},
			AvatarURL: d.avatarURL(s.Key),
		}
		if !s.LiveStart.IsZero() {
			st.LastLiveStart = s.LiveStart.Unix()
			st.LastLiveDuration = int64(s.LastLiveDuration.Seconds())
		}
		d.store.SetStatus(ctx, st, true)
		if s.SessionID != "" {
			d.store.SetSessionState(ctx, s.Key, s.SessionID, recording.SessionState(s.Session))
		}
	}
	d.publishStats(ctx)
}

// publishStats writes each recording Session's progress. Called every second
// unless frozen, so bytes and duration climb as a real recording's would.
func (d *demo) publishStats(ctx context.Context) {
	elapsed := time.Since(d.started)
	if d.o.freeze {
		elapsed = 0
	}
	workers := 0
	for _, s := range d.ds.Streamers {
		if s.SessionID == "" {
			continue
		}
		workers++
		dur := d.ds.Now.Sub(s.LiveStart) + elapsed
		// A little wobble in the rate, steady per Streamer when frozen.
		wobble := 1.0
		if !d.o.freeze {
			wobble = 0.93 + 0.14*rand.Float64()
		}
		vlstate.PutSessionStats(ctx, d.kv, s.Key, recording.Stats{
			TotalBytes: int64(s.BytesPerS * dur.Seconds()),
			Duration:   dur.Truncate(time.Second),
			Speed:      math.Round(s.BytesPerS * wobble),
			SessionID:  s.SessionID,
		})
	}
	vlstate.PutCount(ctx, d.kv, "worker_count", workers)
}

// publishArchive reflects the VOD Archive's in-flight work onto the Streamers
// and the encode queue.
func (d *demo) publishArchive(ctx context.Context) {
	for _, v := range d.ds.VODs {
		switch v.State {
		case vodindex.StateProcessing:
			vlstate.PutSessionStats(ctx, d.kv, v.Key, recording.Stats{
				TotalBytes: v.Bytes, Duration: v.Duration, SessionID: v.SessionID,
			})
			d.store.SetProcessingState(ctx, v.Key, v.SessionID, recording.ProcessProcessing)
		case vodindex.StateMoving:
			d.store.SetMoveState(ctx, v.Key, v.SessionID, true, v.Reason != "")
		case vodindex.StateEncoding:
			d.store.PutEncodeProgress(ctx, vlstate.EncodeProgress{
				JobID:   1,
				Stage:   "encode",
				Percent: v.Percent,
				EtaSec:  int64(v.Duration.Seconds() * (100 - v.Percent) / 100 / 6),
				Speed:   6.1,
			})
		}
	}
	d.store.PutEncodeQueueRev(ctx)
	d.store.PutEncodePending(ctx, len(d.archive.encodeJobs()))
	d.bumpVodsRev(ctx)
}

func (d *demo) bumpVodsRev(ctx context.Context) {
	d.kv.PutString(ctx, "daemon.vods_rev", strconv.FormatInt(time.Now().UnixNano(), 10))
}

func (d *demo) roots() []diskspace.Root {
	warn := uint64(vlconfig.DefaultLowSpaceWarnGB) << 30
	out := make([]diskspace.Root, 0, len(d.ds.Roots))
	for _, r := range d.ds.Roots {
		state := diskspace.StateOK
		if r.Free < warn {
			state = diskspace.StateLow
		}
		out = append(out, diskspace.Root{
			Paths:    r.Paths,
			Download: r.Download,
			Usage:    diskspace.Usage{TotalBytes: r.Total, FreeBytes: r.Free},
			State:    state,
		})
	}
	return out
}

// publishDaemon writes the daemon-wide keys a healthy, registered, up-to-date
// daemon would. The three JSON documents mirror unexported types in
// internal/cloud and internal/app/daemon.
func (d *demo) publishDaemon(ctx context.Context) {
	d.kv.PutString(ctx, "daemon.start_time", strconv.FormatInt(d.ds.StartTime.Unix(), 10))
	vlstate.PutDaemonVersion(ctx, d.kv, d.ds.Version)
	vlstate.PutUpstreamVersion(ctx, d.kv, version.Upstream{
		MinimumVersion:     "1.0.0",
		RecommendedVersion: d.ds.Version,
	})
	putJSON(ctx, d.kv, "daemon.registration", map[string]any{
		"registered": true, "updated_at": d.ds.StartTime.UTC(),
	})
	putJSON(ctx, d.kv, "daemon.service", map[string]any{
		"reachable": true, "since": d.ds.StartTime.UTC(), "updated_at": d.ds.Now.UTC(),
	})
	putJSON(ctx, d.kv, "daemon.disk_status", map[string]any{
		"roots":      d.roots(),
		"warn_bytes": uint64(vlconfig.DefaultLowSpaceWarnGB) << 30,
		"sampled_at": d.ds.Now.UTC(),
	})
}

func putJSON(ctx context.Context, kv jetstream.KeyValue, key string, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		log.Printf("%s: %v", key, err)
		return
	}
	if _, err := kv.Put(ctx, key, b); err != nil {
		log.Printf("%s: %v", key, err)
	}
}
