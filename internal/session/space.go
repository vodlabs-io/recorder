package session

import (
	"context"

	"github.com/vodlabs-io/recorder/internal/diskspace"
	"github.com/vodlabs-io/recorder/internal/streamer"
)

// downloadSpace probes the download folder's Storage root.
//
// The nearest existing ancestor is probed rather than the exact folder, because
// the download folder's per-session subfolders are created as recording starts.
func (r *Registry) downloadSpace(ctx context.Context) (diskspace.Usage, error) {
	cfg := r.sessionCfg()
	return diskspace.ProbeNearest(ctx, r.sampler, cfg.DownloadDir, diskspace.DefaultProbeTimeout)
}

// canStartSession reports whether there is room to begin recording.
//
// A probe that fails is not treated as "no room": an unreadable download folder
// is a different problem, and refusing every recording because of it would turn
// a reporting failure into lost Broadcasts.
func (r *Registry) canStartSession(ctx context.Context, key streamer.StreamerKey) bool {
	usage, err := r.downloadSpace(ctx)
	if err != nil {
		r.logger.Warn("Could not check free space; starting recording anyway",
			"streamer", key, "error", err)
		return true
	}
	if diskspace.CanStart(usage) {
		return true
	}
	r.logger.Warn("Refusing to start recording: download folder is out of space",
		"streamer", key, "free_bytes", usage.FreeBytes,
		"required_bytes", uint64(diskspace.StartFloorBytes))
	return false
}

// StopForSpace cancels every active session because the download folder has
// crossed the hard floor.
//
// Every session on the root stops at once: at the hard floor there are seconds
// of headroom, and a clean cancel finalizes what was recorded, where a write
// refused mid-part would lose its tail.
//
// It returns the number of sessions cancelled. Idempotent: with no active
// workers it does nothing.
func (r *Registry) StopForSpace() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for key, w := range r.workers {
		r.logger.Warn("Stopping recording: download folder is out of space", "streamer", key)
		w.cancelSession()
	}
	return len(r.workers)
}

// SetSampler replaces the free-space sampler. For tests: a full disk is not
// something CI can reproduce.
func (r *Registry) SetSampler(s diskspace.Sampler) { r.sampler = s }
