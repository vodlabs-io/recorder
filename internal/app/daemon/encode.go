package daemon

import (
	"context"
	"fmt"
	"os"

	"github.com/vodlabs-io/recorder/internal/encoding"
	"github.com/vodlabs-io/recorder/internal/vlapi"
	"github.com/vodlabs-io/recorder/internal/vodindex"
)

// currentEncoderPref reads the configured encoder under lock ("" and "auto"
// both mean automatic selection).
func (a *App) currentEncoderPref() string {
	a.cfgMu.RLock()
	defer a.cfgMu.RUnlock()
	return a.appCfg.Processing.Encoder
}

// EnqueueMerge implements vlapi.EncodeController. Session-level validation
// happens here (the encoding service validates the folder contents): the
// session must be listed, ready, and not busy in either work queue.
func (a *App) EnqueueMerge(ctx context.Context, dir string, deleteParts bool) (encoding.Job, error) {
	session, ok := a.vodIndex.Find(ctx, dir)
	if !ok {
		// The index only carries finished sessions, so a folder that is still
		// recording misses here. Saying "not an archived session" about a
		// folder the user can see filling up describes the index, not their
		// situation.
		if isDir(dir) {
			return encoding.Job{}, fmt.Errorf(
				"%w: %s is not a finished recording yet — wait for it to stop, or check the path names a session folder",
				vlapi.ErrNotFound, dir)
		}
		return encoding.Job{}, fmt.Errorf("%w: no such session folder: %s", vlapi.ErrNotFound, dir)
	}
	if session.State != vodindex.StateReady {
		return encoding.Job{}, fmt.Errorf("%w: session is still %s", vlapi.ErrConflict, session.State)
	}
	// A merge rewrites the session folder in place, which a scan folder exists
	// to be spared. Refused rather than redirected: the user picked that folder
	// as read-only, and silently writing the result somewhere else would leave
	// the VOD they acted on unchanged.
	if a.isReadOnlyVODRoot(session.Root) {
		return encoding.Job{}, fmt.Errorf(
			"%w: %s is in the read-only scan folder %s; copy the VOD into the download folder to merge it",
			vlapi.ErrConflict, session.Dir, session.Root)
	}
	if (a.procSvc != nil && a.procSvc.SessionBusy(ctx, session.Dir)) ||
		(a.encSvc != nil && a.encSvc.SessionBusy(ctx, session.Dir)) {
		return encoding.Job{}, fmt.Errorf("%w: session has work in flight", vlapi.ErrConflict)
	}

	meta := encoding.SessionMeta{
		Platform:  session.Platform,
		Username:  session.Username,
		SessionID: session.SessionID,
	}
	return a.encSvc.EnqueueMerge(ctx, session.Dir, meta, deleteParts, a.currentEncoderPref())
}

// ListEncodeQueue implements vlapi.EncodeController.
func (a *App) ListEncodeQueue(ctx context.Context) (encoding.Queue, error) {
	return a.encSvc.List(ctx)
}

// RemoveEncodeJob implements vlapi.EncodeController.
func (a *App) RemoveEncodeJob(ctx context.Context, id int64) error {
	return a.encSvc.Remove(ctx, id)
}

// ReorderEncodeJob implements vlapi.EncodeController.
func (a *App) ReorderEncodeJob(ctx context.Context, id int64, direction string) error {
	return a.encSvc.Reorder(ctx, id, direction)
}

// CancelEncodeJob implements vlapi.EncodeController.
func (a *App) CancelEncodeJob(ctx context.Context, id int64) error {
	return a.encSvc.Cancel(ctx, id)
}

// RetryEncodeJob implements vlapi.EncodeController. The retried job
// re-snapshots the current encoder preference.
func (a *App) RetryEncodeJob(ctx context.Context, id int64) error {
	return a.encSvc.Retry(ctx, id, a.currentEncoderPref())
}

// SetEncodeQueuePaused implements vlapi.EncodeController.
func (a *App) SetEncodeQueuePaused(ctx context.Context, paused bool) error {
	return a.encSvc.SetPaused(ctx, paused)
}

// ListEncoders implements vlapi.EncodeController.
func (a *App) ListEncoders(ctx context.Context, refresh bool) (vlapi.EncoderList, error) {
	encoders, err := a.encSvc.DetectEncoders(ctx, refresh)
	if err != nil {
		return vlapi.EncoderList{}, err
	}
	return vlapi.EncoderList{
		Selected: displayEncoder(a.currentEncoderPref()),
		Encoders: encoders,
	}, nil
}

// Compile-time check that App satisfies the encode controller contract.
var _ vlapi.EncodeController = (*App)(nil)

// isDir reports whether path exists and is a directory, separating "still
// recording" from "wrong path" in the enqueue error.
func isDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}
