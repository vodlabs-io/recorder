package processing

import (
	"context"
	"database/sql"

	"github.com/vodlabs-io/recorder/internal/vodindex"
)

// SetVODsChangedNotifier installs fn to be called whenever processing changes
// what the VOD Archive would list: a group produced, a session finalized, a
// move completed. dirs are the session folders affected, so the VOD Archive
// index can re-check just those. Called once during daemon startup.
func (s *Service) SetVODsChangedNotifier(fn func(dirs ...string)) {
	s.notifyVODsChanged = fn
}

func (s *Service) vodsChanged(dirs ...string) {
	if s.notifyVODsChanged != nil {
		s.notifyVODsChanged(dirs...)
	}
}

// SetThumbnailCacher installs fn to receive each screenshot processing produces
// while the JPEG is still on local disk, since reading it back from the Archive
// folder after the move can be far slower. Called once during daemon startup.
func (s *Service) SetThumbnailCacher(fn func(sessionKey, file, localPath string)) {
	s.cacheThumbnail = fn
}

func (s *Service) thumbnailCached(sessionKey, file, localPath string) {
	if s.cacheThumbnail != nil {
		s.cacheThumbnail(sessionKey, file, localPath)
	}
}

// ActiveVODWork reports the sessions that finished recording but are still
// being worked on: concat/finalize jobs pending or running, and pending moves.
// The VOD Archive scanner uses it to badge those sessions and suppress the
// half-copied destination folder of an in-flight move. Mid-recording rows
// (eager convert/group work while the stream is live) are excluded, since the
// VOD Archive doesn't list active Sessions.
func (s *Service) ActiveVODWork(ctx context.Context) []vodindex.ActiveWork {
	if s.db == nil {
		return nil
	}
	byDir := make(map[string]vodindex.ActiveWork)

	// Post-recording processing: rows enqueued at session end carry
	// session_state='finished' (mid-session convert/group rows say 'recording').
	rows, err := s.db.QueryContext(ctx, `
		SELECT base_out_dir, platform, username, session_id
		FROM postprocess_jobs
		WHERE status IN ('pending', 'processing') AND session_state = ?`,
		"finished")
	if err == nil {
		func() {
			defer rows.Close()
			for rows.Next() {
				var w vodindex.ActiveWork
				if err := rows.Scan(&w.BaseOutDir, &w.Platform, &w.Username, &w.SessionID); err != nil {
					return
				}
				w.State = vodindex.StateProcessing
				byDir[w.BaseOutDir] = w
			}
		}()
	}

	// Pending moves. Processing wins over moving when both exist for a session
	// (the sweep only runs after processing finished, so overlap means an
	// earlier file move is still retrying).
	mrows, err := s.db.QueryContext(ctx, `
		SELECT base_out_dir, platform, username, session_id, dest, rel_dir, last_reason
		FROM move_jobs
		WHERE status = 'pending'`)
	if err == nil {
		func() {
			defer mrows.Close()
			for mrows.Next() {
				var w vodindex.ActiveWork
				var platform, username, sessionID, reason sql.NullString
				if err := mrows.Scan(&w.BaseOutDir, &platform, &username, &sessionID, &w.Dest, &w.RelDir, &reason); err != nil {
					return
				}
				w.Platform = platform.String
				w.Username = username.String
				w.SessionID = sessionID.String
				w.State = vodindex.StateMoving
				w.Reason = reason.String

				existing, exists := byDir[w.BaseOutDir]
				switch {
				case !exists:
					byDir[w.BaseOutDir] = w
				case existing.State == vodindex.StateMoving && existing.Reason == "" && w.Reason != "":
					// Take the reason from whichever of a session's move rows has
					// one: rows arrive ordered by next_retry_at, so the
					// unattempted sweep row (0, no reason) comes first.
					existing.Reason = w.Reason
					byDir[w.BaseOutDir] = existing
				}
			}
		}()
	}

	work := make([]vodindex.ActiveWork, 0, len(byDir))
	for _, w := range byDir {
		work = append(work, w)
	}
	return work
}

// SessionBusy reports whether the session folder still has processing or move
// work in flight. The VOD Archive's delete refuses busy sessions so it can't yank
// files out from under a running concat or move.
func (s *Service) SessionBusy(ctx context.Context, baseOutDir string) bool {
	if s.db == nil {
		return false
	}
	var n int
	err := s.db.QueryRowContext(ctx, `
		SELECT
			(SELECT COUNT(*) FROM postprocess_jobs
			 WHERE base_out_dir = ? AND status IN ('pending', 'processing')) +
			(SELECT COUNT(*) FROM move_jobs
			 WHERE base_out_dir = ? AND status = 'pending')`,
		baseOutDir, baseOutDir).Scan(&n)
	return err == nil && n > 0
}
