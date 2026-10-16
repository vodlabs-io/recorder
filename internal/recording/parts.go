package recording

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// PartFileExt is the extension of the one file each Part of a writer is:
// ".ts" (FLV) for flvwriter, ".webm" for webmwriter. Empty for hlswriter, whose
// Parts are directories, and for an unknown writer.
func PartFileExt(writerName string) string {
	switch writerName {
	case "flvwriter":
		return ".ts"
	case "webmwriter":
		return ".webm"
	}
	return ""
}

// RemovePartFiles deletes the raw Parts left in the session's BaseOutDir after
// processing: the pt*.ts files for flvwriter, the pt*.webm files for
// webmwriter, the pt* directories for hlswriter. VOD files and the session/ffmpeg logs are left untouched. It
// returns the number of Parts removed.
//
// Callers should only invoke this once processing has finished successfully,
// since it discards the source recordings.
func (v Output) RemovePartFiles() (int, error) {
	switch v.WriterName {
	case "hlswriter", "flvwriter", "webmwriter":
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
		default:
			// File parts are the raw recordings (e.g. pt1_1080p.ts). The .mp4
			// intermediates are already removed during concatenation.
			if entry.IsDir() || !strings.HasSuffix(name, PartFileExt(v.WriterName)) {
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
