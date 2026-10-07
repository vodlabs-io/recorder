package vlapi

import "errors"

var (
	ErrNotFound   = errors.New("not found")
	errInvalidArg = errors.New("invalid argument")
	errInternal   = errors.New("internal error")
	ErrConflict   = errors.New("conflict")
	// errUnavailable means the request could not be answered right now, as
	// opposed to being answered no. It is what a client needs to tell "that
	// streamer does not exist" apart from "we could not go and look", since
	// only the second is worth retrying.
	errUnavailable = errors.New("unavailable")
	// ErrMonitorUnsupported refuses to monitor a streamer on a platform whose
	// streamers can only be recorded once. Clients offer to record it once.
	ErrMonitorUnsupported = errors.New("monitor_unsupported")
)

var errByCode = map[string]error{
	ErrNotFound.Error():           ErrNotFound,
	errInvalidArg.Error():         errInvalidArg,
	errInternal.Error():           errInternal,
	ErrConflict.Error():           ErrConflict,
	errUnavailable.Error():        errUnavailable,
	ErrMonitorUnsupported.Error(): ErrMonitorUnsupported,
}

type Err struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}
