package processing

import (
	"context"
	"database/sql"
	"fmt"
	"sync"

	"github.com/vodlabs-io/recorder/internal/diskspace"
	"github.com/vodlabs-io/recorder/internal/recording"
	"github.com/vodlabs-io/recorder/internal/streamer"
	"github.com/vodlabs-io/recorder/internal/vlconfig"
	"github.com/vodlabs-io/recorder/internal/vllog"
	"github.com/vodlabs-io/recorder/internal/vlstate"
)

type Service struct {
	PendingRecordings chan recording.Job
	WorkerDone        chan struct{}
	moverDone         chan struct{}
	jobAvailable      chan struct{}
	// spaceAvailable wakes the worker when free space recovers. Separate from
	// jobAvailable because that one is closed at shutdown, and a disk sample
	// racing the close would send on a closed channel.
	spaceAvailable       chan struct{}
	moveAvailable        chan struct{}
	logger               *vllog.FileLogger
	db                   *sql.DB
	store                *vlstate.Store
	processCancel        context.CancelFunc
	onRecordingProcessed func(context.Context, recording.Output)
	// resolveDelete reports whether a session's raw Parts should be deleted
	// after a group is produced. Called by the group job; nil keeps raw Parts
	// (until the end-of-session sweep).
	resolveDelete func(context.Context, streamer.StreamerKey) bool
	// resolveMove resolves a session's effective Archive folder and the relative
	// directory under it. Called once per group/finalize job and snapshotted
	// onto the job row. nil disables archiving entirely.
	resolveMove func(ctx context.Context, key streamer.StreamerKey, baseOutDir string) (dest, relDir string)
	// notifyVODsChanged is invoked whenever the set of finished VODs on disk
	// changes (group produced, session finalized, move completed), with the
	// affected session folders. Installed via SetVODsChangedNotifier; nil
	// disables notification.
	notifyVODsChanged func(dirs ...string)

	// cacheThumbnail is invoked with a screenshot that is still on local disk,
	// so it can be copied before the move.
	cacheThumbnail func(sessionKey, file, localPath string)
	// sampler reads free space at move destinations. Injected so a full disk is
	// an ordinary test input rather than something CI has to reproduce.
	sampler diskspace.Sampler

	// curMove identifies the move executing right now, with the cancel func for
	// its context, so cancelling that move aborts a copy already under way
	// instead of letting it finish and delete the local file. Only the single
	// move worker writes these, but a cancel arrives on an API goroutine.
	curMoveMu     sync.Mutex
	curMoveID     int64
	curMoveCancel context.CancelFunc
}

func Init(
	mainCtx context.Context,
	cfg vlconfig.ProcessingConfig,
	store *vlstate.Store,
	onRecordingProcessed func(ctx context.Context, v recording.Output),
	resolveDelete func(context.Context, streamer.StreamerKey) bool,
	resolveMove func(ctx context.Context, key streamer.StreamerKey, baseOutDir string) (dest, relDir string),
) (*Service, error) {
	logger, err := vllog.NewFileLogger(cfg.Logger, "processing")
	if err != nil {
		return nil, fmt.Errorf("failed to initialize logger: %w", err)
	}

	db, err := initQueue(mainCtx, logger, cfg.DBPath)
	if err != nil {
		logger.Close()
		return nil, fmt.Errorf("failed to initialize queue: %w", err)
	}

	// One slot per streamer the daemon can be recording at once: a part is
	// handed over as it completes, and every session can have one in flight.
	// The send is a blocking one on purpose — a job dropped here is a recording
	// that never gets converted — so the depth is what keeps a burst of parts
	// finishing together off the recording path.
	pendingRecordings := make(chan recording.Job, streamer.MaxSubscribed)
	workerDone := make(chan struct{})
	moverDone := make(chan struct{})
	jobAvailable := make(chan struct{})
	spaceAvailable := make(chan struct{}, 1)
	moveAvailable := make(chan struct{}, 1)

	// Detached from mainCtx: the daemon decides when to cancel based on
	// whether shutdown was triggered by an OS signal or by the CLI.
	processCtx, processCancel := context.WithCancel(context.Background())

	s := &Service{
		PendingRecordings:    pendingRecordings,
		WorkerDone:           workerDone,
		moverDone:            moverDone,
		jobAvailable:         jobAvailable,
		spaceAvailable:       spaceAvailable,
		moveAvailable:        moveAvailable,
		logger:               logger,
		db:                   db,
		store:                store,
		processCancel:        processCancel,
		onRecordingProcessed: onRecordingProcessed,
		resolveDelete:        resolveDelete,
		resolveMove:          resolveMove,
		sampler:              diskspace.System(),
	}

	go func() {
		s.runWorker(processCtx)
		processCancel()
		close(workerDone)
	}()

	// The move worker drains the move queue on its own goroutine so retrying
	// moves never block ffmpeg post-processing. It is cancelled together with
	// the ffmpeg worker when processCtx is cancelled and signals MoverDone.
	go s.runMoveWorker(processCtx)

	// Enqueue jobs for Recordings that need to be processed
	go s.queuePendingJobs(processCtx)

	return s, nil
}

// CancelProcessing aborts any in-flight job and prevents new jobs from being
// processed. Safe to call multiple times.
func (s *Service) CancelProcessing() {
	s.processCancel()
}

func (s *Service) Close() {
	// Stop the move worker and wait for it to finish so it is no longer
	// touching the database before we close it.
	s.processCancel()
	<-s.moverDone
	if s.db != nil {
		s.db.Close()
	}
	s.logger.Close()
}
