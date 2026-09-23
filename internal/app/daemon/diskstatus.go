package daemon

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"time"

	"github.com/vodlabs-io/recorder/internal/diskspace"
)

// diskStatusKey is the KV key holding the daemon's Storage roots and their free
// space. It rides the clients' existing "daemon.>" KV watch, so the desktop app,
// the Web UI and the Dashboard all receive it without any new subscription.
const diskStatusKey = "daemon.disk_status"

// Sampling cadence. The fast rate engages once a root is already low, since
// concurrent recordings can write past the whole stop floor between two slow
// samples.
const (
	diskSampleInterval    = 60 * time.Second
	diskSampleLowInterval = 5 * time.Second
)

// diskStatus is the published document: every Storage root the daemon writes to,
// with what the last sample found.
type diskStatus struct {
	Roots []diskspace.Root `json:"roots"`
	// WarnBytes is the level the states were classified against, so a client can
	// explain the warning it is showing without reading config separately.
	WarnBytes uint64    `json:"warn_bytes"`
	SampledAt time.Time `json:"sampled_at"`
}

// runDiskMonitor samples the Storage roots and publishes them until ctx ends.
func (a *App) runDiskMonitor(ctx context.Context) {
	// Key for the set of roots the previous sample could not probe. Nil until
	// the first sample has landed: a daemon starting with an offline drive has
	// no transition to react to, and the index is doing its own first walk.
	var unreachable *string
	for {
		low, nowUnreachable := a.sampleAndPublishDisk(ctx)

		// A root coming back (or dropping out) changes which VODs the VOD
		// Archive should list, and the index's processing hooks never see it,
		// so rescan on the transition rather than waiting for the TTL.
		if unreachable != nil && *unreachable != nowUnreachable && a.vodIndex != nil {
			a.log.Info("Storage root reachability changed; rescanning the VOD Archive")
			a.vodIndex.Rescan()
		}
		unreachable = &nowUnreachable

		interval := diskSampleInterval
		if low {
			interval = diskSampleLowInterval
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

// sampleAndPublishDisk probes every Storage root, acts on the download root if
// it has crossed the hard floor, and publishes the result. It reports whether
// any root is low enough to warrant fast re-sampling, plus a key identifying
// the set of roots that could not be probed at all, so the caller can spot one
// appearing or disappearing.
func (a *App) sampleAndPublishDisk(ctx context.Context) (low bool, unreachable string) {
	roots := a.diskRoots(ctx)
	var unreachablePaths []string
	for _, r := range roots {
		// Unknown counts as a reason to re-sample quickly too, so a recovered
		// drive stops reading as unreachable promptly.
		if r.State != diskspace.StateOK {
			low = true
		}
		if r.State == diskspace.StateUnknown {
			// An unprobeable folder is never grouped with another, so a root in
			// this state carries exactly the one path.
			unreachablePaths = append(unreachablePaths, r.Paths...)
		}
		// Only the download root can cost footage: it is the one being written
		// to while a Broadcast is live. An archive root filling up wedges moves,
		// which is recoverable and needs no intervention here.
		if r.Download && r.State != diskspace.StateUnknown {
			if diskspace.MustStop(r.Usage) {
				a.stopRecordingForSpace(r)
			} else if diskspace.CanStart(r.Usage) && a.procSvc != nil {
				// Space is back above the start floor: wake any post-processing
				// job that deferred itself, rather than making it wait for the
				// next recording to finish.
				a.procSvc.SignalSpaceAvailable()
			}
		}
	}

	sort.Strings(unreachablePaths)
	unreachable = strings.Join(unreachablePaths, "\x00")

	if a.natsKV == nil {
		return low, unreachable
	}
	doc := diskStatus{Roots: roots, WarnBytes: a.lowSpaceWarnBytes(), SampledAt: time.Now()}
	b, err := json.Marshal(doc)
	if err != nil {
		a.log.Warn("Failed to encode disk status", "error", err)
		return low, unreachable
	}
	if _, err := a.natsKV.Put(ctx, diskStatusKey, b); err != nil {
		a.log.Warn("Failed to publish disk status", "error", err)
	}
	return low, unreachable
}

// diskRoots resolves the configured folders to Storage roots and probes each.
func (a *App) diskRoots(ctx context.Context) []diskspace.Root {
	a.cfgMu.RLock()
	downloadDir := a.appCfg.Session.DownloadDir
	a.cfgMu.RUnlock()

	return diskspace.Prober{
		Sampler:   diskspace.System(),
		WarnBytes: a.lowSpaceWarnBytes(),
	}.Group(ctx, downloadDir, a.vodArchiveRoots())
}

// lowSpaceWarnBytes returns the configured warning level in bytes.
func (a *App) lowSpaceWarnBytes() uint64 {
	a.cfgMu.RLock()
	defer a.cfgMu.RUnlock()
	gb := a.appCfg.Processing.LowSpaceWarnGB
	if gb <= 0 {
		return 0
	}
	return uint64(gb) << 30
}

// stopRecordingForSpace cancels active recordings once the download root has
// crossed the hard floor, so their Recordings finalize cleanly instead of being
// truncated mid-write when the filesystem refuses the next block.
//
// Sessions do not resume until free space clears the higher start floor. That
// gap is deliberate: restarting at the same level space was lost at would churn
// Sessions on and off while it hovered there.
func (a *App) stopRecordingForSpace(root diskspace.Root) {
	if a.sessionReg == nil {
		return
	}
	stopped := a.sessionReg.StopForSpace()
	if stopped > 0 {
		a.log.Warn("Stopped recordings: download folder is out of space",
			"sessions", stopped, "free_bytes", root.Usage.FreeBytes,
			"floor_bytes", uint64(diskspace.StopFloorBytes))
	}
}
