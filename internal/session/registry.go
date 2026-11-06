package session

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/vodlabs-io/recorder/internal/diskspace"
	"github.com/vodlabs-io/recorder/internal/platformauth"
	"github.com/vodlabs-io/recorder/internal/recording"
	"github.com/vodlabs-io/recorder/internal/streamer"
	"github.com/vodlabs-io/recorder/internal/vlconfig"
	"github.com/vodlabs-io/recorder/internal/vlerror"
	"github.com/vodlabs-io/recorder/internal/vllog"
	"github.com/vodlabs-io/recorder/internal/vlstate"
)

// Registry manages the active Sessions (workers) for streamers.
type Registry struct {
	workers       map[streamer.StreamerKey]*worker
	mu            sync.RWMutex                  // Protects access to the workers map
	wg            sync.WaitGroup                // Waits for all active workers to finish
	logger        *vllog.FileLogger             // High-level registry logs (e.g. worker start/end)
	streamerCh    <-chan streamer.StreamerState // Receives streamer updates from the cloud client
	fatalErrCh    chan error                    // Signals an unrecoverable session error to the daemon
	streamersFile *vlconfig.StreamersFile       // Source of per-streamer rule options (record, ...)

	// Dependencies for creating new workers
	lagReportCh         chan<- streamer.StreamerKey
	streamReqCh         chan<- streamer.StreamerRef
	pendingRecordingsCh chan<- recording.Job
	basicHTTPClient     *http.Client
	flvHTTPClient       *http.Client
	cookies             *platformauth.Store // Local platform logins for authenticated stream downloads
	cfgMu               sync.RWMutex        // Guards cfg, which config.set live-updates.
	cfg                 vlconfig.SessionConfig
	kv                  jetstream.KeyValue
	store               *vlstate.Store
	// sampler reads free space at the download folder, gating session starts.
	// Injected so an out-of-space disk is an ordinary test input.
	sampler diskspace.Sampler
	// resume keeps session checkpoints and the interrupted sessions; nil when
	// its folder could not be opened, which disables resuming.
	resume *resumeStore
	// lifeMu and closed fence work that sends to pendingRecordingsCh outside a
	// worker against Wait, after which the channel is closed.
	lifeMu sync.Mutex
	closed bool
}

// Init initializes the session registry and starts the dispatcher goroutine.
func Init(
	ctx context.Context,
	cfg vlconfig.SessionConfig,
	kv jetstream.KeyValue,
	store *vlstate.Store,
	basicHttpClient *http.Client,
	flvHttpClient *http.Client,
	lagReportCh chan<- streamer.StreamerKey,
	streamReqCh chan<- streamer.StreamerRef,
	streamerCh <-chan streamer.StreamerState,
	pendingRecordingsCh chan<- recording.Job,
	fatalErrCh chan error,
	streamersFile *vlconfig.StreamersFile,
	cookies *platformauth.Store,
) (*Registry, error) {
	logger, err := vllog.NewFileLogger(cfg.Logger, "session")
	if err != nil {
		return nil, fmt.Errorf("failed to initialize logger: %w", err)
	}

	reg := &Registry{
		workers:             make(map[streamer.StreamerKey]*worker),
		logger:              logger,
		streamerCh:          streamerCh,
		fatalErrCh:          fatalErrCh,
		lagReportCh:         lagReportCh,
		streamReqCh:         streamReqCh,
		pendingRecordingsCh: pendingRecordingsCh,
		basicHTTPClient:     basicHttpClient,
		flvHTTPClient:       flvHttpClient,
		cookies:             cookies,
		cfg:                 cfg,
		kv:                  kv,
		store:               store,
		streamersFile:       streamersFile,
		sampler:             diskspace.System(),
	}
	reg.openResume(vlconfig.DefaultFilePath(checkpointDirName))
	go reg.runDispatcher(ctx)
	reg.wg.Go(func() { reg.runResumeSweep(ctx) })
	logger.Info("Started dispatcher")
	return reg, nil
}

// runDispatcher listens for streamer updates and sends them to the appropriate worker.
func (r *Registry) runDispatcher(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case str, ok := <-r.streamerCh:
			if !ok {
				return
			}
			if err := r.dispatch(ctx, str); err != nil {
				r.logger.Error("Failed to dispatch streamer update",
					"error", err,
					"streamer", str.StreamerRef)
			}
		}
	}
}

// dispatch sends a streamer update to the appropriate worker, creating a new worker if necessary.
func (r *Registry) dispatch(ctx context.Context, str streamer.StreamerState) error {
	r.logger.Debug("Dispatching streamer update", "streamer", str.StreamerRef)

	if !str.StreamerKey.Valid() {
		return fmt.Errorf("invalid streamer key")
	}
	log := r.logger.With("streamer", str.StreamerKey, "status", str.Status)

	r.mu.RLock()
	worker, ok := r.workers[str.StreamerKey]
	r.mu.RUnlock()

	if !ok {
		log.Debug("No worker found for streamer")

		r.settleInterrupted(ctx, str)

		if str.Status == streamer.StatusRemoved {
			// No active session to cancel; just purge any lingering KV state so
			// the removed streamer disappears from clients.
			r.store.Delete(ctx, str.StreamerKey)
			if err := vlstate.DeleteStreamer(ctx, r.kv, str.StreamerKey); err != nil {
				log.Error("Failed to purge removed streamer KV", "error", err)
			}
			return nil
		}

		if !str.Recordable() {
			log.Debug("Streamer is not recordable, skipping worker creation",
				"streams_count", len(str.Streams))
			return nil
		}

		opts := r.streamersFile.OptionsFor(str.StreamerRef)
		// A purged one-shot's option overrides live in the KV one-shot list,
		// not the file; fields the file still sets (a fresher edit) win.
		if osOpts, ok := vlstate.GetOneShotOptions(ctx, r.kv, str.StreamerKey); ok {
			if opts.Variant == "" {
				opts.Variant = osOpts.Variant
			}
			if opts.OutputTemplate == "" {
				opts.OutputTemplate = osOpts.OutputTemplate
			}
			if opts.SplitSize == "" {
				opts.SplitSize = osOpts.SplitSize
			}
			if opts.SplitDuration == "" {
				opts.SplitDuration = osOpts.SplitDuration
			}
		}
		if !opts.Record {
			log.Debug("Streamer is not set to record, skipping worker creation")
			return nil
		}

		// Refuse rather than start a recording the disk cannot hold. The
		// Streamer stays monitored and is reconsidered on its next update, so
		// freeing space resumes recording without any further action.
		if !r.canStartSession(ctx, str.StreamerKey) {
			r.store.SetBlockedNoSpace(ctx, str.StreamerKey, true)
			return nil
		}
		r.store.SetBlockedNoSpace(ctx, str.StreamerKey, false)

		cfg := r.sessionCfg()
		if opts.Variant != "" {
			cfg.Variant = opts.Variant
		}
		if opts.OutputTemplate != "" {
			cfg.OutputTemplate = opts.OutputTemplate
		}
		if opts.SplitSize != "" {
			cfg.SplitSize = opts.SplitSize
		}
		if opts.SplitDuration != "" {
			cfg.SplitDuration = opts.SplitDuration
		}

		worker = r.newSessionWorker(cfg)
		resumed := false
		if cp := r.takeResumable(str); cp != nil {
			if err := worker.adopt(cp); err != nil {
				log.Warn("Could not resume the interrupted session; starting a new one", "error", err)
				r.finishInterrupted(cp, "it could not be resumed: "+err.Error())
				worker = r.newSessionWorker(cfg)
			} else {
				resumed = true
				r.store.ClearInterrupted(ctx, str.StreamerKey, cp.SessionID)
			}
		}

		workerCtx, cancel := context.WithCancel(ctx)
		worker.setCancel(cancel)

		if !resumed {
			if err := worker.initSession(str); err != nil {
				worker.cancelSession()
				return fmt.Errorf("failed to initialize session: %w", err)
			}
		}
		if err := r.register(ctx, worker, str.StreamerKey); err != nil {
			worker.cancelSession()
			return fmt.Errorf("failed to register new worker: %w", err)
		}

		worker.str.Store(&str)
		vlstate.PersistOneShot(ctx, r.kv, worker.strKey)
		r.wg.Go(func() { r.runWorker(workerCtx, worker) })

		msg := "Session started"
		if resumed {
			msg = "Session resumed"
		}
		log.Info(msg,
			"session_id", worker.SessionID,
			"variant", worker.config.Variant,
			"out_dir", worker.output.BaseOutDir,
		)
	} else if str.Status == streamer.StatusRemoved {
		log.Debug("Streamer marked as removed, cancelling session")
		// Record the removal so runWorker purges the streamer's KV once the
		// worker has finished tearing down (after its final state writes).
		worker.str.Store(&str)
		worker.stopWith(stopRemoved)
	} else {
		worker.str.Store(&str)

		// Notify the worker that a new update is available
		select {
		case worker.notifyCh <- struct{}{}:
		default:
		}
		// And the metadata log separately: the run loop reads notifyCh only
		// between parts, while a description of the broadcast is worth
		// recording the moment it arrives.
		select {
		case worker.metaCh <- struct{}{}:
		default:
		}
	}

	return nil
}

func (r *Registry) runWorker(ctx context.Context, worker *worker) {
	err := worker.Run(ctx)

	strStatus := worker.str.Load().Status
	log := r.logger.With(
		"streamer", worker.strKey,
		"status", strStatus,
		"session_id", worker.SessionID,
		"variant", worker.config.Variant,
		"out_dir", worker.output.BaseOutDir,
		"total_bytes", worker.output.Stats.TotalBytes,
		"duration_ms", worker.output.Stats.Duration.Milliseconds(),
		"speed", worker.output.Stats.Speed,
	)

	bgCtx := context.Background()
	switch {
	case ctx.Err() != nil && worker.interrupted:
		log.Info("Session interrupted", "reason", worker.stopReason())
		r.store.SetSessionState(bgCtx, worker.strKey, worker.SessionID, recording.SessionInterrupted)
		if cp := r.resume.peek(worker.strKey); cp != nil {
			r.store.SetInterrupted(bgCtx, worker.strKey, cp.SessionID, cp.resumeBy(r.resumeWindow()))
		}
	case ctx.Err() != nil:
		if strStatus == streamer.StatusRemoved {
			// Streamer was removed; its KV is purged below, so don't persist a
			// session state that would only be orphaned.
			log.Info("Session cancelled (streamer removed)")
		} else {
			log.Info("Session cancelled")
			r.store.SetSessionState(bgCtx, worker.strKey, worker.SessionID, recording.SessionCancelled)
		}
	case err != nil:
		log.Error("Session failed", "error", err)
		if errors.Is(err, vlerror.ErrWriteFailed) {
			// Likely disk full or permission error. Signal the daemon to shut down.
			select {
			case r.fatalErrCh <- err:
			default:
			}
		}
		switch strStatus {
		case streamer.StatusInvalid, streamer.StatusUnavailable, streamer.StatusDefunct:
			r.store.SetSessionState(bgCtx, worker.strKey, worker.SessionID, recording.SessionInvalid)
		case streamer.StatusRestricted:
			r.store.SetSessionState(bgCtx, worker.strKey, worker.SessionID, recording.SessionRestricted)
		default:
			r.store.SetSessionState(bgCtx, worker.strKey, worker.SessionID, recording.SessionFailed)
		}
	default:
		log.Info("Session ended")
		r.store.SetSessionState(bgCtx, worker.strKey, worker.SessionID, recording.SessionFinished)
	}

	r.deregister(bgCtx, worker.strKey)

	// Purge the streamer's KV last — after every session-state/stats write above
	// — so a removed streamer leaves no lingering state behind.
	if strStatus == streamer.StatusRemoved {
		r.store.Delete(bgCtx, worker.strKey)
		if err := vlstate.DeleteStreamer(bgCtx, r.kv, worker.strKey); err != nil {
			log.Error("Failed to purge removed streamer KV", "error", err)
		}
	}
}

func (r *Registry) register(ctx context.Context, w *worker, key streamer.StreamerKey) error {
	if !key.Valid() {
		return fmt.Errorf("invalid streamer key: %v", key)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.workers[key] = w
	workerCount := len(r.workers)
	vlstate.PutCount(ctx, r.kv, "worker_count", workerCount)
	return nil
}

func (r *Registry) deregister(ctx context.Context, key streamer.StreamerKey) {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.workers[key]
	if ok {
		delete(r.workers, key)
	}
	workerCount := len(r.workers)
	if !ok {
		r.logger.Debug("Worker not found", "key", key)
	}
	vlstate.PutCount(ctx, r.kv, "worker_count", workerCount)
}

// Wait blocks until all registered workers have completed. Nothing outside a
// worker sends for processing once it is called.
func (r *Registry) Wait() {
	r.lifeMu.Lock()
	r.closed = true
	r.lifeMu.Unlock()
	r.wg.Wait()
	r.logger.Info("All workers have completed")
}

func (r *Registry) Close() {
	r.logger.Close()
}

// SetConfig live-updates the session configuration used when creating new
// workers (download folder, default variant, output template). In-progress
// recordings keep the config they were started with.
func (r *Registry) SetConfig(cfg vlconfig.SessionConfig) {
	r.cfgMu.Lock()
	defer r.cfgMu.Unlock()
	r.cfg = cfg
}

// sessionCfg returns a snapshot of the current session configuration.
func (r *Registry) sessionCfg() vlconfig.SessionConfig {
	r.cfgMu.RLock()
	defer r.cfgMu.RUnlock()
	return r.cfg
}

// ActiveSessionDirs returns the output folders of Sessions currently
// recording. The VOD Archive scanner leaves them out until the Session ends,
// even if they already hold vodN.mp4 from mid-stream splits.
func (r *Registry) ActiveSessionDirs() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	dirs := make([]string, 0, len(r.workers))
	for _, w := range r.workers {
		if dir := w.OutDir(); dir != "" {
			dirs = append(dirs, dir)
		}
	}
	// An interrupted session's folder is a recording in progress too.
	if r.resume != nil {
		dirs = append(dirs, r.resume.heldDirs()...)
	}
	return dirs
}

// ApplyRecordState cancels every active worker whose streamer is currently
// switched off in the streamers file. Idempotent and safe to call repeatedly.
func (r *Registry) ApplyRecordState() {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for key, w := range r.workers {
		id := streamer.StreamerRef{StreamerKey: key}
		if !r.streamersFile.OptionsFor(id).Record {
			r.logger.Info("Cancelling recording (switched off)", "streamer", key)
			w.stopWith(stopSwitchedOff)
		}
	}
}

// newSessionWorker builds a worker for a session about to start or resume.
func (r *Registry) newSessionWorker(cfg vlconfig.SessionConfig) *worker {
	w := newWorker(
		cfg,
		r.kv,
		r.store,
		r.lagReportCh,
		r.streamReqCh,
		r.pendingRecordingsCh,
		r.basicHTTPClient,
		r.flvHTTPClient,
		r.cookies)
	w.resume = r.resume
	w.resumeWindow = r.resumeWindow()
	return w
}

// resumeWindow is the configured resume window; zero disables resuming.
func (r *Registry) resumeWindow() time.Duration {
	d, err := vlconfig.ParseResumeWindow(r.sessionCfg().ResumeWindow)
	if err != nil {
		return 0
	}
	return d
}

// openResume opens the checkpoint folder and takes stock of the sessions it
// holds: interrupted ones wait to resume, and one a crashed daemon left
// recording is recovered first.
func (r *Registry) openResume(dir string) {
	store, cps, err := openResumeStore(dir)
	if store == nil {
		r.logger.Error("Recordings will not resume after an interruption", "error", err)
		return
	}
	if err != nil {
		r.logger.Warn("Some session checkpoints could not be read", "error", err)
	}
	r.resume = store
	for _, cp := range cps {
		if cp.InterruptedAt.IsZero() {
			cp = r.recoverCrashed(cp)
		}
		if err := store.hold(cp); err != nil {
			r.logger.Warn("Failed to keep an interrupted session", "session_id", cp.SessionID, "error", err)
			continue
		}
		r.logger.Info("Interrupted session waiting to resume",
			"streamer", cp.key(),
			"session_id", cp.SessionID,
			"reason", cp.Reason,
			"resume_by", cp.resumeBy(r.resumeWindow()).Format(time.RFC3339))
	}
}

// recoverCrashed turns the checkpoint of a session a daemon was recording when
// it stopped without warning into an interrupted one, keeping the part it was
// writing.
func (r *Registry) recoverCrashed(cp *checkpoint) *checkpoint {
	w := r.dormantWorker(cp)
	w.foldCrashedPart(cp.Current)
	out := w.checkpoint(nil)
	out.InterruptedAt = out.lastByte()
	out.Reason = reasonCrashed
	w.logger.Info("Recovered a session the recorder stopped recording without warning",
		"last_byte_at", out.LastByteAt.Format(time.RFC3339),
		"total_bytes", out.TotalBytes)
	w.closeLog()
	return out
}

// dormantWorker builds a worker holding an interrupted session that is not
// running, to finish it or recover it.
func (r *Registry) dormantWorker(cp *checkpoint) *worker {
	w := r.newSessionWorker(r.sessionCfg())
	w.restore(cp)
	if err := w.openLog(); err != nil {
		w.logger = r.logger.With("session_id", cp.SessionID)
	}
	return w
}

// takeResumable hands out str's interrupted session if the new session should
// carry it on, and finishes it otherwise.
func (r *Registry) takeResumable(str streamer.StreamerState) *checkpoint {
	if r.resume == nil {
		return nil
	}
	cp := r.resume.take(str.StreamerKey)
	if cp == nil {
		return nil
	}
	ok, why := canResume(cp, str, writerFor(str.Streams.Select(cp.Variant)), r.resumeWindow(), time.Now())
	if !ok {
		r.logger.Info("Not resuming the interrupted session", "streamer", cp.key(), "session_id", cp.SessionID, "why", why)
		r.finishInterrupted(cp, why)
		return nil
	}
	return cp
}

// settleInterrupted finishes str's interrupted session when the update shows
// its broadcast is over, and otherwise keeps it on the dashboard.
func (r *Registry) settleInterrupted(ctx context.Context, str streamer.StreamerState) {
	if r.resume == nil {
		return
	}
	cp := r.resume.peek(str.StreamerKey)
	if cp == nil {
		return
	}
	if broadcastOver(cp, str) {
		if cp = r.resume.take(str.StreamerKey); cp != nil {
			r.finishInterrupted(cp, "the broadcast is over ("+string(str.Status)+")")
		}
		return
	}
	r.store.SetInterrupted(ctx, str.StreamerKey, cp.SessionID, cp.resumeBy(r.resumeWindow()))
}

// resumeSweepInterval is how often interrupted sessions are checked against
// the resume window.
const resumeSweepInterval = 10 * time.Second

// runResumeSweep finishes interrupted sessions that run out of resume window,
// starting with any that already have.
func (r *Registry) runResumeSweep(ctx context.Context) {
	if r.resume == nil {
		return
	}
	t := time.NewTicker(resumeSweepInterval)
	defer t.Stop()
	for {
		for _, cp := range r.resume.takeExpired(time.Now(), r.resumeWindow()) {
			r.finishInterrupted(cp, "it did not resume within the resume window")
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// FinishInterrupted finishes key's interrupted session now rather than holding
// it for a resume, and reports whether there was one.
func (r *Registry) FinishInterrupted(key streamer.StreamerKey) bool {
	if r.resume == nil {
		return false
	}
	cp := r.resume.take(key)
	if cp == nil {
		return false
	}
	r.finishInterrupted(cp, "finished on request")
	return true
}

// finishInterrupted sends an interrupted session for processing as it stands.
// During shutdown it is left on disk instead, for the next start to resume or
// finish.
func (r *Registry) finishInterrupted(cp *checkpoint, why string) {
	r.lifeMu.Lock()
	if r.closed {
		r.lifeMu.Unlock()
		return
	}
	r.wg.Add(1)
	r.lifeMu.Unlock()
	defer r.wg.Done()

	bgCtx := context.Background()
	key := cp.key()
	defer r.store.ClearInterrupted(bgCtx, key, cp.SessionID)
	if fi, err := os.Stat(cp.BaseOutDir); err != nil || !fi.IsDir() {
		r.logger.Warn("Dropping an interrupted session whose folder is gone",
			"streamer", key, "session_id", cp.SessionID, "out_dir", cp.BaseOutDir)
		r.resume.remove(cp.SessionID)
		return
	}
	w := r.dormantWorker(cp)
	w.endedAt = cp.lastByte()
	w.logger.Info("Finishing interrupted session", "why", why)
	w.finishSession()
	r.store.SetSessionState(bgCtx, key, cp.SessionID, recording.SessionFinished)
	r.logger.Info("Finished interrupted session",
		"streamer", key, "session_id", cp.SessionID, "why", why, "out_dir", cp.BaseOutDir)
}
