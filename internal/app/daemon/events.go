package daemon

import (
	"context"
	"path/filepath"
	"strings"

	"github.com/Masterminds/semver/v3"
	"github.com/vodlabs-io/recorder/internal/recording"
	"github.com/vodlabs-io/recorder/internal/streamer"
	"github.com/vodlabs-io/recorder/internal/version"
	"github.com/vodlabs-io/recorder/internal/vlconfig"
	"github.com/vodlabs-io/recorder/internal/vlstate"
)

func (a *App) onVodProcessed(ctx context.Context, v recording.Output) {
	a.log.Debug("Recording processed", "session_id", v.SessionID, "process_state", v.ProcessState)
	strKey := streamer.StreamerKey{Platform: v.Platform, Username: v.Username}

	// Only reflect processing state in KV while the streamer is still subscribed.
	// A removed one-shot has its KV purged immediately while its partial
	// Recording keeps processing; writing proc_state would resurrect its row.
	if a.cloudClient.IsSubscribed(streamer.StreamerRef{StreamerKey: strKey}) {
		a.store.SetProcessingState(ctx, strKey, v.SessionID, v.ProcessState)
	} else {
		a.log.Debug("Skipping proc_state write for unsubscribed streamer", "key", strKey)
	}

	if v.ProcessState == recording.ProcessFinished && a.resolveDeletePartFiles(ctx, strKey) {
		if n, err := v.RemovePartFiles(); err != nil {
			a.log.Warn("Failed to delete part files",
				"session_id", v.SessionID, "base_out_dir", v.BaseOutDir, "error", err)
		} else if n > 0 {
			a.log.Info("Deleted unprocessed part files",
				"session_id", v.SessionID, "base_out_dir", v.BaseOutDir, "count", n)
		}
	}

	// Sweep the rest of the session folder into the archive folder once
	// processing is done. Enqueued after RemovePartFiles so raw parts that should
	// be deleted are gone before the sweep runs (it would otherwise move them).
	// A no-op when the session had no archive folder.
	if v.ProcessState == recording.ProcessFinished && a.procSvc != nil {
		a.procSvc.EnqueueMoveSweep(ctx, v)
	}

	if ok, _ := vlstate.DeleteOneShot(ctx, a.natsKV, strKey); ok {
		a.log.Info("Removed streamer from one-shot list", "key", strKey)
		a.cloudClient.ResubscribeCh <- struct{}{}
	}
}

func (a *App) onVersionChecked(ctx context.Context, u version.Upstream) {
	a.log.Debug("Upstream version",
		"minimum", u.MinimumVersion,
		"maximum", u.MaximumVersion,
		"recommended", u.RecommendedVersion)

	if err := vlstate.PutUpstreamVersion(ctx, a.natsKV, u); err != nil {
		a.log.Warn("Failed to write upstream version to KV", "error", err)
	}

	current, err := semver.NewVersion(version.Version)
	if err != nil {
		a.log.Error("Failed to parse daemon version", "error", err)
		return
	}
	recommended, err := semver.NewVersion(u.RecommendedVersion)
	if err != nil {
		a.log.Error("Failed to parse recommended version", "error", err)
		return
	}
	minimum, err := semver.NewVersion(u.MinimumVersion)
	if err != nil {
		a.log.Error("Failed to parse minimum version", "error", err)
		return
	}
	if current.LessThan(recommended) {
		a.log.Warn("New version is available",
			"current_version", current,
			"recommended_version", recommended)
	}
	if current.LessThan(minimum) {
		a.log.Warn("Update required",
			"current_version", current,
			"minimum_version", minimum)
	}
}

// resolveDeletePartFiles reports whether a streamer's raw Parts should
// be deleted after processing, using the same precedence chain as the move
// resolver: global config → one-shot KV → streamers.toml rule. The processing
// service calls it to free a group's raw parts as each vodN is produced, and
// onVodProcessed uses it for the end-of-session cleanup.
func (a *App) resolveDeletePartFiles(ctx context.Context, strKey streamer.StreamerKey) bool {
	del := a.defaultDeletePartFiles()
	if opts, ok := vlstate.GetOneShotOptions(ctx, a.natsKV, strKey); ok && opts.DeletePartFiles != nil {
		del = *opts.DeletePartFiles
	}
	if a.strFile != nil {
		if opts := a.strFile.OptionsFor(streamer.StreamerRef{StreamerKey: strKey}); opts.DeletePartFiles != nil {
			del = *opts.DeletePartFiles
		}
	}
	return del
}

// resolveArchiveDir resolves a session's effective archive folder and the
// relative directory its files should occupy there. It follows the same
// precedence chain as resolveDeletePartFiles (global → one-shot KV →
// streamers.toml rule), with the per-streamer "off" sentinel disabling
// archiving for that streamer. The result is snapshotted onto each
// group/finalize job at enqueue time so it survives the one-shot KV purge in
// onVodProcessed. An empty dest disables archiving. The relative directory
// mirrors the session's path under the download folder so the archive folder
// reproduces the same {platform}/{username}/… layout.
func (a *App) resolveArchiveDir(ctx context.Context, strKey streamer.StreamerKey, baseOutDir string) (dest, relDir string) {
	raw, downloadDir := a.defaultArchiveDir()

	if opts, ok := vlstate.GetOneShotOptions(ctx, a.natsKV, strKey); ok && opts.ArchiveDir != "" {
		raw = opts.ArchiveDir
	}
	if a.strFile != nil {
		if opts := a.strFile.OptionsFor(streamer.StreamerRef{StreamerKey: strKey}); opts.ArchiveDir != "" {
			raw = opts.ArchiveDir
		}
	}

	raw = strings.TrimSpace(raw)
	if raw == "" || vlconfig.IsArchiveOff(raw) {
		return "", ""
	}
	if err := vlconfig.ValidateArchiveDir(raw); err != nil {
		a.log.Warn("Ignoring invalid archive_dir at processing time", "value", raw, "error", err)
		return "", ""
	}
	if err := vlconfig.ValidateArchiveDirAgainstDownloadDir(raw, downloadDir); err != nil {
		a.log.Warn("Ignoring archive_dir overlapping download folder", "value", raw, "error", err)
		return "", ""
	}

	// Mirror the session's relative path under the download folder. If the
	// download folder has changed since recording (Rel fails or escapes), fall
	// back to a platform/username/<basename> layout.
	relDir, err := filepath.Rel(downloadDir, baseOutDir)
	if err != nil || relDir == ".." || strings.HasPrefix(relDir, ".."+string(filepath.Separator)) {
		relDir = filepath.Join(strKey.Platform, strKey.Username, filepath.Base(baseOutDir))
		a.log.Warn("Session dir is outside the download folder; using fallback archive layout",
			"base_out_dir", baseOutDir, "rel_dir", relDir)
	}
	return raw, relDir
}
