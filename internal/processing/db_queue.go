package processing

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/vodlabs-io/recorder/internal/recording"
	"github.com/vodlabs-io/recorder/internal/vlconfig"
	"github.com/vodlabs-io/recorder/internal/vllog"
	_ "modernc.org/sqlite"
)

// initQueue initializes the SQLite database and creates the schema. dbPath sets
// the database location; when empty it defaults to processing.db under the OS
// user config dir (e.g. ~/.config/vodlabs/recorder/processing.db on Linux).
func initQueue(ctx context.Context, log *vllog.FileLogger, dbPath string) (*sql.DB, error) {
	if dbPath == "" {
		dbPath = vlconfig.DefaultFilePath("processing.db")
	}

	// Ensure the parent directory exists.
	if err := os.MkdirAll(filepath.Dir(dbPath), 0755); err != nil {
		return nil, fmt.Errorf("failed to create database directory: %w", err)
	}

	database, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}
	log.Debug("Database opened", "path", dbPath)

	// Limit to 1 connection to avoid "database is locked" errors in SQLite
	database.SetMaxOpenConns(1)
	database.SetMaxIdleConns(1)

	queries := []string{
		// Enable WAL mode for better concurrency
		"PRAGMA journal_mode=WAL;",
		// Sync less frequently for better performance with WAL
		"PRAGMA synchronous=NORMAL;",
		// Set a busy timeout to handle brief lock contention gracefully
		"PRAGMA busy_timeout=5000;",
		// Use in-memory temp store for faster temporary tables/indexes
		"PRAGMA temp_store=memory;",
	}
	for _, query := range queries {
		if _, err = database.ExecContext(ctx, query); err != nil {
			database.Close()
			return nil, fmt.Errorf("failed to set database settings: %w", err)
		}
	}

	schema := `
	CREATE TABLE IF NOT EXISTS postprocess_jobs (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		base_out_dir TEXT NOT NULL,
		writer_name TEXT NOT NULL,
		job_stage TEXT DEFAULT 'concat',
		input_path TEXT,
		platform TEXT,
		username TEXT,
		session_id TEXT,
		session_bytes INTEGER,
		session_duration INTEGER,
		session_speed INTEGER,
		session_state TEXT,
		status TEXT DEFAULT 'pending',
		created_at INTEGER NOT NULL,
		started_at INTEGER,
		completed_at INTEGER,
		error TEXT,
		-- Snapshotted archive folder for the session ("" = resolved-off,
		-- NULL = not yet resolved); move_rel is the relative dir under it.
		archive_dir TEXT,
		move_rel TEXT,
		-- group_inputs (JSON array of part stems) and group_output (vodN.mp4) carry
		-- a worker-defined concat group on a 'group' stage row.
		group_inputs TEXT,
		group_output TEXT,
		-- parts (JSON array of vod.Part) is carried on 'finalize' rows only; the
		-- manifest writer matches them against the session's group rows for
		-- per-file times.
		parts TEXT
	);

	CREATE INDEX IF NOT EXISTS idx_status_created ON postprocess_jobs(status, created_at);

	-- move_jobs is the queue for moving finished files into the archive folder.
	-- 'file' rows move one produced vodN.mp4 as it is concatenated; a single
	-- 'sweep' row per session moves the remaining files and removes the
	-- downloaded folder once all file moves for the session are done.
	--
	-- status is 'pending' | 'done' | 'failed' | 'cancelled'. Moves retry forever
	-- with backoff, because a destination can come back and a full disk can be
	-- emptied. Two states end that: 'failed', for a source file that no longer
	-- exists, which no later attempt can recover; and 'cancelled', for a move
	-- whose streamer has since stopped archiving anywhere, leaving nothing to
	-- retry into.
	CREATE TABLE IF NOT EXISTS move_jobs (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		kind TEXT NOT NULL,
		platform TEXT,
		username TEXT,
		session_id TEXT,
		base_out_dir TEXT NOT NULL,
		rel_dir TEXT NOT NULL,
		file_name TEXT,
		dest TEXT NOT NULL,
		writer_name TEXT,
		status TEXT NOT NULL DEFAULT 'pending',
		attempts INTEGER NOT NULL DEFAULT 0,
		next_retry_at INTEGER NOT NULL DEFAULT 0,
		last_error TEXT,
		-- last_reason classifies last_error (no_space | unreachable | permission
		-- | other) so clients branch on a stable value rather than OS error text.
		last_reason TEXT,
		created_at INTEGER NOT NULL,
		completed_at INTEGER
	);

	CREATE INDEX IF NOT EXISTS idx_move_due ON move_jobs(status, next_retry_at);
	`

	if _, err := database.ExecContext(ctx, schema); err != nil {
		database.Close()
		return nil, fmt.Errorf("failed to create schema: %w", err)
	}

	// Startup recovery. A daemon killed mid-job leaves a 'processing' row that
	// getNextPendingJob would never pick up again; reset it so the idempotent
	// job resumes. Make every pending move due immediately so a network drive
	// that has come back is retried at once (the move pre-flight makes this
	// cheap under a full destination).
	for _, stmt := range []string{
		`UPDATE postprocess_jobs SET status='pending' WHERE status='processing'`,
		`UPDATE move_jobs SET next_retry_at=0 WHERE status='pending'`,
	} {
		if _, err := database.ExecContext(ctx, stmt); err != nil {
			database.Close()
			return nil, fmt.Errorf("failed to run startup recovery: %w", err)
		}
	}

	return database, nil
}

// enqueueJob adds a new post-processing job to the queue. The job's Kind
// selects the stage.
func (s *Service) enqueueJob(ctx context.Context, v recording.Job) error {
	if s.db == nil {
		return fmt.Errorf("database not initialized")
	}

	stage := jobStageConvert
	var groupInputs, groupOutput, parts sql.NullString
	switch v.Kind {
	case recording.JobFinalize:
		stage = jobStageFinalize
		// The finalize row carries the session's part bookkeeping for the
		// manifest writer; it isn't otherwise persisted anywhere.
		if len(v.Parts) > 0 {
			if data, err := json.Marshal(v.Parts); err == nil {
				parts = sql.NullString{String: string(data), Valid: true}
			}
		}
	case recording.JobGroup:
		stage = jobStageGroup
		data, err := json.Marshal(v.GroupInputs)
		if err != nil {
			return fmt.Errorf("failed to encode group inputs: %w", err)
		}
		groupInputs = sql.NullString{String: string(data), Valid: true}
		groupOutput = sql.NullString{String: v.GroupOutput, Valid: true}
	}

	query := `
	INSERT INTO postprocess_jobs (
		base_out_dir,
		writer_name,
		job_stage,
		input_path,
		platform,
		username,
		session_id,
		session_bytes,
		session_duration,
		session_speed,
		session_state,
		group_inputs,
		group_output,
		parts,
		status,
		created_at
	)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`

	_, err := s.db.ExecContext(
		ctx,
		query,
		v.BaseOutDir,
		v.WriterName,
		string(stage),
		v.CurrentPart.Path,
		v.Platform,
		v.Username,
		v.SessionID,
		v.Stats.TotalBytes,
		v.Stats.Duration.Milliseconds(),
		v.Stats.Speed,
		v.SessionState,
		groupInputs,
		groupOutput,
		parts,
		"pending",
		time.Now().Unix(),
	)
	if err != nil {
		return fmt.Errorf("failed to execute query: %w", err)
	}

	return nil
}

// getNextPendingJob retrieves the oldest pending job from the queue.
func (s *Service) getNextPendingJob(ctx context.Context) (*job, error) {
	if s.db == nil {
		return nil, fmt.Errorf("database not initialized")
	}

	query := `
	SELECT
		id,
		base_out_dir,
		writer_name,
		job_stage,
		input_path,
		platform,
		username,
		session_id,
		session_bytes,
		session_duration, 
		session_speed,
		session_state,
		status,
		created_at,
		started_at,
		completed_at,
		error,
		archive_dir,
		move_rel,
		group_inputs,
		group_output,
		parts
	FROM postprocess_jobs
	WHERE status = 'pending'
	ORDER BY created_at ASC, id ASC
	LIMIT 1
	`

	var job job
	var createdAt int64
	var startedAt, completedAt sql.NullInt64
	var errorMsg, archiveDir, moveRel, groupInputs, groupOutput, parts sql.NullString

	err := s.db.QueryRowContext(ctx, query).Scan(
		&job.id,
		&job.vod.BaseOutDir,
		&job.vod.WriterName,
		&job.stage,
		&job.inputPath,
		&job.vod.Platform,
		&job.vod.Username,
		&job.vod.SessionID,
		&job.vod.Stats.TotalBytes,
		&job.vod.Stats.Duration,
		&job.vod.Stats.Speed,
		&job.vod.SessionState,
		&job.status,
		&createdAt,
		&startedAt,
		&completedAt,
		&errorMsg,
		&archiveDir,
		&moveRel,
		&groupInputs,
		&groupOutput,
		&parts,
	)

	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to query pending job: %w", err)
	}

	job.createdAt = time.Unix(createdAt, 0)

	if groupOutput.Valid {
		job.groupOutput = groupOutput.String
	}
	if groupInputs.Valid && groupInputs.String != "" {
		if err := json.Unmarshal([]byte(groupInputs.String), &job.groupInputs); err != nil {
			return nil, fmt.Errorf("failed to decode group inputs: %w", err)
		}
	}
	// Parts ride only on finalize rows; a decode failure just degrades the
	// manifest's per-file times, so it is not fatal.
	if parts.Valid && parts.String != "" {
		_ = json.Unmarshal([]byte(parts.String), &job.vod.Parts)
	}

	// A non-NULL archive_dir means the destination was already resolved and
	// snapshotted on a prior attempt; reuse it instead of re-resolving (the
	// one-shot KV overrides it was derived from may have been purged since).
	if archiveDir.Valid {
		job.moveResolved = true
		job.archiveDir = archiveDir.String
		job.moveRel = moveRel.String
	}

	if startedAt.Valid {
		t := time.Unix(startedAt.Int64, 0)
		job.startedAt = &t
	}
	if completedAt.Valid {
		t := time.Unix(completedAt.Int64, 0)
		job.completedAt = &t
	}
	if errorMsg.Valid {
		job.error = errorMsg.String
	}

	return &job, nil
}

// persistArchiveDir snapshots the resolved archive folder and relative directory
// onto the job row so retries reuse it (the one-shot KV overrides it derives
// from are purged once the Recording finishes processing).
func (s *Service) persistArchiveDir(ctx context.Context, jobID int64, dest, relDir string) error {
	if s.db == nil {
		return fmt.Errorf("database not initialized")
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE postprocess_jobs SET archive_dir = ?, move_rel = ? WHERE id = ?`,
		dest, relDir, jobID)
	if err != nil {
		return fmt.Errorf("failed to persist archive folder: %w", err)
	}
	return nil
}

// updateStatus updates the status of a job.
func (j *job) updateStatus(ctx context.Context, db *sql.DB, status string, errorMsg string) error {
	if db == nil {
		return fmt.Errorf("database not initialized")
	}

	var query string
	var args []any

	switch status {
	case "processing":
		query = `UPDATE postprocess_jobs SET status = ?, started_at = ? WHERE id = ?`
		args = []any{status, time.Now().Unix(), j.id}
	case "completed":
		query = `UPDATE postprocess_jobs SET status = ?, completed_at = ? WHERE id = ?`
		args = []any{status, time.Now().Unix(), j.id}
	case "failed":
		query = `UPDATE postprocess_jobs SET status = ?, completed_at = ?, error = ? WHERE id = ?`
		args = []any{status, time.Now().Unix(), errorMsg, j.id}
	default:
		query = `UPDATE postprocess_jobs SET status = ? WHERE id = ?`
		args = []any{status, j.id}
	}

	_, err := db.ExecContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("failed to execute query: %w", err)
	}

	return nil
}
