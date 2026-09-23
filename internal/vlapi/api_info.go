package vlapi

import (
	"context"
	"os"
	"runtime"
	"time"

	"github.com/nats-io/nats.go/micro"
	"github.com/vodlabs-io/recorder/internal/cloud"
	"github.com/vodlabs-io/recorder/internal/diskspace"
	"github.com/vodlabs-io/recorder/internal/version"
	"github.com/vodlabs-io/recorder/internal/vllog"
)

// infoDiskTimeout bounds the whole free-space sample for one info request, so an
// offline network drive cannot hold up `vod info`. Individual probes have their
// own shorter timeout; this caps the total across several roots.
const infoDiskTimeout = 10 * time.Second

type DaemonInfo struct {
	Version string `json:"version"`
	// Commit the daemon was built from, so a bug report names the exact build
	// rather than a version several builds share.
	Commit string `json:"commit,omitempty"`
	// OS and Arch of the machine the daemon runs on. A recording bug is
	// usually one of platform, OS or architecture, and only this says which.
	OS                string `json:"os"`
	Arch              string `json:"arch"`
	Container         bool   `json:"container"`
	NatsURL           string `json:"nats_url"`
	CloudAPIURL       string `json:"cloud_api_url"`
	CloudNATSURL      string `json:"cloud_nats_url"`
	ExecutablePath    string `json:"executable_path"`
	ConfigFilePath    string `json:"config_file_path"`
	StreamersFilePath string `json:"streamers_file_path"`
	DownloadDirectory string `json:"download_dir"`
	// ArchiveDirectory is the global Archive folder VODs are moved to. Empty
	// when archiving is off. Per-streamer rules can override it.
	ArchiveDirectory      string `json:"archive_dir"`
	DaemonLogFilePath     string `json:"daemon_log_file_path"`
	SessionLogFilePath    string `json:"session_log_file_path"`
	ProcessingLogFilePath string `json:"processing_log_file_path"`
	CloudLogFilePath      string `json:"cloud_log_file_path"`
	ScraperLogFilePath    string `json:"scraper_log_file_path"`
	// StorageRoots is free space per distinct filesystem the daemon writes to.
	// Empty when the daemon has not installed a provider.
	StorageRoots []diskspace.Root `json:"storage_roots,omitempty"`
}

func (s *Service) handleInfo(req micro.Request) {
	execPath, _ := os.Executable()
	// The folder settings are editable at runtime, so they come from the live
	// config rather than the snapshot taken when this service was built.
	downloadDir := s.appConfig.Session.DownloadDir
	archiveDir := s.appConfig.Processing.ArchiveDir
	if s.configCtl != nil {
		cfg := s.configCtl.GetConfig()
		downloadDir, archiveDir = cfg.DownloadDir, cfg.ArchiveDir
	}
	var roots []diskspace.Root
	if s.diskRoots != nil {
		ctx, cancel := context.WithTimeout(context.Background(), infoDiskTimeout)
		roots = s.diskRoots(ctx)
		cancel()
	}
	respond(req, OK(DaemonInfo{
		Version:               version.Version,
		Commit:                version.Commit,
		OS:                    runtime.GOOS,
		Arch:                  runtime.GOARCH,
		Container:             runningInContainer(),
		NatsURL:               s.natsURL,
		CloudAPIURL:           cloud.APIBaseURL(),
		CloudNATSURL:          cloud.NATSEndpoint(),
		ExecutablePath:        execPath,
		ConfigFilePath:        s.appConfig.Daemon.ConfigFilePath,
		StreamersFilePath:     s.appConfig.Daemon.StreamersFilePath,
		DownloadDirectory:     downloadDir,
		ArchiveDirectory:      archiveDir,
		DaemonLogFilePath:     vllog.FilePath(s.appConfig.Daemon.Logger),
		SessionLogFilePath:    vllog.FilePath(s.appConfig.Session.Logger),
		ProcessingLogFilePath: vllog.FilePath(s.appConfig.Processing.Logger),
		CloudLogFilePath:      vllog.FilePath(s.appConfig.Cloud.Logger),
		ScraperLogFilePath:    vllog.FilePath(s.appConfig.Scraper.Logger),
		StorageRoots:          roots,
	}))
}
