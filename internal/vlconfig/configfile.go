package vlconfig

import (
	"os"
	"strings"
)

// Environment variables naming the two user config files, so a separately
// started daemon and CLI (e.g. `docker exec … vod` in a container) read the
// same files. Unlike VLREC_CONFIG_DIR they move nothing else.
const (
	configFileEnv    = "VLREC_CONFIG_FILE"
	streamersFileEnv = "VLREC_STREAMERS_FILE"
)

// DefaultConfigFilePath is the config.toml both binaries read unless a flag
// says otherwise.
func DefaultConfigFilePath() string {
	return fileFromEnvOr(configFileEnv, "config.toml")
}

// DefaultStreamersFilePath is the streamers.toml both binaries read unless a
// flag says otherwise.
func DefaultStreamersFilePath() string {
	return fileFromEnvOr(streamersFileEnv, "streamers.toml")
}

func fileFromEnvOr(env, fileName string) string {
	if p := strings.TrimSpace(os.Getenv(env)); p != "" {
		return p
	}
	return DefaultFilePath(fileName)
}
