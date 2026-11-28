package vlcli

import (
	"os"
	"path/filepath"
	"strings"
)

// managedBy reports which package manager installed exePath and how to update
// vod with it, or empty strings when vod updates itself. Homebrew links
// bin/vod into its Cellar, so the path is resolved first.
func managedBy(exePath string) (name, update string) {
	if real, err := filepath.EvalSymlinks(exePath); err == nil {
		exePath = real
	}
	p := filepath.ToSlash(exePath)
	// Windows paths use backslashes whatever the host OS.
	p = strings.ReplaceAll(p, `\`, "/")
	lower := strings.ToLower(p)
	switch {
	case strings.Contains(p, "/Cellar/vod/"):
		return "Homebrew", "brew upgrade vod"
	case strings.Contains(lower, "/scoop/apps/vod/"):
		return "Scoop", "scoop update vod"
	case strings.Contains(lower, "/winget/packages/vodlabs.vod_"):
		return "winget", "winget upgrade VodLabs.vod"
	case strings.Contains(lower, "/chocolatey/lib/vod/"):
		return "Chocolatey", "choco upgrade vod"
	case strings.HasPrefix(p, "/usr/bin/"):
		return "your system's package manager", "your system's package manager"
	}
	return "", ""
}

// dirWritable reports whether a file can be created in dir, which replacing
// vod in place needs.
func dirWritable(dir string) bool {
	f, err := os.CreateTemp(dir, ".vod-write-check-*")
	if err != nil {
		return false
	}
	name := f.Name()
	f.Close()
	os.Remove(name)
	return true
}
