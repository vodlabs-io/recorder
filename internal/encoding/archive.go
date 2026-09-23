package encoding

import (
	"context"
	"database/sql"

	"github.com/vodlabs-io/recorder/internal/vodindex"
)

// ActiveVODWork reports sessions with queued or running merges so the
// VOD Archive scanner badges them. A job in its copy stage surfaces as "moving"
// (encode done, result being written to possibly-slow storage — the same
// user-visible situation as a post-processing move); everything else is
// "encoding". Dest/RelDir stay empty: the copy targets the session folder
// itself, so there is no second location to suppress.
func (s *Service) ActiveVODWork(ctx context.Context) []vodindex.ActiveWork {
	if s.db == nil {
		return nil
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT session_dir, platform, username, session_id, status, stage
		FROM encode_jobs
		WHERE status IN ('queued', 'running')`)
	if err != nil {
		return nil
	}
	defer rows.Close()

	byDir := make(map[string]vodindex.ActiveWork)
	for rows.Next() {
		var w vodindex.ActiveWork
		var platform, username, sessionID, status, stage sql.NullString
		if err := rows.Scan(&w.BaseOutDir, &platform, &username, &sessionID, &status, &stage); err != nil {
			return nil
		}
		w.Platform = platform.String
		w.Username = username.String
		w.SessionID = sessionID.String
		w.State = vodindex.StateEncoding
		if status.String == statusRunning && stage.String == stageCopy {
			w.State = vodindex.StateMoving
		}
		byDir[w.BaseOutDir] = w
	}

	work := make([]vodindex.ActiveWork, 0, len(byDir))
	for _, w := range byDir {
		work = append(work, w)
	}
	return work
}

// SessionBusy reports whether the session folder has a queued or running
// merge. The VOD Archive's delete (and a second enqueue) refuse busy sessions.
func (s *Service) SessionBusy(ctx context.Context, baseOutDir string) bool {
	if s.db == nil {
		return false
	}
	var n int
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM encode_jobs
		WHERE session_dir = ? AND status IN ('queued', 'running')`,
		baseOutDir).Scan(&n)
	return err == nil && n > 0
}
