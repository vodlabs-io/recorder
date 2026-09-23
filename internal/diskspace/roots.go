package diskspace

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
	"time"
)

// ErrTimeout is returned when a probe did not answer within its timeout —
// almost always an unreachable network drive. It is distinct from a failed
// probe so callers can say "we cannot tell" rather than "it is broken".
type ErrTimeout struct {
	Path  string
	After time.Duration
}

func (e ErrTimeout) Error() string {
	return fmt.Sprintf("probing %s timed out after %s", e.Path, e.After)
}

// Root is one Storage root: the filesystem behind one or more configured
// folders, plus what the last probe learned about it.
type Root struct {
	// Paths are the configured folders resolving to this root, in configuration
	// order. The first is what a UI labels the root with.
	Paths []string `json:"paths"`

	// Download reports whether the Download folder is one of Paths. A root
	// carrying the Download folder is the one that loses footage when it fills,
	// so it is treated differently from an archive-only root.
	Download bool `json:"download"`

	Usage Usage `json:"usage"`
	State State `json:"state"`

	// Error is why the root could not be probed. Set only when State is
	// StateUnknown.
	Error string `json:"error,omitempty"`
}

// Prober resolves configured folders to Storage roots.
type Prober struct {
	Sampler Sampler

	// Timeout bounds each individual probe. Zero means DefaultProbeTimeout.
	Timeout time.Duration

	// WarnBytes is the user-configured level below which a root reports
	// StateLow. Zero disables the warning level entirely.
	WarnBytes uint64
}

// Group probes every configured folder and collapses those sharing a filesystem
// into one Root, so a download folder and an archive folder on the same disk are
// reported once rather than twice with identical numbers.
//
// Folders that cannot be probed cannot be grouped: two unreachable folders may
// or may not be the same volume, and merging them would report a number the user
// has no way to check. Each becomes its own StateUnknown Root.
//
// The result is deterministic: roots appear in the order their first configured
// folder was given, with the Download folder's root always first.
func (p Prober) Group(ctx context.Context, downloadDir string, archiveDirs []string) []Root {
	paths := Normalize(append([]string{downloadDir}, archiveDirs...))
	if len(paths) == 0 {
		return nil
	}
	downloads := Normalize([]string{downloadDir})
	isDownload := len(downloads) == 1 && downloads[0] == paths[0]

	var roots []Root
	byDevice := make(map[string]int, len(paths))
	for i, path := range paths {
		usage, err := Probe(ctx, p.Sampler, path, p.Timeout)
		if err != nil {
			roots = append(roots, Root{
				Paths:    []string{path},
				Download: i == 0 && isDownload,
				State:    StateUnknown,
				Error:    err.Error(),
			})
			continue
		}
		if at, ok := byDevice[usage.Device]; ok {
			roots[at].Paths = append(roots[at].Paths, path)
			continue
		}
		byDevice[usage.Device] = len(roots)
		roots = append(roots, Root{
			Paths:    []string{path},
			Download: i == 0 && isDownload,
			Usage:    usage,
			State:    Classify(usage, p.WarnBytes),
		})
	}
	return roots
}

// ProbeNearest samples the deepest existing ancestor of path.
//
// A move destination's subfolder is created on first write, so probing the exact
// path would report an unreachable root for a perfectly healthy disk the first
// time anything is archived to it. Free space is a property of the filesystem,
// and every ancestor shares it.
func ProbeNearest(ctx context.Context, s Sampler, path string, timeout time.Duration) (Usage, error) {
	p := filepath.Clean(strings.TrimSpace(path))
	if p == "" || p == "." {
		return Usage{}, fmt.Errorf("no path to probe")
	}
	for {
		usage, err := Probe(ctx, s, p, timeout)
		if err == nil {
			return usage, nil
		}
		// Only climb past a folder that simply is not there yet. Anything else —
		// a timeout, a permission error — is the answer, not a reason to look
		// somewhere else and report a different filesystem's numbers.
		if !errors.Is(err, fs.ErrNotExist) {
			return Usage{}, err
		}
		parent := filepath.Dir(p)
		if parent == p {
			return Usage{}, err
		}
		p = parent
	}
}
