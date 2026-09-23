//go:build windows

package diskspace

import (
	"errors"

	"golang.org/x/sys/windows"
)

// IsNoSpace reports whether err is the filesystem refusing a write for lack of
// space. There is no portable sentinel for this, so it is resolved per platform.
func IsNoSpace(err error) bool {
	return errors.Is(err, windows.ERROR_DISK_FULL) ||
		errors.Is(err, windows.ERROR_HANDLE_DISK_FULL)
}
