package processing

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/vodlabs-io/recorder/internal/diskspace"
	"github.com/vodlabs-io/recorder/internal/streamer"
	"github.com/vodlabs-io/recorder/internal/vlstate"
)

// insertPendingMove adds one pending 'file' move for tiktok/alice and returns
// its row id.
func insertPendingMove(t *testing.T, s *Service, baseOutDir, dest string) int64 {
	t.Helper()
	return insertPendingMoveFor(t, s, "tiktok", "alice", "sess", baseOutDir, dest)
}

// insertPendingMoveFor adds one pending 'file' move for an explicit streamer and
// session, and returns its row id.
func insertPendingMoveFor(t *testing.T, s *Service, platform, username, sessionID, baseOutDir, dest string) int64 {
	t.Helper()
	res, err := s.db.Exec(
		`INSERT INTO move_jobs
		  (kind, platform, username, session_id, base_out_dir, rel_dir,
		   file_name, dest, writer_name, status, created_at)
		 VALUES ('file', ?, ?, ?, ?, ?, 'vod.mp4', ?, 'flvwriter', 'pending', ?)`,
		platform, username, sessionID, baseOutDir,
		filepath.Join(platform, username, sessionID), dest, time.Now().Unix())
	if err != nil {
		t.Fatalf("insert move: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("last insert id: %v", err)
	}
	return id
}

// moveRow reads back the status and attempt count of one move row.
func moveRow(t *testing.T, s *Service, id int64) (status string, attempts int) {
	t.Helper()
	if err := s.db.QueryRow(
		`SELECT status, attempts FROM move_jobs WHERE id=?`, id).
		Scan(&status, &attempts); err != nil {
		t.Fatalf("read move row: %v", err)
	}
	return status, attempts
}

// resolveTo installs a resolver reporting dest for every streamer; "" means
// archiving is switched off.
func resolveTo(s *Service, dest string) {
	s.resolveMove = func(context.Context, streamer.StreamerKey, string) (string, string) {
		return dest, "tiktok/alice/sess"
	}
}

// roomySampler reports plenty of free space, so the move pre-flight is never the
// reason a test move is refused.
func roomySampler(s *Service) {
	s.sampler = fixedSampler{usage: diskspace.Usage{TotalBytes: 100 * gib, FreeBytes: 90 * gib}}
}

// Unticking "Move recordings" must retire a move that was retrying, rather than
// leave it aimed at a folder no longer configured anywhere.
func TestExecuteMoveCancelsWhenArchivingDisabled(t *testing.T) {
	s := newTestService(t)
	s.store = vlstate.NewStore(nil)
	ctx := context.Background()
	id := insertPendingMove(t, s, t.TempDir(), "/mnt/nas")
	resolveTo(s, "")

	mj, _, err := s.claimNextDueMove(ctx)
	if err != nil || mj == nil {
		t.Fatalf("expected a claimable move, err=%v mj=%v", err, mj)
	}
	s.executeMove(ctx, mj)

	if status, _ := moveRow(t, s, id); status != "cancelled" {
		t.Fatalf("status = %q, want cancelled", status)
	}
	// A cancelled row must leave the queue: the worker selects on 'pending'.
	next, _, err := s.claimNextDueMove(ctx)
	if err != nil {
		t.Fatalf("claim after cancel: %v", err)
	}
	if next != nil {
		t.Fatalf("cancelled move still claimable: %+v", next)
	}
}

// Changing the archive folder is not the same as turning it off: a session's
// files must keep landing together at the destination snapshotted for them, so
// the move is retried, not cancelled.
func TestExecuteMoveKeepsSnapshotWhenDestChanged(t *testing.T) {
	s := newTestService(t)
	s.store = vlstate.NewStore(nil)
	ctx := context.Background()
	dir := t.TempDir()
	// The source must exist, or the move fails terminally as source_missing and
	// says nothing about the case under test. The destination is a path that
	// cannot be created, so the attempt fails but stays retryable.
	if err := os.WriteFile(filepath.Join(dir, "vod.mp4"), []byte("video-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	unwritable := filepath.Join(t.TempDir(), "file-not-a-dir")
	if err := os.WriteFile(unwritable, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	id := insertPendingMove(t, s, dir, filepath.Join(unwritable, "nas"))
	resolveTo(s, "/mnt/somewhere-else")
	roomySampler(s)

	mj, _, err := s.claimNextDueMove(ctx)
	if err != nil || mj == nil {
		t.Fatalf("expected a claimable move, err=%v mj=%v", err, mj)
	}
	s.executeMove(ctx, mj)

	status, attempts := moveRow(t, s, id)
	if status != "pending" {
		t.Fatalf("status = %q, want pending (a changed folder must not cancel)", status)
	}
	if attempts != 1 {
		t.Fatalf("attempts = %d, want 1 (the move should have been attempted)", attempts)
	}
}

// A queued move belonging to a streamer that still archives is left alone when
// another streamer's archiving is turned off.
func TestExecuteMoveRunsWhenStillConfigured(t *testing.T) {
	s := newTestService(t)
	s.store = vlstate.NewStore(nil)
	ctx := context.Background()
	dir := t.TempDir()
	dest := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "vod.mp4"), []byte("video-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	id := insertPendingMove(t, s, dir, dest)
	resolveTo(s, dest)
	roomySampler(s)

	mj, _, err := s.claimNextDueMove(ctx)
	if err != nil || mj == nil {
		t.Fatalf("expected a claimable move, err=%v mj=%v", err, mj)
	}
	s.executeMove(ctx, mj)

	if status, _ := moveRow(t, s, id); status != "done" {
		t.Fatalf("status = %q, want done", status)
	}
	if _, err := os.Stat(filepath.Join(dest, "tiktok", "alice", "sess", "vod.mp4")); err != nil {
		t.Fatalf("file not archived: %v", err)
	}
}

// ArchiveConfigChanged brings pending moves forward so a disabled destination is
// noticed now rather than at the end of a half-hour backoff.
func TestArchiveConfigChangedMakesPendingMovesDue(t *testing.T) {
	s := newTestService(t)
	ctx := context.Background()
	id := insertPendingMove(t, s, t.TempDir(), "/mnt/nas")
	if _, err := s.db.Exec(
		`UPDATE move_jobs SET next_retry_at=? WHERE id=?`,
		time.Now().Add(30*time.Minute).Unix(), id); err != nil {
		t.Fatalf("push out retry: %v", err)
	}

	if mj, _, err := s.claimNextDueMove(ctx); err != nil || mj != nil {
		t.Fatalf("move should not be due yet, err=%v mj=%v", err, mj)
	}
	s.ArchiveConfigChanged(ctx)
	mj, _, err := s.claimNextDueMove(ctx)
	if err != nil || mj == nil {
		t.Fatalf("move should be due after a config change, err=%v mj=%v", err, mj)
	}
}

// A manual cancel scoped to one streamer must leave every other streamer's
// queued moves alone.
func TestCancelMovesForOneStreamer(t *testing.T) {
	s := newTestService(t)
	s.store = vlstate.NewStore(nil)
	ctx := context.Background()
	alice := insertPendingMoveFor(t, s, "tiktok", "alice", "s1", t.TempDir(), "/mnt/nas")
	bob := insertPendingMoveFor(t, s, "kick", "bob", "s2", t.TempDir(), "/mnt/nas")

	key := streamer.StreamerKey{Platform: "tiktok", Username: "alice"}
	n, err := s.CancelMoves(ctx, &key)
	if err != nil {
		t.Fatalf("CancelMoves: %v", err)
	}
	if n != 1 {
		t.Fatalf("cancelled = %d, want 1", n)
	}
	if status, _ := moveRow(t, s, alice); status != "cancelled" {
		t.Fatalf("alice status = %q, want cancelled", status)
	}
	if status, _ := moveRow(t, s, bob); status != "pending" {
		t.Fatalf("bob status = %q, want pending (out of scope)", status)
	}
}

// With no key, every pending move goes.
func TestCancelMovesAll(t *testing.T) {
	s := newTestService(t)
	s.store = vlstate.NewStore(nil)
	ctx := context.Background()
	alice := insertPendingMoveFor(t, s, "tiktok", "alice", "s1", t.TempDir(), "/mnt/nas")
	bob := insertPendingMoveFor(t, s, "kick", "bob", "s2", t.TempDir(), "/mnt/nas")

	n, err := s.CancelMoves(ctx, nil)
	if err != nil {
		t.Fatalf("CancelMoves: %v", err)
	}
	if n != 2 {
		t.Fatalf("cancelled = %d, want 2", n)
	}
	for _, id := range []int64{alice, bob} {
		if status, _ := moveRow(t, s, id); status != "cancelled" {
			t.Fatalf("row %d status = %q, want cancelled", id, status)
		}
	}
	if mj, _, err := s.claimNextDueMove(ctx); err != nil || mj != nil {
		t.Fatalf("queue should be empty, err=%v mj=%v", err, mj)
	}
}

// Cancelling an empty queue is not an error and reports nothing cancelled.
func TestCancelMovesNoPending(t *testing.T) {
	s := newTestService(t)
	n, err := s.CancelMoves(context.Background(), nil)
	if err != nil {
		t.Fatalf("CancelMoves: %v", err)
	}
	if n != 0 {
		t.Fatalf("cancelled = %d, want 0", n)
	}
}

// The attempt that was already running must not flip a cancelled row back to
// 'done' (or onto a fresh retry schedule) when it finishes.
func TestCancelledMoveIsNotResurrected(t *testing.T) {
	s := newTestService(t)
	s.store = vlstate.NewStore(nil)
	ctx := context.Background()
	id := insertPendingMove(t, s, t.TempDir(), "/mnt/nas")
	if _, err := s.CancelMoves(ctx, nil); err != nil {
		t.Fatalf("CancelMoves: %v", err)
	}

	if err := s.completeMove(ctx, id); err != nil {
		t.Fatalf("completeMove: %v", err)
	}
	if status, _ := moveRow(t, s, id); status != "cancelled" {
		t.Fatalf("status after completeMove = %q, want cancelled", status)
	}

	if err := s.failMove(ctx, id, 3, time.Now().Add(time.Hour), "boom", "unreachable"); err != nil {
		t.Fatalf("failMove: %v", err)
	}
	status, attempts := moveRow(t, s, id)
	if status != "cancelled" || attempts != 0 {
		t.Fatalf("status/attempts after failMove = %q/%d, want cancelled/0", status, attempts)
	}
}

// A cancel sticks for the session: post-processing that is still producing files
// must not queue fresh moves behind the user's back.
func TestEnqueueFileMoveRefusedAfterCancel(t *testing.T) {
	s := newTestService(t)
	s.store = vlstate.NewStore(nil)
	ctx := context.Background()
	dir := t.TempDir()
	insertPendingMove(t, s, dir, "/mnt/nas")
	if _, err := s.CancelMoves(ctx, nil); err != nil {
		t.Fatalf("CancelMoves: %v", err)
	}

	j := newGroupJob(t, dir, "vod2.mp4")
	if err := s.enqueueFileMove(ctx, j, "vod2.mp4"); err != nil {
		t.Fatalf("enqueueFileMove: %v", err)
	}
	var n int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM move_jobs WHERE status='pending'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("pending moves = %d, want 0 (cancel must stick)", n)
	}
}
