package vlupdate

import (
	"fmt"
	"os"
	"runtime"
)

// Replace swaps newPath into currentPath. The previous binary is first moved
// aside to currentPath+".old" so the rename works on Windows, where the
// running .exe can be renamed but not unlinked. On POSIX the .old file is
// removed; on Windows it is left in place and cleaned up by a subsequent run.
func Replace(currentPath, newPath string) error {
	oldPath := currentPath + ".old"
	_ = os.Remove(oldPath)

	if err := os.Rename(currentPath, oldPath); err != nil {
		return fmt.Errorf("moving current binary aside: %w", err)
	}
	if err := os.Rename(newPath, currentPath); err != nil {
		// Best-effort rollback so we don't leave the caller with no binary.
		_ = os.Rename(oldPath, currentPath)
		return fmt.Errorf("installing new binary: %w", err)
	}
	if runtime.GOOS != "windows" {
		_ = os.Remove(oldPath)
	}
	return nil
}
