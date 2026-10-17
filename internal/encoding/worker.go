package encoding

import (
	"context"
	"path/filepath"
	"time"
)

// runWorker drains the queue serially: one job at a time, in queue order,
// regardless of active recordings (recordings are stream-copy and cheap;
// pausing the queue is the user's escape hatch).
func (s *Service) runWorker(ctx context.Context) {
	s.logger.Info("Started encode worker")
	defer s.logger.Info("Stopped encode worker")

	for {
		select {
		case <-ctx.Done():
			return
		case <-s.jobAvailable:
		}
		for ctx.Err() == nil {
			if s.paused(ctx) {
				break
			}
			job, err := s.claimNext(ctx)
			if err != nil {
				s.logger.Error("Failed to claim next encode job", "error", err)
				break
			}
			if job == nil {
				break
			}
			s.runJob(ctx, job)
		}
	}
}

// runJob executes one claimed job and records its terminal state,
// distinguishing user cancellation (→ cancelled, staging cleaned) from
// daemon shutdown (→ requeued, staging kept for the copy-stage resume).
func (s *Service) runJob(workerCtx context.Context, job *Job) {
	jobCtx, cancel := context.WithCancel(workerCtx)
	defer cancel()

	r := &runningJob{id: job.ID, cancel: cancel}
	s.mu.Lock()
	s.running = r
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.running = nil
		s.mu.Unlock()
	}()

	logger := s.logger.With("job_id", job.ID, "dir", job.Dir)
	logger.Info("Starting encode job", "type", job.Type,
		"parts", len(job.Inputs), "delete_parts", job.DeleteParts, "encoder_pref", job.Encoder)

	warning, err := s.runMergeJob(jobCtx, logger, job)

	// Bookkeeping must survive a cancelled job context.
	bgCtx, bgCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer bgCancel()
	s.store.DelEncodeProgress(bgCtx)

	logPath := filepath.Join(job.Dir, encodeLogName)
	outMerging := filepath.Join(job.Dir, job.Output) + mergingSuffix

	switch {
	case err == nil:
		logger.Info("Merge completed", "output", job.Output, "warning", warning)
		s.finishJob(bgCtx, job.ID, statusDone, "", warning, logPath)
		s.vodsChanged(job.Dir)
	case jobCtx.Err() != nil && r.userCancelled.Load():
		logger.Info("Merge cancelled by user")
		s.cleanupStaging(job.ID)
		removePartial(outMerging)
		s.finishJob(bgCtx, job.ID, statusCancelled, "", "", logPath)
		s.vodsChanged(job.Dir)
	case jobCtx.Err() != nil:
		logger.Info("Merge interrupted by shutdown; job requeued")
		removePartial(outMerging) // staging is kept so the encode can resume
		s.requeueJob(bgCtx, job.ID)
	default:
		logger.Error("Merge failed", "error", err)
		s.cleanupStaging(job.ID)
		removePartial(outMerging)
		s.finishJob(bgCtx, job.ID, statusFailed, err.Error(), warning, logPath)
		s.vodsChanged(job.Dir)
	}
}
