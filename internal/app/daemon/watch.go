package daemon

import (
	"context"
	"path/filepath"
	"time"

	"github.com/sgtdi/fswatcher"
)

// streamersPollInterval is how often the streamers file is polled for changes
// as a fallback when filesystem notifications don't arrive.
const streamersPollInterval = 2 * time.Second

// streamerFileChange decides whether an observed file hash represents an
// external edit that should trigger a resubscribe. It returns the updated
// "last seen" hash and whether to resubscribe. A hash equal to lastSeen means
// nothing changed since the previous check; a hash equal to lastWritten means
// the change was the daemon's own write (normalize/purge) and is ignored.
func streamerFileChange(h, lastSeen, lastWritten [32]byte) (newSeen [32]byte, resubscribe bool) {
	if h == lastSeen {
		return lastSeen, false
	}
	if h == lastWritten {
		return h, false
	}
	return h, true
}

func (a *App) watchStreamerConfigFile(ctx context.Context) {
	streamersPath := a.appCfg.Daemon.StreamersFilePath
	strCfgDir := filepath.Dir(streamersPath)

	// fsnotify gives instant response where it works, but events may never
	// arrive or the watcher may fail to initialize. A content-hash poll
	// guarantees changes are still picked up.
	var events <-chan fswatcher.WatchEvent
	if w, err := fswatcher.New(
		fswatcher.WithCooldown(200*time.Millisecond),
		fswatcher.WithPath(strCfgDir),
	); err != nil {
		a.log.Warn("File watcher unavailable, relying on polling", "error", err, "path", strCfgDir)
	} else {
		a.log.Debug("Watching streamer config file for changes", "path", strCfgDir)
		go w.Watch(ctx)
		events = w.Events()
	}

	// Resubscribe only on an actual content change, deduped by hash.
	lastSeen, _ := a.strFile.CurrentHash()
	reload := func(trigger string) {
		h, err := a.strFile.CurrentHash()
		if err != nil {
			a.log.Error("Failed to read streamer config file", "error", err)
			return
		}
		var resub bool
		lastSeen, resub = streamerFileChange(h, lastSeen, a.strFile.LastHash())
		if !resub {
			return
		}
		a.log.Info("Streamer config file changed", "path", streamersPath, "trigger", trigger)
		select {
		case a.cloudClient.ResubscribeCh <- struct{}{}:
		default: // a resubscribe is already queued; it will pick up the latest file
		}
	}

	ticker := time.NewTicker(streamersPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-events:
			if !ok {
				events = nil // watcher closed/unavailable; rely on polling
				continue
			}
			if event.Path != streamersPath {
				continue
			}
			reload("fsnotify")
		case <-ticker.C:
			reload("poll")
		}
	}
}
