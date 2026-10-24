package vlconfig

// Defaults returns the config every file and flag is loaded over. A container
// logs to the console, where `docker logs` reads it, and never auto-starts a
// daemon from `vod`: the container's entrypoint is the daemon. It also serves
// the Web UI on every interface, where the port publish decides who reaches it.
func Defaults() map[string]any {
	logMode, autoStart := "file", true
	webEnabled, webHost := false, "127.0.0.1"
	if RunningInContainer() {
		logMode, autoStart = "console", false
		webEnabled, webHost = true, "0.0.0.0"
	}
	return map[string]any{
		"logger": map[string]any{"level": "info", "mode": logMode},
		"daemon": map[string]any{
			"logger":     map[string]any{"file_name": "daemon.log"},
			"host":       "127.0.0.1",
			"port":       4222,
			"auto_start": autoStart,
			"exe_path":   "",
			"bin_repo":   "https://github.com/vodlabs-io/recorder",
		},
		"session": map[string]any{
			"logger":          map[string]any{"file_name": "session.log"},
			"variant":         "best",
			"output_template": DefaultOutputTemplate,
			"split_size":      "",
			"split_duration":  "",
			"sanitize_flv":    true,
		},
		"processing": map[string]any{
			"logger":            map[string]any{"file_name": "processing.log"},
			"delete_part_files": true,
			"archive_dir":       "",
			"low_space_warn_gb": DefaultLowSpaceWarnGB,
		},
		"cloud": map[string]any{
			"logger": map[string]any{"file_name": "cloud.log"},
		},
		"scraper": map[string]any{
			"logger": map[string]any{"file_name": "scraper.log"},
		},
		"web": map[string]any{
			"enabled": webEnabled,
			"host":    webHost,
			"port":    DefaultWebPort,
		},
	}
}
