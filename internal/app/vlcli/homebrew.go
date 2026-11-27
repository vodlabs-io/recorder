package vlcli

import (
	"path/filepath"
	"strings"
)

// installedByHomebrew reports whether exePath is a vod that Homebrew
// installed, which Homebrew updates itself. Homebrew links bin/vod to the
// Cellar, so the path is resolved first.
func installedByHomebrew(exePath string) bool {
	if real, err := filepath.EvalSymlinks(exePath); err == nil {
		exePath = real
	}
	return strings.Contains(filepath.ToSlash(exePath), "/Cellar/vod/")
}
