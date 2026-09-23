package recording

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// RemovePartFiles deletes the raw Parts left in the session's BaseOutDir after
// processing: the pt*.ts files for flvwriter, the pt* directories for
// hlswriter. VOD files and the session/ffmpeg logs are left untouched. It
// returns the number of Parts removed.
//
// Callers should only invoke this once processing has finished successfully,
// since it discards the source recordings.
func (v Output) RemovePartFiles() (int, error) {
	switch v.WriterName {
	case "hlswriter", "flvwriter":
	default:
		return 0, fmt.Errorf("unsupported writer: %s", v.WriterName)
	}

	entries, err := os.ReadDir(v.BaseOutDir)
	if err != nil {
		return 0, fmt.Errorf("failed to read base directory: %w", err)
	}

	var removed int
	var errs []error
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, "pt") {
			continue
		}
		path := filepath.Join(v.BaseOutDir, name)

		switch v.WriterName {
		case "hlswriter":
			// HLS parts are subdirectories (e.g. pt1_1080p/).
			if !entry.IsDir() {
				continue
			}
			if err := os.RemoveAll(path); err != nil {
				errs = append(errs, fmt.Errorf("remove part dir %s: %w", name, err))
				continue
			}
		case "flvwriter":
			// FLV parts are the raw .ts files (e.g. pt1_1080p.ts). The .mp4
			// intermediates are already removed during concatenation.
			if entry.IsDir() || !strings.HasSuffix(name, ".ts") {
				continue
			}
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				errs = append(errs, fmt.Errorf("remove part file %s: %w", name, err))
				continue
			}
		}
		removed++
	}

	return removed, errors.Join(errs...)
}
