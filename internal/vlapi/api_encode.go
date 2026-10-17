package vlapi

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/nats-io/nats.go/micro"
	"github.com/vodlabs-io/recorder/internal/encoding"
)

// EncodeController is implemented by the daemon to drive the encode queue.
// LOCAL-ONLY: see the encode group in service.go.
type EncodeController interface {
	// EnqueueMerge queues a merge of a VOD's numbered VOD files into a single
	// vod.mp4, or vod.webm from WebM parts.
	EnqueueMerge(ctx context.Context, dir string, deleteParts bool) (encoding.Job, error)
	// EnqueueConvert queues a re-encode of a VOD's single vod.webm to an
	// H.264 vod.mp4.
	EnqueueConvert(ctx context.Context, dir string, deleteOriginal bool) (encoding.Job, error)
	// ListEncodeQueue returns every retained job plus the paused flag.
	ListEncodeQueue(ctx context.Context) (encoding.Queue, error)
	// RemoveEncodeJob deletes a non-running job row.
	RemoveEncodeJob(ctx context.Context, id int64) error
	// ReorderEncodeJob moves a queued job "up", "down" or to the "top".
	ReorderEncodeJob(ctx context.Context, id int64, direction string) error
	// CancelEncodeJob stops a running job or cancels a queued one.
	CancelEncodeJob(ctx context.Context, id int64) error
	// RetryEncodeJob re-queues a failed or cancelled job at the tail.
	RetryEncodeJob(ctx context.Context, id int64) error
	// SetEncodeQueuePaused pauses/resumes claiming of new jobs (the running
	// job is unaffected).
	SetEncodeQueuePaused(ctx context.Context, paused bool) error
	// ListEncoders reports the platform's encoder candidates with
	// availability/self-test status, plus the configured selection.
	ListEncoders(ctx context.Context, refresh bool) (EncoderList, error)
}

// EncoderList is the encoders endpoint's response: the configured selection
// ("auto", "software", or an encoder id) plus every candidate's status.
type EncoderList struct {
	Selected string                   `json:"selected"`
	Encoders []encoding.EncoderStatus `json:"encoders"`
}

// encodeEnqueueRequest's Type is "merge" (the default) or "convert";
// DeleteParts applies to a convert's original vod.webm too.
type encodeEnqueueRequest struct {
	Dir         string `json:"dir"`
	Type        string `json:"type,omitempty"`
	DeleteParts bool   `json:"delete_parts"`
}

type encodeJobRequest struct {
	ID int64 `json:"id"`
}

type encodeReorderRequest struct {
	ID        int64  `json:"id"`
	Direction string `json:"direction"`
}

type encodeEncodersRequest struct {
	Refresh bool `json:"refresh"`
}

func (s *Service) handleEncodeEnqueue(req micro.Request) {
	if s.encodeCtl == nil {
		respond(req, fail(errInternal, "encode controller unavailable"))
		return
	}
	var r encodeEnqueueRequest
	if err := json.Unmarshal(req.Data(), &r); err != nil || r.Dir == "" {
		respond(req, fail(errInvalidArg, "dir is required"))
		return
	}
	var job encoding.Job
	var err error
	switch r.Type {
	case "", "merge":
		job, err = s.encodeCtl.EnqueueMerge(context.Background(), r.Dir, r.DeleteParts)
	case "convert":
		job, err = s.encodeCtl.EnqueueConvert(context.Background(), r.Dir, r.DeleteParts)
	default:
		respond(req, fail(errInvalidArg, "unknown job type: "+r.Type))
		return
	}
	if err != nil {
		respondEncodeError(req, err, "enqueue failed")
		return
	}
	respond(req, OK(job))
}

func (s *Service) handleEncodeList(req micro.Request) {
	if s.encodeCtl == nil {
		respond(req, fail(errInternal, "encode controller unavailable"))
		return
	}
	q, err := s.encodeCtl.ListEncodeQueue(context.Background())
	if err != nil {
		respondEncodeError(req, err, "list failed")
		return
	}
	respond(req, OK(q))
}

func (s *Service) handleEncodeRemove(req micro.Request) {
	s.handleEncodeJobOp(req, func(ctx context.Context, id int64) error {
		return s.encodeCtl.RemoveEncodeJob(ctx, id)
	}, "remove failed")
}

func (s *Service) handleEncodeCancel(req micro.Request) {
	s.handleEncodeJobOp(req, func(ctx context.Context, id int64) error {
		return s.encodeCtl.CancelEncodeJob(ctx, id)
	}, "cancel failed")
}

func (s *Service) handleEncodeRetry(req micro.Request) {
	s.handleEncodeJobOp(req, func(ctx context.Context, id int64) error {
		return s.encodeCtl.RetryEncodeJob(ctx, id)
	}, "retry failed")
}

// handleEncodeJobOp factors the shared {id} request shape of
// remove/cancel/retry.
func (s *Service) handleEncodeJobOp(req micro.Request, op func(context.Context, int64) error, prefix string) {
	if s.encodeCtl == nil {
		respond(req, fail(errInternal, "encode controller unavailable"))
		return
	}
	var r encodeJobRequest
	if err := json.Unmarshal(req.Data(), &r); err != nil || r.ID <= 0 {
		respond(req, fail(errInvalidArg, "id is required"))
		return
	}
	if err := op(context.Background(), r.ID); err != nil {
		respondEncodeError(req, err, prefix)
		return
	}
	respond(req, OK("ok"))
}

func (s *Service) handleEncodeReorder(req micro.Request) {
	if s.encodeCtl == nil {
		respond(req, fail(errInternal, "encode controller unavailable"))
		return
	}
	var r encodeReorderRequest
	if err := json.Unmarshal(req.Data(), &r); err != nil || r.ID <= 0 || r.Direction == "" {
		respond(req, fail(errInvalidArg, "id and direction are required"))
		return
	}
	if err := s.encodeCtl.ReorderEncodeJob(context.Background(), r.ID, r.Direction); err != nil {
		respondEncodeError(req, err, "reorder failed")
		return
	}
	respond(req, OK("ok"))
}

func (s *Service) handleEncodePause(req micro.Request) {
	s.handleEncodePauseState(req, true)
}

func (s *Service) handleEncodeResume(req micro.Request) {
	s.handleEncodePauseState(req, false)
}

func (s *Service) handleEncodePauseState(req micro.Request, paused bool) {
	if s.encodeCtl == nil {
		respond(req, fail(errInternal, "encode controller unavailable"))
		return
	}
	if err := s.encodeCtl.SetEncodeQueuePaused(context.Background(), paused); err != nil {
		respondEncodeError(req, err, "pause state change failed")
		return
	}
	respond(req, OK("ok"))
}

func (s *Service) handleEncodeEncoders(req micro.Request) {
	if s.encodeCtl == nil {
		respond(req, fail(errInternal, "encode controller unavailable"))
		return
	}
	var r encodeEncodersRequest
	if len(req.Data()) > 0 {
		if err := json.Unmarshal(req.Data(), &r); err != nil {
			respond(req, fail(errInvalidArg, "invalid JSON"))
			return
		}
	}
	list, err := s.encodeCtl.ListEncoders(context.Background(), r.Refresh)
	if err != nil {
		respondEncodeError(req, err, "encoder detection failed")
		return
	}
	respond(req, OK(list))
}

// respondEncodeError maps encoding package sentinels (and vlapi sentinels)
// onto API error codes.
func respondEncodeError(req micro.Request, err error, prefix string) {
	code := errInternal
	switch {
	case errors.Is(err, encoding.ErrNotFound), errors.Is(err, ErrNotFound):
		code = ErrNotFound
	case errors.Is(err, encoding.ErrConflict), errors.Is(err, ErrConflict):
		code = ErrConflict
	case errors.Is(err, encoding.ErrInvalid), errors.Is(err, errInvalidArg):
		code = errInvalidArg
	}
	// The envelope already carries the code, and the client prints it, so a
	// sentinel-wrapped message would say it twice:
	// "not found: enqueue failed: not found: ...".
	msg := strings.TrimPrefix(err.Error(), code.Error()+": ")
	respond(req, fail(code, prefix+": "+msg))
}
