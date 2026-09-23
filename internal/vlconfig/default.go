package vlconfig

var Default = map[string]any{
	"logger": map[string]any{"level": "info", "mode": "file"},
	"daemon": map[string]any{
		"logger":     map[string]any{"file_name": "daemon.log"},
		"host":       "127.0.0.1",
		"port":       4222,
		"auto_start": true,
		"exe_path":   "",
		"bin_repo":   "https://github.com/vodlabs-io/assets",
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
}
