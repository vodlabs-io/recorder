package vodmove

import (
	"errors"
	"fmt"
	"io/fs"

	"github.com/vodlabs-io/recorder/internal/diskspace"
)

// Reason is why a move is not succeeding, in a form a UI can branch on.
//
// The message shown for a full archive folder and an unplugged one are
// different, and so is what the user should do about each. Deciding that from
// substring matches against OS error text breaks the moment the text changes or
// the machine is not in English, so the classification happens once, here.
type Reason string

const (
	// ReasonNoSpace: the destination filesystem cannot hold the move. Recoverable
	// by freeing space; the queue keeps polling.
	ReasonNoSpace Reason = "no_space"

	// ReasonUnreachable: the destination could not be reached at all — an offline
	// NAS, an unmounted drive, a folder that has been deleted.
	ReasonUnreachable Reason = "unreachable"

	// ReasonPermission: the destination is there but refuses to be written to.
	ReasonPermission Reason = "permission"

	// ReasonSourceMissing: the local file to be moved is gone, and the
	// destination does not have it either. Terminal — unlike an unreachable
	// destination, no amount of waiting brings the file back.
	ReasonSourceMissing Reason = "source_missing"

	// ReasonOther: anything unclassified. The accompanying error string is all
	// there is to go on.
	ReasonOther Reason = "other"
)

// ErrNoSpace reports a destination that cannot hold what is being moved to it.
// It carries the numbers the user needs to act: how much more space to free.
type ErrNoSpace struct {
	Dest string

	// NeedBytes is the size of what is being moved; ShortBytes additionally
	// accounts for the headroom kept free at the destination.
	NeedBytes  uint64
	FreeBytes  uint64
	ShortBytes uint64
}

func (e *ErrNoSpace) Error() string {
	return fmt.Sprintf("%s has %s free, needs %s more",
		e.Dest, humanBytes(e.FreeBytes), humanBytes(e.ShortBytes))
}

// ErrSourceMissing reports that the local file to be moved is gone and the
// destination does not already hold it.
//
// It wraps the underlying not-exist error, so it must be tested for before any
// generic fs.ErrNotExist check — a missing source and an unreachable
// destination are the same OS error and completely different situations.
type ErrSourceMissing struct {
	Path string
	err  error
}

func (e *ErrSourceMissing) Error() string {
	return fmt.Sprintf("%s no longer exists and was never archived", e.Path)
}

// Unwrap falls back to fs.ErrNotExist when no underlying error was captured:
// this type always means the file is not there, so errors.Is must agree however
// it was constructed.
func (e *ErrSourceMissing) Unwrap() error {
	if e.err != nil {
		return e.err
	}
	return fs.ErrNotExist
}

// Terminal reports whether a Reason can never succeed on a later attempt.
//
// Everything else is worth retrying forever: a destination can come back online,
// a full disk can be emptied, a permission can be corrected. A source file that
// is gone cannot return, so retrying it is pure churn — a warning every backoff
// interval and a "Moving (retrying)" badge that never clears.
func (r Reason) Terminal() bool { return r == ReasonSourceMissing }

// Classify maps a move failure onto a Reason.
func Classify(err error) Reason {
	var noSpace *ErrNoSpace
	var missing *ErrSourceMissing
	var timeout diskspace.ErrTimeout
	switch {
	case err == nil:
		return ""
	case errors.As(err, &noSpace), diskspace.IsNoSpace(err):
		return ReasonNoSpace
	// Before the fs.ErrNotExist arm below: this wraps that same error.
	case errors.As(err, &missing):
		return ReasonSourceMissing
	case errors.Is(err, fs.ErrPermission):
		return ReasonPermission
	case errors.As(err, &timeout), errors.Is(err, fs.ErrNotExist):
		return ReasonUnreachable
	default:
		return ReasonOther
	}
}

// humanBytes formats a byte count for a message a user reads, not a log line.
func humanBytes(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := uint64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
