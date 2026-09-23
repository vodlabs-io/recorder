// Package vodmove moves finished VOD files into an Archive folder such as a
// network drive or second disk, freeing local disk space. It is transport-only:
// callers (the processing service) own the queue, retries and bookkeeping; this
// package just durably places one local file at a destination and verifies the
// copy before the caller deletes the source.
//
// The destination is an opaque string; only local or network directory paths
// are supported (see ForDest).
package vodmove

import (
	"context"
	"fmt"
	"strings"
)

// Store durably places a finished local file under relPath at a destination.
//
// Store is idempotent and safe to retry: if the destination already holds an
// identical-size copy it returns nil without re-copying, and a destination file
// of a different size (a stale partial from an interrupted attempt) is
// overwritten. On success the file is durably at the destination; the local
// source may or may not still exist (a same-volume rename consumes it), so the
// caller deletes the local file afterwards and tolerates "not exist". On any
// failure the local source is left untouched.
//
// relPath is always slash-separated; implementations translate it to the
// destination's native separator.
type Store interface {
	Store(ctx context.Context, localPath, relPath string) error
}

// ForDest returns the Store for a destination string. Only local/network
// directory paths are supported, so any value containing a "://" scheme is
// rejected.
func ForDest(dest string) (Store, error) {
	dest = strings.TrimSpace(dest)
	if dest == "" {
		return nil, fmt.Errorf("empty archive folder")
	}
	if strings.Contains(dest, "://") {
		return nil, fmt.Errorf("archive folder %q uses an unsupported scheme; only local or network folder paths are supported", dest)
	}
	return &localDir{Root: dest}, nil
}
