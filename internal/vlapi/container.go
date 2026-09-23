package vlapi

import (
	"fmt"
	"os"
	"strings"

	"github.com/vodlabs-io/recorder/internal/vlconfig"
)

// containerEnvVar marks a containerized deployment, where the daemon binary
// can't be swapped in place and only mounted volumes are writable.
const containerEnvVar = "VLREC_CONTAINER"

// runningInContainer reports whether the daemon is running in a container.
func runningInContainer() bool { return os.Getenv(containerEnvVar) != "" }

// CheckArchiveDirWritable rejects an Archive folder the daemon can't write to,
// so a misconfigured path fails at config time rather than on every move.
// Empty and "off" values always pass. The error's guidance differs for a
// container, which can only write to mounted volumes.
func CheckArchiveDirWritable(dest string) error {
	dest = strings.TrimSpace(dest)
	if dest == "" || vlconfig.IsArchiveOff(dest) {
		return nil
	}
	if err := vlconfig.ArchiveDirWritable(dest); err != nil {
		if runningInContainer() {
			return fmt.Errorf(
				"archive folder %q is not writable inside the container; mount it as a "+
					"volume in docker-compose.yml and set this to the container path "+
					"(see docs/docker.md): %w", dest, err)
		}
		return fmt.Errorf(
			"archive folder %q is not writable; make sure the folder (or its parent) "+
				"exists and is writable, and that the target drive is mounted: %w", dest, err)
	}
	return nil
}
