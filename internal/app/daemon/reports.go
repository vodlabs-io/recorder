package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/knadh/koanf/parsers/toml"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/v2"
	"github.com/vodlabs-io/recorder/internal/errreport"
	"github.com/vodlabs-io/recorder/internal/version"
	"github.com/vodlabs-io/recorder/internal/vlapi"
	"github.com/vodlabs-io/recorder/internal/vlconfig"
	"github.com/vodlabs-io/recorder/internal/vllog"
)

// startErrorReports creates the Sentry client and routes every logger's
// error records through it. It runs before any logger is created.
func (a *App) startErrorReports() {
	a.flushReports = errreport.Init(errreport.Options{
		Component: "daemon",
		Launcher:  os.Getenv("VLREC_LAUNCHER"),
		Enabled:   a.appCfg.ErrorReports.Enabled,
	})
	vllog.WrapHandlers(errreport.Handler)
}

// watchCrashes reports the previous run's crash, if it left one, and keeps
// this run's in the logs folder.
func (a *App) watchCrashes() {
	if err := errreport.WatchCrashes(a.logDir()); err != nil {
		a.log.Warn("Could not set up the crash log", "error", err)
	}
}

// logDir is the daemon's log folder.
func (a *App) logDir() string {
	return filepath.Dir(vllog.FilePath(a.appCfg.Daemon.Logger))
}

// ErrorReports returns the user's answer to sending error reports.
// Implements vlapi's reportController.
func (a *App) ErrorReports() vlapi.ErrorReportsState {
	a.cfgMu.RLock()
	defer a.cfgMu.RUnlock()
	return vlapi.ErrorReportsState{
		Enabled:   a.appCfg.ErrorReports.Enabled,
		Available: errreport.Available(),
	}
}

// SetErrorReports records the user's answer in config.toml and applies it
// straight away. Implements vlapi's reportController.
func (a *App) SetErrorReports(on bool) error {
	a.cfgMu.RLock()
	path := a.appCfg.Daemon.ConfigFilePath
	a.cfgMu.RUnlock()
	a.cfgFileMu.Lock()
	err := writeErrorReports(path, on)
	a.cfgFileMu.Unlock()
	if err != nil {
		return fmt.Errorf("failed to persist config: %w", err)
	}
	a.cfgMu.Lock()
	a.appCfg.ErrorReports.Enabled = &on
	a.cfgMu.Unlock()
	errreport.SetEnabled(on)
	a.log.Info("Error reports setting changed", "enabled", on)
	return nil
}

// writeErrorReports sets [error_reports] enabled in config.toml, keeping the
// file's other keys, as writeUserConfig does.
func writeErrorReports(path string, on bool) error {
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
	k.Set("error_reports.enabled", on)
	b, err := toml.Parser().Marshal(k.Raw())
	if err != nil {
		return fmt.Errorf("failed to marshal config: %w", err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		return fmt.Errorf("failed to write config file %s: %w", path, err)
	}
	return nil
}

// SendBugReport sends the user's bug report, with the recorder's recent logs
// if they asked. Implements vlapi's reportController.
func (a *App) SendBugReport(r vlapi.BugReportRequest) (string, error) {
	extra := map[string]string{
		"daemon_version": version.Version,
		"os":             runtime.GOOS,
		"arch":           runtime.GOARCH,
	}
	if version.Commit != "" {
		extra["daemon_commit"] = version.Commit
	}
	if r.AppVersion != "" {
		extra["app_version"] = r.AppVersion
	}
	if vlconfig.RunningInContainer() {
		extra["container"] = "true"
	}
	var clientID string
	if a.cloudClient != nil {
		clientID = a.cloudClient.ClientID()
	}
	report := errreport.BugReport{
		Message:   r.Message,
		Component: r.SentFrom,
		ClientID:  clientID,
		Extra:     extra,
	}
	if r.IncludeLogs {
		report.Logs = errreport.LogTails(a.logDirs()...)
	}
	id, err := errreport.SendBugReport(report)
	if err != nil {
		a.log.Warn("Could not send a bug report", "error", err)
		return "", err
	}
	a.log.Info("Sent a bug report", "id", id, "logs", len(report.Logs))
	return id, nil
}

// logDirs are the folders holding the recorder's logs: each component's, and
// the default one, where the desktop app writes its own.
func (a *App) logDirs() []string {
	a.cfgMu.RLock()
	cfg := a.appCfg
	a.cfgMu.RUnlock()
	dirs := []string{vlconfig.DefaultCacheFilePath("logs")}
	for _, l := range []vlconfig.LoggerConfig{
		cfg.Daemon.Logger, cfg.Session.Logger, cfg.Processing.Logger,
		cfg.Cloud.Logger, cfg.Scraper.Logger,
	} {
		dirs = append(dirs, filepath.Dir(vllog.FilePath(l)))
	}
	return dirs
}
