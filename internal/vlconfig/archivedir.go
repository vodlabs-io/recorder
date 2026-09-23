package vlconfig

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// DefaultLowSpaceWarnGB is the free space, in gibibytes, below which a Storage
// root reports as running low. Chosen to be a few recordings' worth on any disk
// a user is likely to archive to, so the warning arrives with time to act.
const DefaultLowSpaceWarnGB = 20

// IsArchiveOff reports whether raw is the explicit "disable" token ("off"),
// meaning a streamer's VODs stay in the Download folder.
func IsArchiveOff(raw string) bool {
	return strings.EqualFold(strings.TrimSpace(raw), "off")
}

// ValidateArchiveDir checks that raw is an acceptable Archive folder value.
// Empty and the "off" token are always valid (they disable archiving). A
// non-empty value must be an absolute local/network directory path;
// scheme-prefixed URIs (e.g. s3://…) are rejected.
func ValidateArchiveDir(raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" || IsArchiveOff(raw) {
		return nil
	}
	if strings.Contains(raw, "://") {
		return fmt.Errorf("archive folder %q uses an unsupported scheme; only local or network folder paths are supported", raw)
	}
	if !filepath.IsAbs(raw) {
		return fmt.Errorf("archive folder %q must be an absolute path", raw)
	}
	return nil
}

// ArchiveDirWritable reports whether dest can be created and written to. The
// folder need not exist yet (the move worker creates it), so it probes the
// nearest existing ancestor with a temp file. This catches e.g. a container
// pointed at a host path with no mounted volume behind it. dest should already
// be absolute (see ValidateArchiveDir).
func ArchiveDirWritable(dest string) error {
	p := filepath.Clean(strings.TrimSpace(dest))
	for {
		info, err := os.Stat(p)
		if err == nil {
			if !info.IsDir() {
				return fmt.Errorf("%s exists but is not a directory", p)
			}
			f, err := os.CreateTemp(p, ".vlrec-writable-*")
			if err != nil {
				return fmt.Errorf("%s is not writable: %w", p, err)
			}
			name := f.Name()
			f.Close()
			os.Remove(name)
			return nil
		}
		if !os.IsNotExist(err) {
			return fmt.Errorf("cannot access %s: %w", p, err)
		}
		parent := filepath.Dir(p)
		if parent == p {
			return fmt.Errorf("no existing parent directory for %s", dest)
		}
		p = parent
	}
}

// ValidateArchiveDirAgainstDownloadDir rejects an Archive folder that overlaps
// the Download folder in either direction (equal, or one nested inside the
// other), where a move would recurse or clobber live Recordings. Empty/"off"
// Archive folders and an empty downloadDir are accepted.
func ValidateArchiveDirAgainstDownloadDir(dest, downloadDir string) error {
	dest = strings.TrimSpace(dest)
	if dest == "" || IsArchiveOff(dest) || strings.TrimSpace(downloadDir) == "" {
		return nil
	}
	d := filepath.Clean(dest)
	a := filepath.Clean(downloadDir)
	if d == a {
		return fmt.Errorf("archive folder must differ from the download folder")
	}
	if isWithin(d, a) {
		return fmt.Errorf("archive folder %q must not be inside the download folder", dest)
	}
	if isWithin(a, d) {
		return fmt.Errorf("archive folder %q must not contain the download folder", dest)
	}
	return nil
}

// isWithin reports whether child is located inside parent (not equal). Both
// arguments must already be cleaned.
func isWithin(child, parent string) bool {
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && rel != "."
}
