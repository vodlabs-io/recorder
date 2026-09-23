package vlconfig

import (
	"fmt"
	"path/filepath"
	"strings"
)

// Scan folders are never written to, so they are not Storage roots and are
// checked only for a sane, non-overlapping path.

// NormalizeScanDirs cleans and deduplicates a scan folder list, dropping blanks
// and preserving order. It does not make a path absolute; ValidateScanDirs
// rejects a relative one. The cleaned form is what a scan reports as a VOD's
// root, so clients can match the two by exact string comparison.
func NormalizeScanDirs(dirs []string) []string {
	out := make([]string, 0, len(dirs))
	seen := make(map[string]bool, len(dirs))
	for _, d := range dirs {
		d = strings.TrimSpace(d)
		if d == "" {
			continue
		}
		d = filepath.Clean(d)
		if seen[d] {
			continue
		}
		seen[d] = true
		out = append(out, d)
	}
	return out
}

// ValidateScanDirs checks a normalized scan folder list against the folders the
// daemon writes to. An overlap would list the same VODs twice and label a
// folder being written into as read-only.
func ValidateScanDirs(dirs []string, downloadDir string, archiveDirs []string) error {
	for _, d := range dirs {
		if strings.Contains(d, "://") {
			return fmt.Errorf("scan folder %q uses an unsupported scheme; only local or network folder paths are supported", d)
		}
		if !filepath.IsAbs(d) {
			return fmt.Errorf("scan folder %q must be an absolute path", d)
		}
		if err := scanDirAgainst(d, downloadDir, "download folder"); err != nil {
			return err
		}
		for _, a := range archiveDirs {
			if err := scanDirAgainst(d, a, "archive folder"); err != nil {
				return err
			}
		}
	}
	return nil
}

// scanDirAgainst rejects a scan folder that is, contains, or sits inside one of
// the daemon's write destinations. label names that destination in the error.
func scanDirAgainst(dir, other, label string) error {
	other = strings.TrimSpace(other)
	if other == "" || IsArchiveOff(other) {
		return nil
	}
	o := filepath.Clean(other)
	switch {
	case dir == o:
		return fmt.Errorf("scan folder %q is already the %s", dir, label)
	case isWithin(dir, o):
		return fmt.Errorf("scan folder %q must not be inside the %s", dir, label)
	case isWithin(o, dir):
		return fmt.Errorf("scan folder %q must not contain the %s", dir, label)
	}
	return nil
}
