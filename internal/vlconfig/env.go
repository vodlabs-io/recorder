package vlconfig

import (
	"os"
	"strings"
)

// envKeys maps environment variables to config keys. The mapping is an explicit
// allowlist rather than a naming transform because several VLREC_* variables
// (VLREC_CONFIG_DIR, VLREC_CACHE_DIR, VLREC_CONTAINER, VLREC_CLOUD_*) are
// special-purpose and must not become config keys.
//
// Environment configuration exists mainly for container deployments, where
// putting settings in the run command beats mounting a config file to change
// one value. The daemon and `vod` both apply it, so `docker exec … vod` reads
// the same settings the daemon runs with.
var envKeys = map[string]string{
	"VLREC_DAEMON_HOST":       "daemon.host",
	"VLREC_DAEMON_PORT":       "daemon.port",
	"VLREC_LOG_LEVEL":         "logger.level",
	"VLREC_LOG_MODE":          "logger.mode",
	"VLREC_DOWNLOAD_DIR":      "session.download_dir",
	"VLREC_VARIANT":           "session.variant",
	"VLREC_OUTPUT_TEMPLATE":   "session.output_template",
	"VLREC_SPLIT_SIZE":        "session.split_size",
	"VLREC_SPLIT_DURATION":    "session.split_duration",
	"VLREC_ARCHIVE_DIR":       "processing.archive_dir",
	"VLREC_ENCODER":           "processing.encoder",
	"VLREC_DELETE_PART_FILES": "processing.delete_part_files",
}

// EnvConfig collects the set environment variables as a flat config map, to
// load over the config file. Values stay strings; the unmarshaller is weakly
// typed and converts them.
func EnvConfig() map[string]any {
	out := make(map[string]any, len(envKeys))
	for env, key := range envKeys {
		if v, ok := os.LookupEnv(env); ok && strings.TrimSpace(v) != "" {
			out[key] = strings.TrimSpace(v)
		}
	}
	return out
}

// containerEnv marks a containerized deployment, where the daemon binary
// can't be swapped in place and only mounted volumes are writable.
const containerEnv = "VLREC_CONTAINER"

// RunningInContainer reports whether this process runs in a container.
func RunningInContainer() bool { return strings.TrimSpace(os.Getenv(containerEnv)) != "" }
