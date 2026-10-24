package vlapi

import (
	"fmt"
	"strings"

	"github.com/vodlabs-io/recorder/internal/vlconfig"
)

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
		if vlconfig.RunningInContainer() {
			return fmt.Errorf(
				"archive folder %q is not writable inside the container; mount it as a "+
					"volume in compose.yaml and set this to the container path "+
					"(see docs/docker.md): %w", dest, err)
		}
		return fmt.Errorf(
			"archive folder %q is not writable; make sure the folder (or its parent) "+
				"exists and is writable, and that the target drive is mounted: %w", dest, err)
	}
	return nil
}
