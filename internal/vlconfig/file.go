package vlconfig

import (
	_ "embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

//go:embed default_config.toml
var defaultConfigToml []byte

//go:embed default_streamers.toml
var defaultStreamersToml []byte

//go:embed default_platforms.json
var defaultPlatformsJSON []byte

func EnsureConfigFileExists(filePath string) error {
	return ensureFileExists(filePath, defaultConfigToml)
}

func EnsureStreamersFileExists(filePath string) error {
	return ensureFileExists(filePath, defaultStreamersToml)
}

// EnsurePlatformsCacheExists extracts the embedded default supported-platforms
// list to the cache directory if no cache file is present yet. The list comes
// from the cloud via the heartbeat, so it is cache, not user config.
func EnsurePlatformsCacheExists() error {
	return ensureFileExists(DefaultCacheFilePath("platforms.json"), defaultPlatformsJSON)
}

func ensureFileExists(filePath string, content []byte) error {
	if _, err := os.Stat(filePath); errors.Is(err, fs.ErrNotExist) {
		if err := os.MkdirAll(filepath.Dir(filePath), 0755); err != nil {
			return fmt.Errorf("failed to create directory for file %s: %w", filePath, err)
		}
		if err := os.WriteFile(filePath, content, 0644); err != nil {
			return fmt.Errorf("failed to write file %s: %w", filePath, err)
		}
		fmt.Println("Created default config at ", filePath)
	}
	return nil
}

// writeFileAtomic replaces path with data through a synced temp file in the
// same directory, so a crash mid-write leaves either the old or the new
// contents, never a truncated or zero-filled file.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Chmod(tmpName, perm); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}

// Environment variables that relocate the daemon's config and cache
// directories. Each names the final directory ("vodlabs/recorder" is not
// appended). Unlike the XDG variables, which os.UserConfigDir and
// os.UserCacheDir ignore on macOS, they work on every platform.
const (
	configDirEnv = "VLREC_CONFIG_DIR"
	CacheDirEnv  = "VLREC_CACHE_DIR"
)

// ConfigDirOverridden reports whether VLREC_CONFIG_DIR redirects daemon state.
// Callers that migrate a legacy file into the config dir use it to skip that
// migration when the profile is redirected: a scratch or side-by-side daemon
// must start empty rather than adopt the real user's state.
func ConfigDirOverridden() bool {
	return strings.TrimSpace(os.Getenv(configDirEnv)) != ""
}

func DefaultFilePath(fileName string) string {
	return defaultPathIn(configDirEnv, os.UserConfigDir, fileName)
}

// DefaultCacheFilePath returns the path for a regenerable cache file under the
// user cache dir (XDG_CACHE_HOME on Linux, /cache in docker). Used for
// cloud-supplied state such as the supported-platforms list.
func DefaultCacheFilePath(fileName string) string {
	return defaultPathIn(CacheDirEnv, os.UserCacheDir, fileName)
}

// defaultPathIn resolves fileName against the override directory named by env,
// falling back to the per-user directory that userDir reports.
func defaultPathIn(env string, userDir func() (string, error), fileName string) string {
	if dir := strings.TrimSpace(os.Getenv(env)); dir != "" {
		return filepath.Join(dir, fileName)
	}
	if base, err := userDir(); err == nil && base != "" {
		return filepath.Join(base, "vodlabs", "recorder", fileName)
	}
	return fileName // Fallback to current working directory
}
