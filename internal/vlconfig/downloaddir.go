package vlconfig

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// DefaultOutputTemplate is the directory layout, relative to the download
// folder, used when [session] output_template is unset.
const DefaultOutputTemplate = "{platform}/{username}/{datetime}"

// ContainerDownloadDir is the download folder in a container, where the
// compose file mounts the recordings volume.
const ContainerDownloadDir = "/recordings"

// DefaultDownloadDir returns the user's Downloads folder (ContainerDownloadDir
// in a container), falling back to the current working directory if the home
// directory cannot be determined.
func DefaultDownloadDir() string {
	if RunningInContainer() {
		return ContainerDownloadDir
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, "Downloads", "recordings")
	}
	return "."
}

// EnsureStorageDir creates a configured storage folder if it is not there yet,
// so it isn't reported as an unreachable Storage root until first written to.
// Only the leaf is created: under an unmounted mount point, MkdirAll would
// create the path on the local disk and shadow the mount.
func EnsureStorageDir(path string) error {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("no folder to create")
	}
	if err := os.Mkdir(path, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	return nil
}
