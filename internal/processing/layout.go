package processing

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Recorders up to 0.0.20 kept every sidecar flat in the session folder,
// named after its video (vod1.jpg, vod1.thumbs.vtt, vod1.thumbs.000.jpg, …,
// contact-sheet.json). Readers fall back to those names, for folders that
// are scanned but never written to; MigrateLayout moves the rest.

// legacyName is the flat name a sidecar had before previews/, or "" for one
// that had none.
func legacyName(name string) string {
	parts := strings.Split(filepath.ToSlash(name), "/")
	if parts[0] != PreviewsDir {
		return ""
	}
	switch len(parts) {
	case 2:
		if parts[1] == sessionSheetKeyBase {
			return parts[1]
		}
	case 3:
		if parts[2] == "thumbnail.jpg" {
			return parts[1] + ".jpg"
		}
		return parts[1] + "." + parts[2]
	}
	return ""
}

// ReadSidecar reads the sidecar name of the session folder dir, at its flat
// name when an older recorder left it there.
func ReadSidecar(dir, name string) ([]byte, error) {
	b, err := os.ReadFile(filepath.Join(dir, name))
	if errors.Is(err, fs.ErrNotExist) {
		if old := legacyName(name); old != "" {
			if b, oldErr := os.ReadFile(filepath.Join(dir, old)); oldErr == nil {
				return b, nil
			}
		}
	}
	return b, err
}

// OpenSidecar opens the sidecar name of the session folder dir, at its flat
// name when an older recorder left it there. Neither may resolve outside dir.
func OpenSidecar(dir, name string) (*os.File, error) {
	f, err := os.OpenInRoot(dir, name)
	if errors.Is(err, fs.ErrNotExist) {
		if old := legacyName(name); old != "" {
			if f, oldErr := os.OpenInRoot(dir, old); oldErr == nil {
				return f, nil
			}
		}
	}
	return f, err
}

// SidecarPath is where the sidecar name of the session folder dir is: its
// flat path when only an older recorder's copy exists.
func SidecarPath(dir, name string) string {
	p := filepath.Join(dir, name)
	if _, err := os.Stat(p); errors.Is(err, fs.ErrNotExist) {
		if old := legacyName(name); old != "" {
			if _, err := os.Stat(filepath.Join(dir, old)); err == nil {
				return filepath.Join(dir, old)
			}
		}
	}
	return p
}

// legacySheetRef matches a flat track's cue images.
var legacySheetRef = regexp.MustCompile(`(?m)^[^\s#/]+\.thumbs\.(\d{3}\.jpg#xywh=)`)

// MigrateLayout moves the flat sidecars of vodFiles in the session folder
// dir under previews/, rewriting each timeline-preview track's cues to the
// sheets' new names. A sidecar already at its new name wins over the flat
// copy, which is removed. It returns how many sidecars it moved.
func MigrateLayout(dir string, vodFiles []string) (int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, err
	}
	moved := 0
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		name := migratedName(e.Name(), vodFiles)
		if name == "" {
			continue
		}
		from, to := filepath.Join(dir, e.Name()), filepath.Join(dir, name)
		if _, err := os.Stat(to); err == nil {
			os.Remove(from)
			continue
		}
		if strings.HasSuffix(name, ".vtt") {
			b, err := os.ReadFile(from)
			if err != nil {
				return moved, err
			}
			if err := WriteFileAtomic(to, legacySheetRef.ReplaceAll(b, []byte(storyboardSheetPrefix+"$1"))); err != nil {
				return moved, err
			}
			if err := os.Remove(from); err != nil {
				return moved, err
			}
		} else {
			if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
				return moved, err
			}
			if err := os.Rename(from, to); err != nil {
				return moved, err
			}
		}
		moved++
	}
	return moved, nil
}

// migratedName is the name the flat sidecar old moves to, or "" when old is
// not a sidecar of vodFiles.
func migratedName(old string, vodFiles []string) string {
	if old == sessionSheetKeyBase {
		return sessionSheetKeyName
	}
	for _, f := range vodFiles {
		s := stem(f)
		if s == f || !strings.HasPrefix(old, s+".") {
			continue
		}
		leaf := old[len(s)+1:]
		switch {
		case leaf == "jpg":
			return ThumbnailName(f)
		case leaf == "thumbs.vtt", leaf == "scrub.jpg", leaf == "previews.json", sheetLeaf.MatchString(leaf):
			return filepath.Join(PartPreviewsDir(f), leaf)
		}
	}
	return ""
}

var sheetLeaf = regexp.MustCompile(`^thumbs\.\d{3}\.jpg$`)
