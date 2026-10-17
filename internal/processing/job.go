package processing

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/vodlabs-io/recorder/internal/ffmpeg"
	"github.com/vodlabs-io/recorder/internal/provenance"
	"github.com/vodlabs-io/recorder/internal/recording"
	"github.com/vodlabs-io/recorder/internal/streamer"
)

// job represents a post-processing job in the queue.
type job struct {
	id          int64
	vod         recording.Output
	stage       jobStage
	inputPath   string
	status      string
	createdAt   time.Time
	startedAt   *time.Time
	completedAt *time.Time
	error       string
	// groupInputs holds the part stems for a group job and groupOutput its frozen
	// output file name (e.g. "vod1.mp4"). Both come from the session worker,
	// which is authoritative for split grouping and vodN numbering.
	groupInputs []string
	groupOutput string
	// archiveDir is the snapshotted Archive folder for this session's files (""
	// when archiving is disabled). moveRel is the snapshotted relative directory
	// under it. moveResolved reports whether the Archive folder has already been
	// resolved and persisted to the job row (NULL archive_dir = not yet resolved).
	archiveDir   string
	moveRel      string
	moveResolved bool
}

type jobStage string

const (
	// jobStageConvert turns one flvwriter .ts part into .mp4 (FLV only).
	jobStageConvert jobStage = "convert"
	// jobStageGroup concatenates a worker-defined group of parts into a single
	// vodN.mp4 and queues it for move.
	jobStageGroup jobStage = "group"
	// jobStageFinalize runs once at session end: state, raw-part cleanup, and the
	// logs sweep. It performs no concatenation.
	jobStageFinalize jobStage = "finalize"
)

// provenanceRecord describes this build and session for one output file, for
// muxing into that file's container metadata.
func (j *job) provenanceRecord(partName string) string {
	return provenance.Record(j.vod.WriterName, j.vod.SessionID, partName)
}

func (j *job) strKey() streamer.StreamerKey {
	return streamer.StreamerKey{
		Platform: j.vod.Platform,
		Username: j.vod.Username,
	}
}

// process runs a single post-processing job: an FLV part conversion, a group
// concat, or the session's finalize.
func (j *job) process(ctx context.Context, logger *slog.Logger, s *Service) error {
	db := s.db
	store := s.store
	switch j.stage {
	case jobStageConvert, jobStageGroup: // Intermediate stages that run during recording
		// Clear the proc_state when done so the CLI/dashboard stop showing
		// "processing" between groups; the session is still recording, and the
		// final state is set by the finalize stage's onRecordingProcessed.
		defer store.ClearProcessingState(ctx, j.strKey(), j.vod.SessionID)
	case jobStageFinalize: // Final stage
		defer func() { s.onRecordingProcessed(ctx, j.vod) }()
		if j.vod.Stats.TotalBytes < recording.MinPartBytes {
			j.vod.ProcessState = recording.ProcessRejected
			if err := j.updateStatus(ctx, db, "rejected", "Total bytes too small"); err != nil {
				return err
			}
			return nil
		}
	}
	if err := j.updateStatus(ctx, db, "processing", ""); err != nil {
		return err
	}
	j.vod.ProcessState = recording.ProcessProcessing
	store.SetProcessingState(ctx, j.strKey(), j.vod.SessionID, recording.ProcessProcessing)

	logger.Info("Processing job",
		"base_out_dir", j.vod.BaseOutDir,
		"writer_name", j.vod.WriterName,
		"job_stage", j.stage,
		"input_path", j.inputPath,
		"platform", j.vod.Platform,
		"username", j.vod.Username,
		"session_id", j.vod.SessionID)

	// Determine processing strategy based on the job stage. Convert is FLV-only;
	// group and finalize are writer-agnostic (the group job derives its input
	// paths from the writer name internally).
	var err error
	switch j.stage {
	case jobStageConvert:
		err = j.processFLVConvert(ctx, logger)
	case jobStageGroup:
		err = j.processGroupJob(ctx, logger, s)
	case jobStageFinalize:
		// Bookkeeping only — the deferred onRecordingProcessed does the work. Write
		// the session manifest here, after every group has been produced and
		// before the move sweep (enqueued by onRecordingProcessed) moves the
		// folder. Best-effort: without it the VOD Archive falls back to the
		// filesystem parse.
		if merr := j.writeManifest(ctx, logger, s); merr != nil {
			logger.Warn("Failed to write session manifest", "error", merr)
		}
		// A session with a failed group finishes failed, which keeps every raw
		// part on disk for a retry.
		err = s.failedGroupsErr(ctx, j.vod.SessionID)
	default:
		err = fmt.Errorf("unsupported job stage: %s", j.stage)
	}

	if err != nil {
		if ctx.Err() != nil {
			bgCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			logger.Info("Job cancelled due to shutdown")
			// Reset job back to pending so it can be retried on next startup
			if updateErr := j.updateStatus(bgCtx, db, "pending", ""); updateErr != nil {
				logger.Error("Failed to reset job to pending", "error", updateErr)
			}
			ctxErr := ctx.Err()
			// onRecordingProcessed callback gets the background context
			ctx = bgCtx
			j.vod.ProcessState = recording.ProcessCancelled
			return ctxErr
		}
		j.vod.ProcessState = recording.ProcessFailed

		logger.Error("Job processing failed", "error", err)
		if updateErr := j.updateStatus(ctx, db, "failed", err.Error()); updateErr != nil {
			return updateErr
		}
		return nil // Don't return error, continue processing other jobs
	}
	j.vod.ProcessState = recording.ProcessFinished

	logger.Info("Job completed")
	if err := j.updateStatus(ctx, db, "completed", ""); err != nil {
		return err
	}
	// A produced group or a finalized session changes what the VOD Archive
	// lists; convert jobs only touch intermediates.
	if j.stage != jobStageConvert {
		s.vodsChanged(j.vod.BaseOutDir)
	}
	return nil
}

// runFFmpeg executes ffmpeg with the given arguments.
// It handles context cancellation and cleans up partial output files.
func (j *job) runFFmpeg(ctx context.Context, args []string, outputPath string, logger *slog.Logger) error {
	cmd, err := ffmpeg.Command(ctx, logger, args...)
	if err != nil {
		return fmt.Errorf("failed to create ffmpeg command: %w", err)
	}

	logFile := filepath.Join(j.vod.BaseOutDir, "ffmpeg.log")
	f, err := os.OpenFile(logFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("failed to open log file: %w", err)
	}
	defer f.Close()

	// Redirect ffmpeg output to log file, filtering noisy lines
	fw := &filterWriter{w: f}
	cmd.Stdout = fw
	cmd.Stderr = fw

	logger.Info("Starting ffmpeg", "ffmpeg_args", strings.Join(args, " "), "ffmpeg_log", logFile)

	err = cmd.Run()
	fw.flush()

	if ctx.Err() != nil {
		logger.Info("Context cancelled, cleaning up partial output", "output_path", outputPath)
		if removeErr := os.Remove(outputPath); removeErr != nil && !os.IsNotExist(removeErr) {
			logger.Warn("Failed to remove partial output", "error", removeErr)
		}
		return fmt.Errorf("ffmpeg cancelled: %w", ctx.Err())
	}

	if err != nil {
		if removeErr := os.Remove(outputPath); removeErr != nil && !os.IsNotExist(removeErr) {
			logger.Warn("Failed to remove partial output after error", "error", removeErr)
		}
		return fmt.Errorf("ffmpeg failed: %w", err)
	}

	logger.Info("FFmpeg completed successfully", "output", outputPath)
	return nil
}

func (j *job) writeLogFile(content string) error {
	logFile := filepath.Join(j.vod.BaseOutDir, "ffmpeg.log")
	f, err := os.OpenFile(logFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("failed to open log file: %w", err)
	}
	defer f.Close()

	if _, err := f.WriteString(content); err != nil {
		return fmt.Errorf("failed to write to log file: %w", err)
	}
	return nil
}
