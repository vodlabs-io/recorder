package processing

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/vodlabs-io/recorder/internal/vodmove"
)

// minMoveWait is the shortest the move worker will sleep between queue scans,
// so a queue that reports due work the claim query won't hand over can never
// become a busy loop (see runMoveWorker).
const minMoveWait = time.Second

// moveBackoff returns the delay before the n-th retry of a failed move,
// stepping 1m → 5m → 15m → 30m and holding at 30m thereafter. Moves retry
// forever (a network drive may be offline for hours), so there is no cap on the
// number of attempts.
func moveBackoff(attempts int) time.Duration {
	switch {
	case attempts <= 1:
		return time.Minute
	case attempts == 2:
		return 5 * time.Minute
	case attempts == 3:
		return 15 * time.Minute
	default:
		return 30 * time.Minute
	}
}

// noSpaceRetryInterval is how often a move blocked only by a full destination is
// re-checked. Short and flat, since the pre-flight check is a single statfs and
// freeing space should take effect promptly.
const noSpaceRetryInterval = time.Minute

// moveRetryAt returns when a failed move should next be attempted: the flat
// poll for a destination that is merely full, the escalating backoff for
// everything else.
func moveRetryAt(reason vodmove.Reason, attempts int) time.Time {
	if reason == vodmove.ReasonNoSpace {
		return time.Now().Add(noSpaceRetryInterval)
	}
	return time.Now().Add(moveBackoff(attempts))
}

// signalMove wakes the move worker without blocking; the worker re-scans the
// queue on each wake, so a single pending signal is enough.
func (s *Service) signalMove() {
	select {
	case s.moveAvailable <- struct{}{}:
	default:
	}
}

// runMoveWorker drains the move queue on its own goroutine so retrying moves
// never block ffmpeg post-processing. It claims one due move at a time, executes
// it, and otherwise sleeps until the next move is enqueued or its backoff
// elapses. Unlike runWorker it never exits on a transient DB error — moves must
// survive to retry — only on context cancellation.
func (s *Service) runMoveWorker(ctx context.Context) {
	s.logger.Info("Started move worker")
	defer s.logger.Info("Stopped move worker")
	defer close(s.moverDone)

	for {
		if ctx.Err() != nil {
			return
		}
		mj, nextDue, err := s.claimNextDueMove(ctx)
		if err != nil {
			s.logger.Error("Failed to claim move", "error", err)
			if !sleepCtx(ctx, 30*time.Second) {
				return
			}
			continue
		}
		if mj != nil {
			s.executeMove(ctx, mj)
			continue
		}

		// Nothing due now. Sleep until a move is enqueued, the next backoff
		// elapses, or the context is cancelled.
		wait := time.Minute
		if !nextDue.IsZero() {
			if d := time.Until(nextDue); d < wait {
				wait = d
			}
		}
		// Floor the wait, so a nextDue the claim query refused can't busy-loop.
		// A genuinely due job is returned by the claim above and never sleeps.
		if wait < minMoveWait {
			wait = minMoveWait
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-s.moveAvailable:
			timer.Stop()
		case <-timer.C:
		}
	}
}

// sleepCtx sleeps for d unless ctx is cancelled first. It returns false if the
// context was cancelled (the caller should stop).
func sleepCtx(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// executeMove runs one move job and records the outcome. Success clears the
// Moving badge once no moves remain for the session; failure schedules a backed
// off retry and flips the badge to the retrying state. A context cancellation
// (daemon shutdown) leaves the row pending and untouched so it resumes on the
// next start.
func (s *Service) executeMove(ctx context.Context, mj *moveJob) {
	// Archiving may have been switched off since this move was queued. Retire it
	// instead of retrying into an unconfigured folder forever; whatever is left
	// stays in the Download folder.
	if s.archivingDisabled(ctx, mj.strKey(), mj.baseOutDir) {
		s.logger.Info("Archiving is off, cancelling queued move",
			"id", mj.id, "kind", mj.kind, "session_id", mj.sessionID, "dest", mj.dest)
		if err := s.cancelMove(ctx, mj.id); err != nil {
			s.logger.Error("Failed to cancel move", "id", mj.id, "error", err)
			return
		}
		s.clearMoveStateIfDone(ctx, mj)
		// Both sides changed: the download folder keeps what was never moved, and
		// the destination folder stops being suppressed as a move in flight.
		s.vodsChanged(mj.baseOutDir, filepath.Join(mj.dest, mj.relDir))
		return
	}

	// Run under a context a cancel can abort, so a copy in flight stops instead
	// of finishing and deleting the local file. Shadowing ctx hands it to every
	// step below, including the shutdown checks, which treat an aborted move the
	// same way: leave the row as it is and return.
	ctx, abort := context.WithCancel(ctx)
	defer abort()
	s.setCurrentMove(mj.id, abort)
	defer s.setCurrentMove(0, nil)

	store, err := vodmove.ForDest(mj.dest)
	if err != nil {
		s.recordMoveFailure(ctx, mj, fmt.Errorf("invalid destination: %w", err))
		return
	}

	if err := s.preflightMove(ctx, mj); err != nil {
		if ctx.Err() != nil {
			return // shutdown: leave the row pending to resume next start
		}
		s.recordMoveFailure(ctx, mj, err)
		return
	}

	var moveErr error
	switch mj.kind {
	case "file":
		moveErr = s.moveOneFile(ctx, store, mj)
	case "sweep":
		moveErr = s.sweepSession(ctx, store, mj)
	default:
		moveErr = fmt.Errorf("unknown move kind %q", mj.kind)
	}

	if ctx.Err() != nil {
		return // shutdown: leave the row pending to resume next start
	}
	if moveErr != nil {
		s.recordMoveFailure(ctx, mj, moveErr)
		return
	}

	if err := s.completeMove(ctx, mj.id); err != nil {
		s.logger.Error("Failed to mark move done", "id", mj.id, "error", err)
		return
	}
	s.clearMoveStateIfDone(ctx, mj)
	// Files changed location: the downloaded session folder lost a file (or,
	// after the sweep, disappeared entirely) and the Archive folder gained one.
	// Both sides are named so the VOD Archive can re-check just those two.
	s.vodsChanged(mj.baseOutDir, filepath.Join(mj.dest, mj.relDir))
}

// moveOneFile moves a single produced VOD file into the Archive folder and
// deletes the downloaded copy.
func (s *Service) moveOneFile(ctx context.Context, store vodmove.Store, mj *moveJob) error {
	localPath := filepath.Join(mj.baseOutDir, mj.fileName)
	relPath := path.Join(filepath.ToSlash(mj.relDir), mj.fileName)
	if err := store.Store(ctx, localPath, relPath); err != nil {
		return err
	}
	if err := os.Remove(localPath); err != nil && !os.IsNotExist(err) {
		s.logger.Warn("Moved file but failed to remove downloaded copy",
			"path", localPath, "error", err)
	}
	s.logger.Info("Moved VOD file",
		"session_id", mj.sessionID,
		"platform", mj.platform,
		"username", mj.username,
		"file_name", mj.fileName,
		"dest", mj.dest)
	return nil
}

// sweepSession moves every remaining file in the local session folder to the
// destination, then removes the now-empty folder. It runs last for a session
// (the claim query holds it until all 'file' moves are done), so what remains is
// logs and any raw parts kept when delete_part_files is off.
func (s *Service) sweepSession(ctx context.Context, store vodmove.Store, mj *moveJob) error {
	base := mj.baseOutDir
	if _, err := os.Stat(base); os.IsNotExist(err) {
		return nil // already swept
	}

	var files []string
	var dirs []string
	err := filepath.WalkDir(base, func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == base {
			return nil
		}
		if e.IsDir() {
			dirs = append(dirs, p)
			return nil
		}
		files = append(files, p)
		return nil
	})
	if err != nil {
		return fmt.Errorf("walk session dir: %w", err)
	}

	for _, p := range files {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		rel, err := filepath.Rel(base, p)
		if err != nil {
			return fmt.Errorf("relativize %q: %w", p, err)
		}
		relPath := path.Join(filepath.ToSlash(mj.relDir), filepath.ToSlash(rel))
		if err := store.Store(ctx, p, relPath); err != nil {
			return fmt.Errorf("move %q: %w", rel, err)
		}
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove local %q: %w", rel, err)
		}
	}

	// Remove emptied subdirectories deepest-first, then the base itself.
	sort.Slice(dirs, func(i, j int) bool { return len(dirs[i]) > len(dirs[j]) })
	for _, d := range dirs {
		if err := os.Remove(d); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove subdir %q: %w", d, err)
		}
	}
	if err := os.Remove(base); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove session dir: %w", err)
	}

	// Climb up removing now-empty parent folders (e.g. the {username} and
	// {platform} dirs left by an output template), stopping at (and never
	// removing) the download root. base = <download root>/<relDir>, so trimming
	// the relDir suffix yields the download root; if it doesn't match (the
	// fallback layout, where base sits outside the download folder),
	// downloadRoot stays base and no parent is touched.
	rel := filepath.Clean(mj.relDir)
	downloadRoot := strings.TrimSuffix(base, string(filepath.Separator)+rel)
	for parent := filepath.Dir(base); len(parent) > len(downloadRoot) && parent != downloadRoot; parent = filepath.Dir(parent) {
		if err := os.Remove(parent); err != nil {
			// Already gone (a concurrent sweep): keep climbing. Otherwise the dir
			// is non-empty (another session lives there), so nothing above it can
			// be empty either — stop.
			if os.IsNotExist(err) {
				continue
			}
			break
		}
		s.logger.Info("Removed empty parent folder", "path", parent)
	}

	s.logger.Info("Swept session folder to destination",
		"session_id", mj.sessionID, "dest", mj.dest)
	return nil
}

// recordMoveFailure schedules a backed-off retry and surfaces the failure on the
// streamer card.
func (s *Service) recordMoveFailure(ctx context.Context, mj *moveJob, cause error) {
	attempts := mj.attempts + 1
	reason := vodmove.Classify(cause)

	// A source that no longer exists cannot be moved by any later attempt.
	// Retiring the row stops it warning every backoff interval forever and
	// releases the session's Moving badge; the error is kept on the row so the
	// loss stays visible rather than disappearing quietly.
	if reason.Terminal() {
		s.logger.Error("Move failed permanently, giving up",
			"id", mj.id, "kind", mj.kind, "attempts", attempts,
			"reason", string(reason), "error", cause)
		if err := s.failMoveTerminal(ctx, mj.id, attempts, cause.Error(), reason); err != nil {
			s.logger.Error("Failed to record terminal move failure", "id", mj.id, "error", err)
		}
		s.clearMoveStateIfDone(ctx, mj)
		return
	}

	next := moveRetryAt(reason, attempts)
	s.logger.Warn("Move failed, will retry",
		"id", mj.id, "kind", mj.kind, "attempts", attempts, "reason", string(reason),
		"next_retry", next.Format(time.RFC3339), "error", cause)
	if err := s.failMove(ctx, mj.id, attempts, next, cause.Error(), reason); err != nil {
		s.logger.Error("Failed to record move failure", "id", mj.id, "error", err)
	}
	s.store.SetMoveState(ctx, mj.strKey(), mj.sessionID, true, true)
}

// clearMoveStateIfDone clears the Moving badge for a session once no pending
// moves remain for it.
func (s *Service) clearMoveStateIfDone(ctx context.Context, mj *moveJob) {
	n, err := s.pendingMovesForSession(ctx, mj.sessionID)
	if err != nil {
		s.logger.Error("Failed to count pending moves", "session_id", mj.sessionID, "error", err)
		return
	}
	if n == 0 {
		s.store.SetMoveState(ctx, mj.strKey(), mj.sessionID, false, false)
	}
}
