package processing

import (
	"context"
	"fmt"
	"path/filepath"
)

// runWorker starts the post-processing worker which processes jobs from the queue
func (s *Service) runWorker(processCtx context.Context) {
	s.logger.Info("Started worker")
	defer s.logger.Info("Stopped worker")

	for {
		// ok stays true for a space-recovery wake: only jobAvailable closing
		// means there will be no more work.
		ok := true
		select {
		case <-processCtx.Done():
			return
		case _, ok = <-s.jobAvailable:
		case <-s.spaceAvailable:
		}
		if processCtx.Err() != nil {
			return
		}
		if err := s.processAllJobs(processCtx); err != nil {
			s.logger.Error("processAllJobs failed", "error", err)
			return
		}
		if !ok {
			return
		}
	}
}

func (s *Service) processAllJobs(ctx context.Context) error {
	for {
		job, err := s.getNextPendingJob(ctx)
		if err != nil {
			return fmt.Errorf("failed to get next pending job: %w", err)
		}
		// No pending jobs
		if job == nil {
			break
		}
		jobLogger := s.logger.With("job_id", job.id, "session_id", job.vod.SessionID)
		jobLogger.Info("Found pending job")

		// Defer rather than run a job the disk cannot hold the output of. The
		// row stays pending and resumes on the next signal — including the one
		// the daemon sends when free space recovers.
		if !s.hasScratchSpace(ctx, filepath.Dir(job.vod.BaseOutDir)) {
			jobLogger.Warn("Deferring processing: not enough free space")
			return nil
		}

		// Resolve and snapshot the archive folder on a group/finalize job's first
		// attempt. It is persisted so retries reuse it: the one-shot KV overrides
		// it derives from are purged once the Recording finishes processing.
		if job.stage != jobStageConvert && !job.moveResolved && s.resolveMove != nil {
			dest, relDir := s.resolveMove(ctx, job.strKey(), job.vod.BaseOutDir)
			if err := s.persistArchiveDir(ctx, job.id, dest, relDir); err != nil {
				jobLogger.Error("Failed to persist archive folder", "error", err)
			}
			job.archiveDir = dest
			job.moveRel = relDir
			job.moveResolved = true
		}

		if err := job.process(ctx, jobLogger, s); err != nil {
			return fmt.Errorf("failed to process job: %w", err)
		}
	}
	return nil
}

func (s *Service) queuePendingJobs(ctx context.Context) {
	defer close(s.jobAvailable)

	// Tell worker to process any jobs that were pending at startup
	s.jobAvailable <- struct{}{}

	for vod := range s.PendingRecordings {
		if err := s.enqueueJob(ctx, vod); err != nil {
			s.logger.Error("Failed to enqueue job", "error", err)
		}
		select {
		case s.jobAvailable <- struct{}{}:
		default:
		}
	}
}
