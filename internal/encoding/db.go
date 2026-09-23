package encoding

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	"github.com/vodlabs-io/recorder/internal/vlconfig"
	"github.com/vodlabs-io/recorder/internal/vllog"
	_ "modernc.org/sqlite"
)

// terminalStatuses is the SQL set of statuses a job can no longer leave
// without an explicit retry.
const terminalStatuses = `('done', 'failed', 'cancelled')`

// maxTerminalJobs caps how many finished/failed/cancelled rows are retained
// for the queue screen's history; older ones are pruned.
const maxTerminalJobs = 50

// initDB opens the encode queue database. It lives next to processing.db
// (same directory, own file — no cross-package connection sharing) and uses
// the same SQLite settings.
func initDB(ctx context.Context, log *vllog.FileLogger, processingDBPath string) (*sql.DB, error) {
	var dbPath string
	if processingDBPath != "" {
		dbPath = filepath.Join(filepath.Dir(processingDBPath), "encoding.db")
	} else {
		dbPath = vlconfig.DefaultFilePath("encoding.db")
	}

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

	for _, query := range []string{
		"PRAGMA journal_mode=WAL;",
		"PRAGMA synchronous=NORMAL;",
		"PRAGMA busy_timeout=5000;",
		"PRAGMA temp_store=memory;",
	} {
		if _, err = database.ExecContext(ctx, query); err != nil {
			database.Close()
			return nil, fmt.Errorf("failed to set database settings: %w", err)
		}
	}

	schema := `
	CREATE TABLE IF NOT EXISTS encode_jobs (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		-- job_type/source future-proof the queue for non-merge re-encode tasks
		-- and automatic enqueueing; v1 only ever writes 'merge'/'manual'.
		job_type TEXT NOT NULL DEFAULT 'merge',
		source TEXT NOT NULL DEFAULT 'manual',
		session_dir TEXT NOT NULL,
		platform TEXT,
		username TEXT,
		session_id TEXT,
		-- inputs is the JSON array of part file names snapshotted at enqueue.
		inputs TEXT NOT NULL,
		output TEXT NOT NULL DEFAULT 'vod.mp4',
		delete_parts INTEGER NOT NULL DEFAULT 1,
		-- encoder preference snapshotted at enqueue ('' = auto).
		encoder TEXT NOT NULL DEFAULT '',
		position INTEGER NOT NULL,
		status TEXT NOT NULL DEFAULT 'queued',
		stage TEXT NOT NULL DEFAULT '',
		error TEXT,
		warning TEXT,
		log_path TEXT,
		created_at INTEGER NOT NULL,
		started_at INTEGER,
		completed_at INTEGER
	);

	CREATE INDEX IF NOT EXISTS idx_encode_status_pos ON encode_jobs(status, position);

	CREATE TABLE IF NOT EXISTS encode_meta (
		key TEXT PRIMARY KEY,
		value TEXT NOT NULL
	);
	`
	if _, err := database.ExecContext(ctx, schema); err != nil {
		database.Close()
		return nil, fmt.Errorf("failed to create schema: %w", err)
	}

	// Startup recovery: a daemon killed mid-encode leaves a 'running' row the
	// worker would never pick up again. Reset it; the job restarts from
	// scratch (or resumes at the copy stage if its staged output survived and
	// verifies).
	for _, stmt := range []string{
		`UPDATE encode_jobs SET status='queued', stage='' WHERE status='running'`,
		`DELETE FROM encode_jobs WHERE status IN ` + terminalStatuses + `
		 AND id NOT IN (
			SELECT id FROM encode_jobs WHERE status IN ` + terminalStatuses + `
			ORDER BY completed_at DESC, id DESC LIMIT ` + fmt.Sprint(maxTerminalJobs) + `)`,
	} {
		if _, err := database.ExecContext(ctx, stmt); err != nil {
			database.Close()
			return nil, fmt.Errorf("failed to run startup recovery: %w", err)
		}
	}

	return database, nil
}

// pruneTerminal trims finished/failed/cancelled history beyond maxTerminalJobs.
func (s *Service) pruneTerminal(ctx context.Context) {
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM encode_jobs WHERE status IN `+terminalStatuses+`
		 AND id NOT IN (
			SELECT id FROM encode_jobs WHERE status IN `+terminalStatuses+`
			ORDER BY completed_at DESC, id DESC LIMIT `+fmt.Sprint(maxTerminalJobs)+`)`); err != nil {
		s.logger.Warn("Failed to prune finished encode jobs", "error", err)
	}
}
