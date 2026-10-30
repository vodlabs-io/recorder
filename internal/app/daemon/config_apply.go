package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/knadh/koanf/parsers/toml"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/v2"
	"github.com/vodlabs-io/recorder/internal/encoding"
	"github.com/vodlabs-io/recorder/internal/vlapi"
	"github.com/vodlabs-io/recorder/internal/vlconfig"
)

// GetConfig returns the current user-facing global configuration.
// Implements vlapi's configController.
func (a *App) GetConfig() vlapi.UserConfig {
	a.cfgMu.RLock()
	defer a.cfgMu.RUnlock()
	return vlapi.UserConfig{
		DownloadDir:     a.appCfg.Session.DownloadDir,
		Variant:         a.appCfg.Session.Variant,
		OutputTemplate:  a.appCfg.Session.OutputTemplate,
		DeletePartFiles: a.appCfg.Processing.DeletePartFiles,
		SplitSize:       a.appCfg.Session.SplitSize,
		SplitDuration:   a.appCfg.Session.SplitDuration,
		ArchiveDir:      a.appCfg.Processing.ArchiveDir,
		// Normalized on the way out, not just on the way in: a hand-edited
		// config.toml holds whatever was typed, and clients match a listed VOD's
		// root against these strings exactly.
		ScanDirs:       vlconfig.NormalizeScanDirs(a.appCfg.Processing.ScanDirs),
		Encoder:        displayEncoder(a.appCfg.Processing.Encoder),
		LowSpaceWarnGB: a.appCfg.Processing.LowSpaceWarnGB,
	}
}

// displayEncoder maps the stored encoder value to its API spelling: an unset
// value reads as "auto".
func displayEncoder(v string) string {
	if v == "" {
		return "auto"
	}
	return v
}

// SetConfig validates, persists to config.toml, and live-applies the user-facing
// global configuration. Changes take effect for new recordings/processing;
// in-progress work is not interrupted. Implements vlapi's configController.
func (a *App) SetConfig(c vlapi.UserConfig) error {
	downloadDir := strings.TrimSpace(c.DownloadDir)
	if downloadDir == "" {
		downloadDir = vlconfig.DefaultDownloadDir()
	}
	downloadDir, err := filepath.Abs(downloadDir)
	if err != nil {
		return fmt.Errorf("invalid download_dir: %w", err)
	}
	variant := strings.TrimSpace(c.Variant)
	if variant == "" {
		variant = "best"
	}

	// Validate split fields. Global empty = disabled; off-tokens map to "".
	splitSize := strings.TrimSpace(c.SplitSize)
	if vlconfig.IsSplitOff(splitSize) {
		splitSize = ""
	}
	if splitSize != "" {
		if _, err := vlconfig.ParseSplitSize(splitSize); err != nil {
			return fmt.Errorf("invalid split_size: %w", err)
		}
	}
	splitDuration := strings.TrimSpace(c.SplitDuration)
	if vlconfig.IsSplitOff(splitDuration) {
		splitDuration = ""
	}
	if splitDuration != "" {
		if _, err := vlconfig.ParseSplitDuration(splitDuration); err != nil {
			return fmt.Errorf("invalid split_duration: %w", err)
		}
	}

	// Validate the archive folder. Global empty = disabled; an off-token also
	// disables (the per-streamer "off" sentinel has no meaning globally). A
	// real path must be valid and must not overlap the download folder.
	archiveDir := strings.TrimSpace(c.ArchiveDir)
	if vlconfig.IsArchiveOff(archiveDir) {
		archiveDir = ""
	}
	if archiveDir != "" {
		if err := vlconfig.ValidateArchiveDir(archiveDir); err != nil {
			return fmt.Errorf("invalid archive_dir: %w", err)
		}
		if err := vlconfig.ValidateArchiveDirAgainstDownloadDir(archiveDir, downloadDir); err != nil {
			return err
		}
		// Reject an archive folder the daemon can't write to (an unmounted drive,
		// a container path with no volume behind it) so the move doesn't fail
		// every retry at runtime.
		if err := vlapi.CheckArchiveDirWritable(archiveDir); err != nil {
			return err
		}
	}

	// Scan folders are read-only, so unlike the archive folder they are not
	// checked for writability or created: an unplugged drive is reported by the
	// VOD Archive, not a reason to refuse the save. Overlap with a write
	// destination is refused (see ValidateScanDirs).
	scanDirs := vlconfig.NormalizeScanDirs(c.ScanDirs)
	if err := vlconfig.ValidateScanDirs(scanDirs, downloadDir, a.perStreamerArchiveDirs(archiveDir)); err != nil {
		return err
	}

	// Validate the encoder preference against this platform's known encoders.
	encoder := strings.TrimSpace(c.Encoder)
	if encoder == "" {
		encoder = "auto"
	}
	if encoder != "auto" && encoder != "software" {
		valid := false
		for _, id := range encoding.KnownEncoderIDs() {
			if encoder == id {
				valid = true
				break
			}
		}
		if !valid {
			return fmt.Errorf("invalid encoder %q (use auto, software, or one of %s)",
				encoder, strings.Join(encoding.KnownEncoderIDs(), ", "))
		}
	}

	// A negative warning level is meaningless; clamp rather than reject, so a
	// stray value cannot lock the user out of saving the rest of the form.
	lowSpaceWarnGB := c.LowSpaceWarnGB
	if lowSpaceWarnGB < 0 {
		lowSpaceWarnGB = 0
	}

	applied := vlapi.UserConfig{
		DownloadDir:     downloadDir,
		Variant:         variant,
		OutputTemplate:  c.OutputTemplate, // empty falls back to DefaultOutputTemplate at render time
		DeletePartFiles: c.DeletePartFiles,
		SplitSize:       splitSize,
		SplitDuration:   splitDuration,
		ArchiveDir:      archiveDir,
		ScanDirs:        scanDirs,
		Encoder:         encoder,
		LowSpaceWarnGB:  lowSpaceWarnGB,
	}

	a.cfgMu.Lock()
	a.appCfg.Session.DownloadDir = applied.DownloadDir
	a.appCfg.Session.Variant = applied.Variant
	a.appCfg.Session.OutputTemplate = applied.OutputTemplate
	a.appCfg.Processing.DeletePartFiles = applied.DeletePartFiles
	a.appCfg.Session.SplitSize = applied.SplitSize
	a.appCfg.Session.SplitDuration = applied.SplitDuration
	a.appCfg.Processing.ArchiveDir = applied.ArchiveDir
	a.appCfg.Processing.ScanDirs = applied.ScanDirs
	a.appCfg.Processing.Encoder = applied.Encoder
	a.appCfg.Processing.LowSpaceWarnGB = applied.LowSpaceWarnGB
	sessionCfg := a.appCfg.Session
	configPath := a.appCfg.Daemon.ConfigFilePath
	a.cfgMu.Unlock()

	// A folder the user just picked or typed may not exist yet; create it so the
	// disk status reports the new location straight away instead of showing it
	// as unreachable until the first recording or move lands there.
	if err := vlconfig.EnsureStorageDir(applied.DownloadDir); err != nil {
		a.log.Warn("Could not create the download folder",
			"path", applied.DownloadDir, "error", err)
	}
	if applied.ArchiveDir != "" {
		if err := vlconfig.EnsureStorageDir(applied.ArchiveDir); err != nil {
			a.log.Warn("Could not create the archive folder",
				"path", applied.ArchiveDir, "error", err)
		}
	}

	// Live-apply to the running session registry (affects new sessions only).
	if a.sessionReg != nil {
		a.sessionReg.SetConfig(sessionCfg)
	}

	// Archiving may have just been switched off for every streamer that inherits
	// the global setting, so queued moves are re-examined against what was just
	// applied.
	a.ArchiveConfigChanged()

	a.cfgFileMu.Lock()
	err = writeUserConfig(configPath, applied)
	a.cfgFileMu.Unlock()
	if err != nil {
		return fmt.Errorf("failed to persist config: %w", err)
	}
	return nil
}

// ArchiveConfigChanged tells the move queue to re-examine itself after an edit
// to the global archive folder or to a streamer's override, so a move that now
// has nowhere to go is retired instead of retrying. Implements vlapi's
// configController, which is how the per-streamer options handler reaches it;
// SetConfig calls it directly.
func (a *App) ArchiveConfigChanged() {
	if a.procSvc == nil {
		return
	}
	a.procSvc.ArchiveConfigChanged(context.Background())
}

// defaultDeletePartFiles returns the global [processing] delete_part_files
// setting under lock, so the processing callback reads a consistent value while
// config.set may be updating it concurrently.
func (a *App) defaultDeletePartFiles() bool {
	a.cfgMu.RLock()
	defer a.cfgMu.RUnlock()
	return a.appCfg.Processing.DeletePartFiles
}

// defaultArchiveDir returns the global [processing] archive_dir and download_dir
// under lock, for use by the processing callback when resolving a session's
// effective destination.
func (a *App) defaultArchiveDir() (dest string, downloadDir string) {
	a.cfgMu.RLock()
	defer a.cfgMu.RUnlock()
	return a.appCfg.Processing.ArchiveDir, a.appCfg.Session.DownloadDir
}

// writeUserConfig persists the user-facing fields into config.toml, preserving
// the file's other keys. The file is loaded fresh (without defaults or flags) so
// the on-disk config stays minimal; comments are not preserved across a write.
func writeUserConfig(path string, c vlapi.UserConfig) error {
	if path == "" {
		return fmt.Errorf("config file path is empty")
	}
	if err := vlconfig.EnsureConfigFileExists(path); err != nil {
		return fmt.Errorf("failed to ensure config file exists: %w", err)
	}
	k := koanf.New(".")
	if err := k.Load(file.Provider(path), toml.Parser()); err != nil {
		return fmt.Errorf("failed to load config file: %w", err)
	}
	k.Set("session.download_dir", c.DownloadDir)
	k.Set("session.variant", c.Variant)
	k.Set("session.output_template", c.OutputTemplate)
	k.Set("processing.delete_part_files", c.DeletePartFiles)
	k.Set("session.split_size", c.SplitSize)
	k.Set("session.split_duration", c.SplitDuration)
	k.Set("processing.archive_dir", c.ArchiveDir)
	// Always written, even when empty: koanf keeps the file's previous value for
	// a key that is not set, so clearing the last scan folder must write [].
	k.Set("processing.scan_dirs", nonNilStrings(c.ScanDirs))
	k.Set("processing.encoder", c.Encoder)
	k.Set("processing.low_space_warn_gb", c.LowSpaceWarnGB)

	b, err := toml.Parser().Marshal(k.Raw())
	if err != nil {
		return fmt.Errorf("failed to marshal config: %w", err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		return fmt.Errorf("failed to write config file %s: %w", path, err)
	}
	return nil
}

// perStreamerArchiveDirs is every folder a VOD can be moved into: the
// global archive folder plus each streamer's archive_dir override. A scan
// folder is validated against all of them, since any one of them can receive a
// move.
func (a *App) perStreamerArchiveDirs(global string) []string {
	dirs := []string{global}
	if a.strFile != nil {
		for _, id := range a.strFile.AllIDs() {
			if opts := a.strFile.OptionsFor(id); opts.ArchiveDir != "" {
				dirs = append(dirs, opts.ArchiveDir)
			}
		}
	}
	return dirs
}

// nonNilStrings guarantees a slice the TOML marshaller renders as [] rather
// than omitting or nulling the key.
func nonNilStrings(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}
