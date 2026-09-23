//go:build !windows

package diskspace

import (
	"errors"
	"syscall"
)

// IsNoSpace reports whether err is the filesystem refusing a write for lack of
// space. There is no portable sentinel for this, so it is resolved per platform.
func IsNoSpace(err error) bool {
	return errors.Is(err, syscall.ENOSPC) || errors.Is(err, syscall.EDQUOT)
}
