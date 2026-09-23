// Package diskspace reports free space per Storage root: the distinct
// filesystem behind one or more Download or Archive folders. Several folders
// routinely share one root, so space is measured and reported per root, never
// per folder.
//
// Sampling goes through a Sampler so callers can be tested without a real
// disk. Probe adds a timeout, since a folder on an unreachable network drive
// must yield StateUnknown rather than block.
package diskspace

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Space levels that drive behaviour. Only WarnBytes is user-configurable; the
// rest are fixed because the failure mode of a badly set floor is lost footage,
// and because they can be retuned in code without a config migration.
const (
	// StartFloorBytes is the free space a Storage root must have before a new
	// Session may start writing to it.
	StartFloorBytes = 2 << 30 // 2 GiB

	// StopFloorBytes is the free space below which Sessions already writing to a
	// root are stopped, so their Recordings finalize instead of being truncated
	// mid-write when the filesystem refuses the next block.
	//
	// The gap between this and StartFloorBytes is deliberate hysteresis: below
	// StopFloorBytes Sessions stop, above StartFloorBytes they may start again,
	// and in between nothing happens. Without the gap, space hovering at the
	// limit would churn Sessions on and off.
	StopFloorBytes = 512 << 20 // 512 MiB

	// MoveHeadroomBytes is kept free at a move destination, so a successful move
	// never leaves a volume at literally zero — a state that breaks things with
	// nothing to do with the recorder.
	MoveHeadroomBytes = 1 << 30 // 1 GiB
)

// DefaultProbeTimeout bounds a single probe: long enough for a busy but healthy
// NAS (which can take several seconds to answer a statfs), short enough that an
// unreachable one does not stall a UI request or a scan.
const DefaultProbeTimeout = 10 * time.Second

// State is a Storage root's space situation.
//
// StateUnknown is not zero free space: it means the root could not be probed at
// all — an offline NAS, an unmounted drive — which needs a different message to
// the user and a different response from the daemon than a root that is
// genuinely full.
type State string

const (
	StateOK      State = "ok"
	StateLow     State = "low"
	StateFull    State = "full"
	StateUnknown State = "unknown"
)

// Usage is what one probe learned about a path's filesystem.
type Usage struct {
	TotalBytes uint64 `json:"total_bytes"`
	FreeBytes  uint64 `json:"free_bytes"`

	// Device identifies the filesystem the path lives on: the device id on unix,
	// the volume serial on Windows. Paths with equal Device share a Storage root
	// and are reported once. It is an opaque comparison key, never displayed.
	Device string `json:"-"`
}

// Sampler probes one path's filesystem.
//
// Sample takes no context on purpose: the syscalls behind it cannot be
// cancelled, so a Sampler that accepted one would be promising something it
// cannot deliver. Use Probe for a bounded call.
type Sampler interface {
	Sample(path string) (Usage, error)
}

// System returns the real Sampler for this platform.
func System() Sampler { return sysSampler{} }

// WithTimeout runs a blocking filesystem call on its own goroutine and gives up
// on it after timeout.
//
// The call is abandoned, not cancelled: stat and statfs cannot be interrupted,
// so the goroutine stays parked until the filesystem answers, leaking one
// goroutine per hung mount until it recovers. Every filesystem touch a network
// drive could stall goes through here.
func WithTimeout[T any](ctx context.Context, path string, timeout time.Duration, fn func() (T, error)) (T, error) {
	if timeout <= 0 {
		timeout = DefaultProbeTimeout
	}
	type result struct {
		value T
		err   error
	}
	done := make(chan result, 1) // buffered: an abandoned call must not block forever
	go func() {
		v, err := fn()
		done <- result{v, err}
	}()

	var zero T
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case r := <-done:
		return r.value, r.err
	case <-timer.C:
		return zero, ErrTimeout{Path: path, After: timeout}
	case <-ctx.Done():
		return zero, ctx.Err()
	}
}

// Probe samples path's free space, giving up after timeout.
func Probe(ctx context.Context, s Sampler, path string, timeout time.Duration) (Usage, error) {
	return WithTimeout(ctx, path, timeout, func() (Usage, error) {
		return s.Sample(path)
	})
}

// Reachable reports whether path is a directory the daemon can currently see,
// giving up after timeout so an offline network drive is reported as
// unreachable instead of stalling the caller.
func Reachable(ctx context.Context, path string, timeout time.Duration) error {
	_, err := WithTimeout(ctx, path, timeout, func() (struct{}, error) {
		info, err := os.Stat(path)
		switch {
		case err != nil:
			return struct{}{}, err
		case !info.IsDir():
			return struct{}{}, fmt.Errorf("%s is not a folder", path)
		}
		return struct{}{}, nil
	})
	return err
}

// Classify maps free space onto a State. warnBytes is the user-configured
// warning level; below the start floor a root is Full, because that is the point
// at which the daemon starts refusing to record rather than merely worrying.
func Classify(u Usage, warnBytes uint64) State {
	switch {
	case u.FreeBytes < StartFloorBytes:
		return StateFull
	case warnBytes > 0 && u.FreeBytes < warnBytes:
		return StateLow
	default:
		return StateOK
	}
}

// CanStart reports whether a new Session may begin writing to a root.
func CanStart(u Usage) bool { return u.FreeBytes >= StartFloorBytes }

// MustStop reports whether Sessions already writing to a root have to stop now
// to finalize cleanly.
func MustStop(u Usage) bool { return u.FreeBytes < StopFloorBytes }

// Fits reports whether n more bytes can be written to a root while still leaving
// MoveHeadroomBytes free.
func Fits(u Usage, n uint64) bool {
	return u.FreeBytes >= n+MoveHeadroomBytes
}

// Shortfall returns how many bytes short a root is of holding n more, or 0 if it
// fits. It is what the user is told to free.
func Shortfall(u Usage, n uint64) uint64 {
	need := n + MoveHeadroomBytes
	if u.FreeBytes >= need {
		return 0
	}
	return need - u.FreeBytes
}

// Normalize cleans and deduplicates configured folder paths, dropping empty and
// relative-to-nothing entries while preserving the order given.
//
// It is the single definition of what counts as a distinct configured folder,
// so callers cannot disagree about trimming, cleaning and deduplication.
func Normalize(paths []string) []string {
	out := make([]string, 0, len(paths))
	seen := make(map[string]bool, len(paths))
	for _, p := range paths {
		p = filepath.Clean(strings.TrimSpace(p))
		if p == "" || p == "." || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}
