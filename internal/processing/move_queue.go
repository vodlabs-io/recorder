package processing

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"time"

	"github.com/vodlabs-io/recorder/internal/recording"
	"github.com/vodlabs-io/recorder/internal/streamer"
	"github.com/vodlabs-io/recorder/internal/vodmove"
)

// moveJob is one row of the move_jobs queue.
type moveJob struct {
	id          int64
	kind        string // "file" | "sweep"
	platform    string
	username    string
	sessionID   string
	baseOutDir  string
	relDir      string
	fileName    string // "file" kind only
	dest        string
	writerName  string
	attempts    int
	nextRetryAt int64
}

func (m *moveJob) strKey() streamer.StreamerKey {
	return streamer.StreamerKey{Platform: m.platform, Username: m.username}
}

// EnqueueMoveSweep queues the final sweep for a finished session: it moves
// whatever remains in the local session folder (logs, and any raw parts kept
// when delete_part_files is off) and removes the now-empty folder. It reuses the
// destination snapshot recorded on the session's group/finalize jobs, so it
// always targets the same place as the per-file moves even if config changed
// since. A no-op when the session had no archive folder.
func (s *Service) EnqueueMoveSweep(ctx context.Context, v recording.Output) {
	if s.db == nil {
		return
	}
	var dest, relDir sql.NullString
	err := s.db.QueryRowContext(ctx,
		`SELECT archive_dir, move_rel FROM postprocess_jobs
		   WHERE session_id = ? AND archive_dir IS NOT NULL
		   ORDER BY id DESC LIMIT 1`, v.SessionID).Scan(&dest, &relDir)
	if err == sql.ErrNoRows {
		return
	}
	if err != nil {
		s.logger.Error("Failed to look up archive folder for sweep",
			"session_id", v.SessionID, "error", err)
		return
	}
	if !dest.Valid || dest.String == "" {
		return // moving disabled for this session
	}
	if s.sessionMovesCancelled(ctx, v.SessionID) {
		return // the user cancelled this session's moves
	}

	// Idempotent: finalize may re-run after a crash, but a session needs one sweep.
	var existing int
	if err := s.db.QueryRowContext(ctx,
		`SELECT 1 FROM move_jobs WHERE session_id = ? AND kind = 'sweep' LIMIT 1`,
		v.SessionID).Scan(&existing); err == nil {
		return // sweep already queued
	} else if err != sql.ErrNoRows {
		s.logger.Error("Failed to check existing sweep", "session_id", v.SessionID, "error", err)
		return
	}

	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO move_jobs
		  (kind, platform, username, session_id, base_out_dir, rel_dir,
		   dest, writer_name, status, created_at)
		 VALUES ('sweep', ?, ?, ?, ?, ?, ?, ?, 'pending', ?)`,
		v.Platform, v.Username, v.SessionID, v.BaseOutDir, relDir.String,
		dest.String, v.WriterName, time.Now().Unix()); err != nil {
		s.logger.Error("Failed to enqueue sweep", "session_id", v.SessionID, "error", err)
		return
	}
	s.store.SetMoveState(ctx, streamer.StreamerKey{Platform: v.Platform, Username: v.Username},
		v.SessionID, true, false)
	s.signalMove()
}

// enqueueFileMove queues a produced vodN.mp4 for archiving, reflects the move on
// the streamer card, and wakes the move worker. It is a no-op when moving is off,
// and idempotent across group-job retries: an identical pending/done row for the
// same session+file is left untouched so a re-run never queues a duplicate move.
func (s *Service) enqueueFileMove(ctx context.Context, j *job, fileName string) error {
	if j.archiveDir == "" {
		return nil
	}
	if s.sessionMovesCancelled(ctx, j.vod.SessionID) {
		return nil // the user cancelled this session's moves
	}

	var existing int
	err := s.db.QueryRowContext(ctx,
		`SELECT 1 FROM move_jobs WHERE session_id = ? AND kind = 'file' AND file_name = ? LIMIT 1`,
		j.vod.SessionID, fileName).Scan(&existing)
	if err == nil {
		return nil // already queued
	}
	if err != sql.ErrNoRows {
		return fmt.Errorf("check existing move: %w", err)
	}

	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO move_jobs
		  (kind, platform, username, session_id, base_out_dir, rel_dir,
		   file_name, dest, writer_name, status, created_at)
		 VALUES ('file', ?, ?, ?, ?, ?, ?, ?, ?, 'pending', ?)`,
		j.vod.Platform, j.vod.Username, j.vod.SessionID, j.vod.BaseOutDir,
		j.moveRel, fileName, j.archiveDir, j.vod.WriterName, time.Now().Unix()); err != nil {
		return fmt.Errorf("enqueue file move: %w", err)
	}

	s.store.SetMoveState(ctx, j.strKey(), j.vod.SessionID, true, false)
	s.signalMove()
	return nil
}

// archivingDisabled reports whether archiving is switched off for this
// streamer as the configuration stands now, globally or by the streamer's own
// "off" override. A move otherwise keeps the destination snapshotted when it
// was enqueued, so a session's files land in one place even if the configured
// folder changes.
//
// A one-shot's archive override is purged from KV once processing finishes,
// so its queued moves are judged by the global setting alone. With no resolver
// wired, it reports false.
func (s *Service) archivingDisabled(ctx context.Context, key streamer.StreamerKey, baseOutDir string) bool {
	if s.resolveMove == nil {
		return false
	}
	dest, _ := s.resolveMove(ctx, key, baseOutDir)
	return dest == ""
}

// ArchiveConfigChanged re-examines the queue after the archive configuration is
// edited. It makes every pending move due immediately and wakes the worker,
// which re-checks each one against the new configuration as it is claimed, so
// unticking "Move recordings" clears the retrying badge at once.
func (s *Service) ArchiveConfigChanged(ctx context.Context) {
	if s.db == nil {
		return
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE move_jobs SET next_retry_at=0 WHERE status='pending'`); err != nil {
		s.logger.Error("Failed to re-examine moves after an archive config change", "error", err)
		return
	}
	s.signalMove()
}

// CancelMoves retires every pending move matching scope and returns how many
// rows it retired. A nil key cancels the whole queue; otherwise only that
// streamer's moves are cancelled.
//
// Cancelling is one-way. Files already at the destination stay there, so a
// part-moved session is left split across the Download folder and the Archive
// folder, and the enqueue guards refuse to re-queue a cancelled session.
func (s *Service) CancelMoves(ctx context.Context, key *streamer.StreamerKey) (int, error) {
	if s.db == nil {
		return 0, fmt.Errorf("database not initialized")
	}

	// The affected sessions are read before the update, while the rows are still
	// pending, so the badge and the archive index can be brought up to date.
	sessions, err := s.pendingMoveSessions(ctx, key)
	if err != nil {
		return 0, err
	}

	query := `UPDATE move_jobs SET status='cancelled', completed_at=? WHERE status='pending'`
	args := []any{time.Now().Unix()}
	if key != nil {
		query += ` AND platform=? AND username=?`
		args = append(args, key.Platform, key.Username)
	}
	res, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, fmt.Errorf("cancel moves: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("cancel moves: %w", err)
	}
	if n == 0 {
		return 0, nil
	}

	// A copy already under way would otherwise run to completion and delete the
	// local file despite the cancel.
	s.abortCurrentMoveIfCancelled(ctx)

	for _, sess := range sessions {
		s.store.SetMoveState(ctx, sess.key, sess.sessionID, false, false)
		// Both sides changed: the download folder keeps what was never moved, and
		// the destination folder stops being suppressed as a move in flight, so it
		// becomes listable in its own right.
		s.vodsChanged(sess.baseOutDir, filepath.Join(sess.dest, sess.relDir))
	}
	s.logger.Info("Cancelled queued moves", "rows", n, "sessions", len(sessions))
	return int(n), nil
}

// moveSession identifies one session with pending moves, with the two folders a
// cancel changes the contents of.
type moveSession struct {
	key        streamer.StreamerKey
	sessionID  string
	baseOutDir string
	dest       string
	relDir     string
}

// pendingMoveSessions lists the distinct sessions with pending moves in scope.
func (s *Service) pendingMoveSessions(ctx context.Context, key *streamer.StreamerKey) ([]moveSession, error) {
	query := `SELECT DISTINCT COALESCE(platform, ''), COALESCE(username, ''),
	                 COALESCE(session_id, ''), base_out_dir, dest, rel_dir
	            FROM move_jobs WHERE status='pending'`
	var args []any
	if key != nil {
		query += ` AND platform=? AND username=?`
		args = append(args, key.Platform, key.Username)
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list pending move sessions: %w", err)
	}
	defer rows.Close()

	var out []moveSession
	for rows.Next() {
		var m moveSession
		if err := rows.Scan(&m.key.Platform, &m.key.Username, &m.sessionID,
			&m.baseOutDir, &m.dest, &m.relDir); err != nil {
			return nil, fmt.Errorf("scan pending move session: %w", err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// setCurrentMove records which move is executing, so a cancel can abort it.
// id 0 clears the registration.
func (s *Service) setCurrentMove(id int64, cancel context.CancelFunc) {
	s.curMoveMu.Lock()
	s.curMoveID, s.curMoveCancel = id, cancel
	s.curMoveMu.Unlock()
}

// abortCurrentMoveIfCancelled cancels the running move's context if its row is
// no longer pending. Reading the row back is what keeps this simple: the caller
// does not have to know which ids its UPDATE touched.
func (s *Service) abortCurrentMoveIfCancelled(ctx context.Context) {
	s.curMoveMu.Lock()
	id, cancel := s.curMoveID, s.curMoveCancel
	s.curMoveMu.Unlock()
	if id == 0 || cancel == nil {
		return
	}
	var status string
	if err := s.db.QueryRowContext(ctx,
		`SELECT status FROM move_jobs WHERE id=?`, id).Scan(&status); err != nil {
		return
	}
	if status == "cancelled" {
		s.logger.Info("Aborting move already in flight", "id", id)
		cancel()
	}
}

// sessionMovesCancelled reports whether a session has had its moves cancelled,
// which makes the cancellation stick: post-processing that is still producing
// files for this session must not queue fresh moves behind the user's back.
// Unlike the archive configuration, a per-session cancel cannot be re-derived,
// so the queue itself is the record of it.
func (s *Service) sessionMovesCancelled(ctx context.Context, sessionID string) bool {
	if s.db == nil || sessionID == "" {
		return false
	}
	var one int
	err := s.db.QueryRowContext(ctx,
		`SELECT 1 FROM move_jobs WHERE session_id=? AND status='cancelled' LIMIT 1`,
		sessionID).Scan(&one)
	return err == nil
}

// claimNextDueMove returns the oldest due move (next_retry_at <= now). A sweep
// is held back while any pending 'file' move shares its session so the folder is
// never removed before its VOD files are out. The second return value is the
// soonest future retry time among not-yet-due rows, so the worker can sleep
// until then.
func (s *Service) claimNextDueMove(ctx context.Context) (*moveJob, time.Time, error) {
	if s.db == nil {
		return nil, time.Time{}, fmt.Errorf("database not initialized")
	}
	now := time.Now().Unix()

	query := `
	SELECT id, kind, COALESCE(platform, ''), COALESCE(username, ''),
	       COALESCE(session_id, ''), base_out_dir, rel_dir,
	       COALESCE(file_name, ''), dest, COALESCE(writer_name, ''),
	       attempts, next_retry_at
	  FROM move_jobs m
	 WHERE status = 'pending'
	   AND next_retry_at <= ?
	   AND NOT (m.kind = 'sweep' AND EXISTS (
	         SELECT 1 FROM move_jobs f
	          WHERE f.status = 'pending' AND f.kind = 'file'
	            AND f.session_id = m.session_id))
	 ORDER BY next_retry_at ASC, id ASC
	 LIMIT 1`

	var mj moveJob
	err := s.db.QueryRowContext(ctx, query, now).Scan(
		&mj.id, &mj.kind, &mj.platform, &mj.username, &mj.sessionID,
		&mj.baseOutDir, &mj.relDir, &mj.fileName, &mj.dest, &mj.writerName,
		&mj.attempts, &mj.nextRetryAt)
	if err == nil {
		return &mj, time.Time{}, nil
	}
	if err != sql.ErrNoRows {
		return nil, time.Time{}, fmt.Errorf("failed to claim move: %w", err)
	}

	// Nothing due now; find the soonest future retry so we can wake for it.
	//
	// This MUST apply the same blocked-sweep exclusion as the claim query above:
	// a blocked sweep sits at next_retry_at = 0 and would otherwise make the
	// worker spin. Its session's pending file job supplies the real next time,
	// and runMoveWorker re-claims as soon as the last file completes.
	var next sql.NullInt64
	if err := s.db.QueryRowContext(ctx,
		`SELECT MIN(next_retry_at) FROM move_jobs m
		  WHERE m.status = 'pending'
		    AND NOT (m.kind = 'sweep' AND EXISTS (
		          SELECT 1 FROM move_jobs f
		           WHERE f.status = 'pending' AND f.kind = 'file'
		             AND f.session_id = m.session_id))`).Scan(&next); err != nil {
		return nil, time.Time{}, fmt.Errorf("failed to query next move: %w", err)
	}
	if next.Valid {
		return nil, time.Unix(next.Int64, 0), nil
	}
	return nil, time.Time{}, nil
}

// completeMove marks a move done.
//
// Guarded on status='pending' like every other outcome write: a move cancelled
// while its copy was in flight must stay cancelled, not be flipped to 'done' by
// the attempt that was already running.
func (s *Service) completeMove(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE move_jobs SET status='done', completed_at=? WHERE id=? AND status='pending'`,
		time.Now().Unix(), id)
	return err
}

// cancelMove retires a move whose streamer no longer archives anywhere. The
// status is deliberately not 'failed': nothing went wrong, the destination was
// unconfigured. Every queue query selects status='pending', so the row stops
// being claimed and stops holding the session's Moving badge open.
func (s *Service) cancelMove(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE move_jobs SET status='cancelled', completed_at=? WHERE id=?`,
		time.Now().Unix(), id)
	return err
}

// failMove records a failed attempt and schedules the next retry. Guarded on
// status='pending' so a cancelled move is not put back on the retry schedule by
// the attempt that was already running.
func (s *Service) failMove(ctx context.Context, id int64, attempts int, nextRetry time.Time, errMsg string, reason vodmove.Reason) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE move_jobs SET attempts=?, next_retry_at=?, last_error=?, last_reason=? WHERE id=? AND status='pending'`,
		attempts, nextRetry.Unix(), errMsg, string(reason), id)
	return err
}

// failMoveTerminal records a failure that no retry can fix, taking the job out
// of the queue for good. The claim query and the archive's badge both select on
// status='pending', so a retired row stops churning and stops holding the
// session's "Moving" badge open.
func (s *Service) failMoveTerminal(ctx context.Context, id int64, attempts int, errMsg string, reason vodmove.Reason) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE move_jobs SET status='failed', attempts=?, last_error=?, last_reason=?, completed_at=? WHERE id=? AND status='pending'`,
		attempts, errMsg, string(reason), time.Now().Unix(), id)
	return err
}

// pendingMovesForSession counts the still-pending moves for a session, used to
// decide when the Moving badge can clear.
func (s *Service) pendingMovesForSession(ctx context.Context, sessionID string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM move_jobs WHERE status='pending' AND session_id=?`,
		sessionID).Scan(&n)
	return n, err
}
