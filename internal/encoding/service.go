// Package encoding owns the daemon's re-encode job queue: user-triggered
// merges of a VOD's numbered VOD files run serially through a persistent
// SQLite-backed queue, invoking the embedded ffmpeg/ffprobe. The queue is
// deliberately daemon-local: none of its API subjects or KV keys are bridged
// to the Dashboard.
package encoding

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/vodlabs-io/recorder/internal/vlconfig"
	"github.com/vodlabs-io/recorder/internal/vllog"
	"github.com/vodlabs-io/recorder/internal/vlstate"
)

// Service runs the encode queue: one worker goroutine executing jobs
// serially, backed by encoding.db (queue truth — the KV keys it writes are
// ephemeral notifications only).
type Service struct {
	// WorkerDone is closed when the worker goroutine has exited.
	WorkerDone chan struct{}

	logger        *vllog.FileLogger
	db            *sql.DB
	store         *vlstate.Store
	processCancel context.CancelFunc
	// jobAvailable wakes the worker after enqueue/retry/resume (buffered 1).
	jobAvailable chan struct{}
	// notifyVODsChanged mirrors processing.Service's notifier: called with the
	// session folders whose VOD Archive listing changed (merge produced/cleaned).
	notifyVODsChanged func(dirs ...string)

	// cacheThumbnail mirrors processing.Service's: called with a screenshot
	// that is still readable cheaply, so it can be copied.
	cacheThumbnail func(sessionKey, file, localPath string)
	// forgetThumbnail drops a cached screenshot for a file a merge retired.
	forgetThumbnail func(sessionKey, file string)

	mu      sync.Mutex
	running *runningJob // non-nil while a job is executing

	detectMu sync.Mutex
	detected []EncoderStatus // cached DetectEncoders result
}

// runningJob tracks the in-flight job so Cancel can kill its ffmpeg/copy and
// the worker can tell a user cancel from a daemon shutdown.
type runningJob struct {
	id            int64
	cancel        context.CancelFunc
	userCancelled atomic.Bool
}

// Init opens (or creates) the queue database and starts the worker. Like
// processing, the worker runs on a context detached from mainCtx so the
// daemon decides when in-flight work is aborted.
func Init(mainCtx context.Context, cfg vlconfig.ProcessingConfig, store *vlstate.Store) (*Service, error) {
	logger, err := vllog.NewFileLogger(cfg.Logger, "encoding")
	if err != nil {
		return nil, fmt.Errorf("failed to initialize logger: %w", err)
	}

	db, err := initDB(mainCtx, logger, cfg.DBPath)
	if err != nil {
		logger.Close()
		return nil, fmt.Errorf("failed to initialize encode queue: %w", err)
	}

	processCtx, processCancel := context.WithCancel(context.Background())

	s := &Service{
		WorkerDone:    make(chan struct{}),
		logger:        logger,
		db:            db,
		store:         store,
		processCancel: processCancel,
		jobAvailable:  make(chan struct{}, 1),
	}

	go func() {
		s.runWorker(processCtx)
		processCancel()
		close(s.WorkerDone)
	}()

	// Publish the recovered queue's count before any mutation, so a restart
	// with jobs still pending shows the badge right away.
	s.publishQueueChange(mainCtx)

	// Kick the worker once so jobs recovered at startup resume immediately.
	s.signal()

	return s, nil
}

// cancelEncoding aborts the in-flight job (if any) and stops the worker from
// picking up new ones. Safe to call multiple times.
func (s *Service) cancelEncoding() {
	s.processCancel()
}

// SetVODsChangedNotifier installs fn to be called whenever a merge changes
// what the VOD Archive lists. Called once during daemon startup.
func (s *Service) SetVODsChangedNotifier(fn func(dirs ...string)) {
	s.notifyVODsChanged = fn
}

// SetThumbnailCachers installs the hooks a merge uses to keep the thumbnail
// cache in step: cache receives the merged output's screenshot, forget drops
// the parts' screenshots the merge just retired. Called once during daemon
// startup, like SetVODsChangedNotifier.
func (s *Service) SetThumbnailCachers(
	cache func(sessionKey, file, localPath string),
	forget func(sessionKey, file string),
) {
	s.cacheThumbnail = cache
	s.forgetThumbnail = forget
}

func (s *Service) vodsChanged(dirs ...string) {
	if s.notifyVODsChanged != nil {
		s.notifyVODsChanged(dirs...)
	}
}

// signal wakes the worker without blocking (the channel is buffered).
func (s *Service) signal() {
	select {
	case s.jobAvailable <- struct{}{}:
	default:
	}
}

func (s *Service) Close() {
	s.processCancel()
	<-s.WorkerDone
	if s.db != nil {
		s.db.Close()
	}
	s.logger.Close()
}
