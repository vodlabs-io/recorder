package processing

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/vodlabs-io/recorder/internal/streamer"
	"github.com/vodlabs-io/recorder/internal/vlconfig"
	"github.com/vodlabs-io/recorder/internal/vllog"
	"github.com/vodlabs-io/recorder/internal/vlstate"
	"github.com/vodlabs-io/recorder/internal/vodmove"
)

// discardLogger is a no-op slog logger for exercising job helpers in tests.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// newTestService builds a Service backed by a throwaway on-disk SQLite queue and
// a silent logger, for exercising the queue/plan helpers in isolation.
func newTestService(t *testing.T) *Service {
	t.Helper()
	logger, err := vllog.NewFileLogger(vlconfig.LoggerConfig{Mode: "off", Level: "error"}, "test")
	if err != nil {
		t.Fatalf("logger: %v", err)
	}
	t.Cleanup(func() { logger.Close() })

	db, err := initQueue(context.Background(), logger, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("initQueue: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	return &Service{
		db:            db,
		logger:        logger,
		moveAvailable: make(chan struct{}, 1),
	}
}

func TestMoveBackoffProgression(t *testing.T) {
	want := []time.Duration{
		time.Minute,      // attempt 1
		5 * time.Minute,  // attempt 2
		15 * time.Minute, // attempt 3
		30 * time.Minute, // attempt 4
		30 * time.Minute, // attempt 5 (capped)
		30 * time.Minute, // attempt 50 (still capped)
	}
	attempts := []int{1, 2, 3, 4, 5, 50}
	for i, a := range attempts {
		if got := moveBackoff(a); got != want[i] {
			t.Errorf("moveBackoff(%d) = %v, want %v", a, got, want[i])
		}
	}
}

// newGroupJob builds an FLV group job rooted at dir with a single converted part
// present, so processGroupJob takes the rename fast path (no ffmpeg needed).
func newGroupJob(t *testing.T, dir, output string) *job {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "pt1_best.mp4"), []byte("video-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	j := &job{id: 1, groupInputs: []string{"pt1_best"}, groupOutput: output,
		archiveDir: "/mnt/nas", moveRel: "tiktok/alice/sess"}
	j.vod.WriterName = "flvwriter"
	j.vod.BaseOutDir = dir
	j.vod.Platform = "tiktok"
	j.vod.Username = "alice"
	j.vod.SessionID = "sess"
	return j
}

func TestProcessGroupJobRenamesAndEnqueuesMove(t *testing.T) {
	s := newTestService(t)
	s.store = vlstate.NewStore(nil) // SetMoveState no-ops for an untracked streamer
	ctx := context.Background()
	dir := t.TempDir()
	j := newGroupJob(t, dir, "vod.mp4")

	if err := j.processGroupJob(ctx, discardLogger(), s); err != nil {
		t.Fatalf("processGroupJob: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "vod.mp4")); err != nil {
		t.Fatalf("output not produced: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "pt1_best.mp4")); !os.IsNotExist(err) {
		t.Fatalf("input should be consumed by rename, stat err = %v", err)
	}

	mj, _, err := s.claimNextDueMove(ctx)
	if err != nil || mj == nil {
		t.Fatalf("expected a move job, err=%v mj=%v", err, mj)
	}
	if mj.kind != "file" || mj.fileName != "vod.mp4" || mj.dest != "/mnt/nas" || mj.relDir != "tiktok/alice/sess" {
		t.Fatalf("unexpected move job: %+v", mj)
	}
}

func TestProcessGroupJobIdempotentRetry(t *testing.T) {
	s := newTestService(t)
	s.store = vlstate.NewStore(nil)
	ctx := context.Background()
	dir := t.TempDir()
	j := newGroupJob(t, dir, "vod1.mp4")

	if err := j.processGroupJob(ctx, discardLogger(), s); err != nil {
		t.Fatalf("first run: %v", err)
	}
	// Re-run after the input is already consumed and the output exists (a crash
	// before the row was marked complete): no error, no duplicate move row.
	if err := j.processGroupJob(ctx, discardLogger(), s); err != nil {
		t.Fatalf("retry run: %v", err)
	}

	var n int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM move_jobs WHERE session_id='sess' AND kind='file'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("expected exactly one move row after retry, got %d", n)
	}
}

func TestProcessGroupJobDeletesRawParts(t *testing.T) {
	s := newTestService(t)
	s.store = vlstate.NewStore(nil)
	s.resolveDelete = func(context.Context, streamer.StreamerKey) bool { return true }
	ctx := context.Background()
	dir := t.TempDir()
	j := newGroupJob(t, dir, "vod.mp4")
	// The raw .ts sits beside the converted .mp4; with deletion enabled the group
	// job must free it once the output is produced.
	if err := os.WriteFile(filepath.Join(dir, "pt1_best.ts"), []byte("raw"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := j.processGroupJob(ctx, discardLogger(), s); err != nil {
		t.Fatalf("processGroupJob: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "pt1_best.ts")); !os.IsNotExist(err) {
		t.Fatalf("raw .ts should be deleted when delete_part_files is on, stat err = %v", err)
	}
}

func TestProcessGroupJobKeepsRawWhenDisabled(t *testing.T) {
	s := newTestService(t)
	s.store = vlstate.NewStore(nil)
	// resolveDelete left nil ⇒ raw parts retained for the end-of-session sweep.
	ctx := context.Background()
	dir := t.TempDir()
	j := newGroupJob(t, dir, "vod.mp4")
	if err := os.WriteFile(filepath.Join(dir, "pt1_best.ts"), []byte("raw"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := j.processGroupJob(ctx, discardLogger(), s); err != nil {
		t.Fatalf("processGroupJob: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "pt1_best.ts")); err != nil {
		t.Fatalf("raw .ts should be retained when deletion is off: %v", err)
	}
}

func TestClaimDeferssSweepUntilFilesDone(t *testing.T) {
	s := newTestService(t)
	ctx := context.Background()
	now := time.Now().Unix()

	insert := func(kind, fileName string) {
		_, err := s.db.ExecContext(ctx,
			`INSERT INTO move_jobs (kind, platform, username, session_id, base_out_dir,
			   rel_dir, file_name, dest, status, next_retry_at, created_at)
			 VALUES (?, 'tiktok', 'alice', 'sess', '/local', 'rel', ?, '/dest', 'pending', 0, ?)`,
			kind, fileName, now)
		if err != nil {
			t.Fatalf("insert %s: %v", kind, err)
		}
	}
	insert("file", "vod1.mp4")
	insert("sweep", "")

	// With a pending file move for the session, the sweep must be held back.
	mj, _, err := s.claimNextDueMove(ctx)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if mj == nil || mj.kind != "file" {
		t.Fatalf("expected to claim the file move first, got %+v", mj)
	}
	if err := s.completeMove(ctx, mj.id); err != nil {
		t.Fatalf("completeMove: %v", err)
	}

	// Now only the sweep remains and becomes claimable.
	mj, _, err = s.claimNextDueMove(ctx)
	if err != nil {
		t.Fatalf("claim 2: %v", err)
	}
	if mj == nil || mj.kind != "sweep" {
		t.Fatalf("expected to claim the sweep now, got %+v", mj)
	}
}

func TestFailMoveSchedulesBackoff(t *testing.T) {
	s := newTestService(t)
	ctx := context.Background()
	now := time.Now().Unix()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO move_jobs (kind, base_out_dir, rel_dir, dest, status, next_retry_at, created_at)
		 VALUES ('file', '/local', 'rel', '/dest', 'pending', 0, ?)`, now)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	mj, _, err := s.claimNextDueMove(ctx)
	if err != nil || mj == nil {
		t.Fatalf("claim: %v (mj=%v)", err, mj)
	}
	future := time.Now().Add(time.Minute)
	if err := s.failMove(ctx, mj.id, 1, future, "boom", vodmove.ReasonOther); err != nil {
		t.Fatalf("failMove: %v", err)
	}

	// Not yet due, so a claim returns nothing but reports the next-due time.
	got, nextDue, err := s.claimNextDueMove(ctx)
	if err != nil {
		t.Fatalf("claim after fail: %v", err)
	}
	if got != nil {
		t.Fatalf("expected no due move, got %+v", got)
	}
	if nextDue.IsZero() {
		t.Fatal("expected a next-due time for the backed-off move")
	}
}

// TestNextDueSkipsBlockedSweep covers a sweep enqueued with next_retry_at = 0
// and blocked behind a pending file job: reporting that 0 while the claim query
// refuses the sweep would make runMoveWorker spin. The reported time must be
// the blocking file's backoff instead.
func TestNextDueSkipsBlockedSweep(t *testing.T) {
	s := newTestService(t)
	ctx := context.Background()
	now := time.Now().Unix()
	fileRetry := time.Now().Add(5 * time.Minute)

	// A file job backed off into the future (destination offline), plus its
	// session's sweep sitting at next_retry_at = 0.
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO move_jobs (kind, session_id, base_out_dir, rel_dir, dest, status, next_retry_at, created_at)
		 VALUES ('file', 'sess1', '/local', 'rel', '/dest', 'pending', ?, ?)`,
		fileRetry.Unix(), now); err != nil {
		t.Fatalf("insert file: %v", err)
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO move_jobs (kind, session_id, base_out_dir, rel_dir, dest, status, next_retry_at, created_at)
		 VALUES ('sweep', 'sess1', '/local', 'rel', '/dest', 'pending', 0, ?)`, now); err != nil {
		t.Fatalf("insert sweep: %v", err)
	}

	mj, nextDue, err := s.claimNextDueMove(ctx)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if mj != nil {
		t.Fatalf("expected nothing claimable, got %+v", mj)
	}
	if nextDue.IsZero() {
		t.Fatal("expected the blocking file's retry time, got zero")
	}
	if !nextDue.After(time.Now()) {
		t.Fatalf("next-due %v is in the past: the worker would spin", nextDue)
	}
	if nextDue.Unix() != fileRetry.Unix() {
		t.Fatalf("next-due = %v, want the file's retry at %v", nextDue, fileRetry)
	}
}

func TestStartupRecoveryResets(t *testing.T) {
	logger, err := vllog.NewFileLogger(vlconfig.LoggerConfig{Mode: "off", Level: "error"}, "test")
	if err != nil {
		t.Fatalf("logger: %v", err)
	}
	defer logger.Close()
	dbPath := filepath.Join(t.TempDir(), "recover.db")

	// First boot: create a stuck 'processing' job and a backed-off move.
	db, err := initQueue(context.Background(), logger, dbPath)
	if err != nil {
		t.Fatalf("initQueue: %v", err)
	}
	ctx := context.Background()
	if _, err := db.ExecContext(ctx,
		`INSERT INTO postprocess_jobs (base_out_dir, writer_name, job_stage, status, created_at)
		 VALUES ('/x', 'flvwriter', 'concat', 'processing', 0)`); err != nil {
		t.Fatalf("insert job: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO move_jobs (kind, base_out_dir, rel_dir, dest, status, next_retry_at, created_at)
		 VALUES ('file', '/x', 'rel', '/d', 'pending', 9999999999, 0)`); err != nil {
		t.Fatalf("insert move: %v", err)
	}
	db.Close()

	// Second boot: startup recovery resets the stuck job to pending and makes the
	// pending move due immediately.
	db2, err := initQueue(ctx, logger, dbPath)
	if err != nil {
		t.Fatalf("re-initQueue: %v", err)
	}
	defer db2.Close()

	var jobStatus string
	if err := db2.QueryRowContext(ctx, `SELECT status FROM postprocess_jobs LIMIT 1`).Scan(&jobStatus); err != nil {
		t.Fatalf("query job: %v", err)
	}
	if jobStatus != "pending" {
		t.Errorf("job status = %q, want pending", jobStatus)
	}
	var nextRetry int64
	if err := db2.QueryRowContext(ctx, `SELECT next_retry_at FROM move_jobs LIMIT 1`).Scan(&nextRetry); err != nil {
		t.Fatalf("query move: %v", err)
	}
	if nextRetry != 0 {
		t.Errorf("move next_retry_at = %d, want 0", nextRetry)
	}
}

func TestSweepRemovesEmptyParents(t *testing.T) {
	s := newTestService(t)
	ctx := context.Background()
	archive := t.TempDir()
	dest := t.TempDir()
	rel := filepath.Join("tiktok", "alice", "sess")
	base := filepath.Join(archive, rel)
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatal(err)
	}
	store, err := vodmove.ForDest(dest)
	if err != nil {
		t.Fatal(err)
	}
	mj := &moveJob{kind: "sweep", baseOutDir: base, relDir: rel, dest: dest, sessionID: "sess"}

	if err := s.sweepSession(ctx, store, mj); err != nil {
		t.Fatalf("sweepSession: %v", err)
	}
	// Session dir and its now-empty parents are gone; the download root remains.
	for _, p := range []string{base, filepath.Join(archive, "tiktok", "alice"), filepath.Join(archive, "tiktok")} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("expected %q removed, stat err = %v", p, err)
		}
	}
	if _, err := os.Stat(archive); err != nil {
		t.Fatalf("archive root must remain: %v", err)
	}
}

func TestSweepKeepsNonEmptyParents(t *testing.T) {
	s := newTestService(t)
	ctx := context.Background()
	archive := t.TempDir()
	dest := t.TempDir()
	rel := filepath.Join("tiktok", "alice", "sess")
	base := filepath.Join(archive, rel)
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatal(err)
	}
	// Another streamer under the same platform keeps the platform dir alive.
	sibling := filepath.Join(archive, "tiktok", "bob", "sess2")
	if err := os.MkdirAll(sibling, 0o755); err != nil {
		t.Fatal(err)
	}
	store, err := vodmove.ForDest(dest)
	if err != nil {
		t.Fatal(err)
	}
	mj := &moveJob{kind: "sweep", baseOutDir: base, relDir: rel, dest: dest, sessionID: "sess"}

	if err := s.sweepSession(ctx, store, mj); err != nil {
		t.Fatalf("sweepSession: %v", err)
	}
	if _, err := os.Stat(filepath.Join(archive, "tiktok", "alice")); !os.IsNotExist(err) {
		t.Errorf("empty {username} dir should be removed, stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(archive, "tiktok")); err != nil {
		t.Errorf("non-empty {platform} dir must remain: %v", err)
	}
	if _, err := os.Stat(sibling); err != nil {
		t.Errorf("sibling session must remain: %v", err)
	}
}
