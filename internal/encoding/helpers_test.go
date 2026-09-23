package encoding

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/vodlabs-io/recorder/internal/vlconfig"
	"github.com/vodlabs-io/recorder/internal/vllog"
)

// discardLogger returns a logger that drops everything (mode "off").
func discardLogger(t *testing.T) *vllog.FileLogger {
	t.Helper()
	logger, err := vllog.NewFileLogger(vlconfig.LoggerConfig{Mode: "off", Level: "error"}, "encoding-test")
	if err != nil {
		t.Fatalf("new logger: %v", err)
	}
	return logger
}

// newTestServiceIn builds a Service backed by encoding.db under dir. The
// worker is NOT started; tests drive the queue methods directly.
func newTestServiceIn(t *testing.T, dir string) *Service {
	t.Helper()
	logger := discardLogger(t)
	db, err := initDB(context.Background(), logger, filepath.Join(dir, "processing.db"))
	if err != nil {
		logger.Close()
		t.Fatalf("initDB: %v", err)
	}
	s := &Service{db: db, logger: logger, jobAvailable: make(chan struct{}, 1)}
	t.Cleanup(func() {
		db.Close()
		logger.Close()
	})
	return s
}

func newTestService(t *testing.T) *Service {
	t.Helper()
	return newTestServiceIn(t, t.TempDir())
}

// makeSession creates a session folder with the named part files (nonzero so
// they look like real recordings) and returns its path.
func makeSession(t *testing.T, parts ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, name := range parts {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("fake mp4 bytes"), 0o644); err != nil {
			t.Fatalf("write part %s: %v", name, err)
		}
	}
	return dir
}

// insertJob inserts a raw row for tests that need a specific status/stage
// without going through the worker. Returns the new job id.
func insertJob(t *testing.T, s *Service, sessionDir, status, stage string) int64 {
	t.Helper()
	res, err := s.db.Exec(`
		INSERT INTO encode_jobs (job_type, source, session_dir, inputs, output, position, status, stage, created_at)
		VALUES ('merge', 'manual', ?, '["vod1.mp4","vod2.mp4"]', 'vod.mp4', 1, ?, ?, 0)`,
		sessionDir, status, stage)
	if err != nil {
		t.Fatalf("insert job: %v", err)
	}
	id, _ := res.LastInsertId()
	return id
}

func countJobs(t *testing.T, db *sql.DB, where string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM encode_jobs `+where, args...).Scan(&n); err != nil {
		t.Fatalf("count jobs: %v", err)
	}
	return n
}
