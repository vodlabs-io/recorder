package vlstate

import (
	"context"
	"encoding/json"
	"strconv"
	"time"
)

// Encode queue KV keys. Deliberately a top-level prefix OUTSIDE
// streamer.>/daemon.>: the cloud bridge mirrors only those two prefixes
// (internal/cloud/bridge.go), so encode state stays daemon-local by
// construction. Local UI clients subscribe to encode.> explicitly
// (shared/daemon/session.ts DEFAULT_KV_FILTERS).
const (
	encodeQueueRevKey = "encode.queue_rev"
	encodePendingKey  = "encode.pending"
	encodeProgressKey = "encode.progress"
)

// EncodeProgress is the live progress document for the running encode job.
// Mirrored in shared/types.ts.
type EncodeProgress struct {
	JobID int64 `json:"job_id"`
	// Stage is "encode" or "copy".
	Stage string `json:"stage"`
	// Percent is 0-100, or -1 when indeterminate (unknown total duration).
	Percent float64 `json:"percent"`
	// EtaSec is the estimated seconds remaining (encode stage only, 0 when
	// unknown).
	EtaSec int64 `json:"eta_sec,omitempty"`
	// Speed is ffmpeg's realtime multiplier (encode stage only).
	Speed float64 `json:"speed,omitempty"`
}

// PutEncodeQueueRev bumps the queue revision key; clients re-fetch the queue
// listing when it moves.
func (s *Store) PutEncodeQueueRev(ctx context.Context) {
	if s.kv == nil {
		return
	}
	_, _ = s.kv.PutString(ctx, encodeQueueRevKey, strconv.FormatInt(time.Now().UnixNano(), 10))
}

// PutEncodePending publishes how many jobs are running or waiting, for the
// sidebar's badge. Separate from the queue revision because clients read it
// directly rather than using it as a signal to re-fetch.
func (s *Store) PutEncodePending(ctx context.Context, n int) {
	if s.kv == nil {
		return
	}
	_, _ = s.kv.PutString(ctx, encodePendingKey, strconv.Itoa(n))
}

// PutEncodeProgress publishes the running job's progress (throttled by the
// caller).
func (s *Store) PutEncodeProgress(ctx context.Context, p EncodeProgress) {
	if s.kv == nil {
		return
	}
	data, err := json.Marshal(p)
	if err != nil {
		return
	}
	_, _ = s.kv.Put(ctx, encodeProgressKey, data)
}

// DelEncodeProgress clears the progress key once no job is running.
func (s *Store) DelEncodeProgress(ctx context.Context) {
	if s.kv == nil {
		return
	}
	_ = s.kv.Delete(ctx, encodeProgressKey)
}
