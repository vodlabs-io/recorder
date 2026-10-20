//go:build !windows

package vlcli

import (
	"errors"
	"syscall"
)

// isConnRefused reports whether err is a dial to a port nothing listens on.
func isConnRefused(err error) bool {
	return errors.Is(err, syscall.ECONNREFUSED)
}
