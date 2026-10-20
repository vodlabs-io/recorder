//go:build windows

package vlcli

import (
	"errors"

	"golang.org/x/sys/windows"
)

// isConnRefused reports whether err is a dial to a port nothing listens on.
func isConnRefused(err error) bool {
	return errors.Is(err, windows.WSAECONNREFUSED)
}
