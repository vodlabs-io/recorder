package vlapi

import (
	"encoding/json"
	"runtime"

	"github.com/nats-io/nats.go/micro"
	"github.com/vodlabs-io/recorder/internal/mountinfo"
	"github.com/vodlabs-io/recorder/internal/vlconfig"
)

// UserConfig is the user-facing subset of global daemon configuration exposed
// over the API for the desktop GUI / CLI to read and edit. The remaining config
// (logger, daemon transport, cloud, scraper) stays file-only.
type UserConfig struct {
	DownloadDir     string `json:"download_dir"`
	Variant         string `json:"variant"`
	OutputTemplate  string `json:"output_template"`
	DeletePartFiles bool   `json:"delete_part_files"`
	// SplitSize is the global maximum raw bytes per VOD file (e.g. "2GB").
	// Empty string means no size-based splitting.
	SplitSize string `json:"split_size"`
	// SplitDuration is the global maximum duration per VOD file (e.g. "1h30m").
	// Empty string means no duration-based splitting.
	SplitDuration string `json:"split_duration"`
	// ArchiveDir is the Archive folder VODs are moved to after processing (e.g.
	// a network drive). Empty means VODs stay in the Download folder.
	ArchiveDir string `json:"archive_dir"`
	// ScanDirs are read-only folders the VOD Archive also lists VODs from. They
	// are never written to, so they are not Storage roots and carry no
	// free-space warning; a VOD whose root is one of these cannot be merged.
	// Always absolute and cleaned, so a client can match a listed VOD's root
	// against this list by exact string comparison.
	ScanDirs []string `json:"scan_dirs"`
	// Encoder selects the video encoder for merge/re-encode jobs: "auto"
	// (default), "software" (libx264), or a specific encoder id.
	Encoder string `json:"encoder"`
	// LowSpaceWarnGB is the free space, in GiB, below which a Storage root is
	// reported as running low. 0 disables the warning.
	LowSpaceWarnGB int `json:"low_space_warn_gb"`
}

// configController is implemented by the daemon to read and live-apply the
// user-facing configuration. SetConfig persists config.toml and updates the
// running session/processing components so changes take effect for new work
// without interrupting active recordings.
type configController interface {
	GetConfig() UserConfig
	SetConfig(UserConfig) error
	// ArchiveConfigChanged reports that the archive destination configuration
	// was edited, so queued moves can be re-examined against it. SetConfig
	// already covers the global setting; this is for the per-streamer overrides,
	// which are written to streamers.toml without passing through SetConfig.
	ArchiveConfigChanged()
}

// handleGetConfig returns the current user-facing global configuration.
func (s *Service) handleGetConfig(req micro.Request) {
	if s.configCtl == nil {
		respond(req, fail(errInternal, "config controller unavailable"))
		return
	}
	respond(req, OK(s.configCtl.GetConfig()))
}

// handleSetConfig persists and live-applies the user-facing global
// configuration, then responds with the resulting effective values.
func (s *Service) handleSetConfig(req micro.Request) {
	if s.configCtl == nil {
		respond(req, fail(errInternal, "config controller unavailable"))
		return
	}
	var cfg UserConfig
	if err := json.Unmarshal(req.Data(), &cfg); err != nil {
		respond(req, fail(errInvalidArg, "invalid JSON"))
		return
	}
	if err := s.configCtl.SetConfig(cfg); err != nil {
		respond(req, fail(errInvalidArg, err.Error()))
		return
	}
	respond(req, OK(s.configCtl.GetConfig()))
}

// handleConfigFolders suggests folders for the folder settings: in a Linux
// container, the mounted volumes other than the container's own and the
// download folder, since only a mounted folder reaches the host. Elsewhere
// the list is empty.
func (s *Service) handleConfigFolders(req micro.Request) {
	folders := []mountinfo.Mount{}
	if runtime.GOOS == "linux" && vlconfig.RunningInContainer() {
		exclude := ""
		if s.configCtl != nil {
			exclude = s.configCtl.GetConfig().DownloadDir
		}
		if f := mountinfo.Folders(exclude); f != nil {
			folders = f
		}
	}
	respond(req, OK(folders))
}
