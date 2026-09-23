package recording

import (
	"time"
)

type ProcessState string

const (
	ProcessProcessing ProcessState = "processing"
	ProcessFinished   ProcessState = "finished"
	ProcessCancelled  ProcessState = "cancelled"
	ProcessFailed     ProcessState = "failed"
	ProcessRejected   ProcessState = "rejected"
)

type SessionState string

const (
	SessionRecording  SessionState = "recording"
	SessionLagging    SessionState = "lagging"
	SessionPaused     SessionState = "paused"
	SessionFinished   SessionState = "finished"
	SessionCancelled  SessionState = "cancelled"
	SessionInvalid    SessionState = "invalid"
	SessionRestricted SessionState = "restricted"
	SessionFailed     SessionState = "failed"
)

const MinPartBytes = 102400 // 100 KiB

// Stats represents a snapshot of current recording statistics. The JSON form
// is a wire contract: GUI clients read it from the KV "session.<id>.stats"
// keys (see shared/daemon/streamer-state.ts).
type Stats struct {
	TotalBytes int64         `json:"total_bytes"`
	Duration   time.Duration `json:"duration_ns"` // nanoseconds
	Speed      float64       `json:"speed_bps"`   // bytes per second
	SessionID  string        `json:"session_id"`
}

// Part is one chunk of a Session as written to disk: a single file (FLV) or a
// directory of files (HLS), depending on the writer. Processing assembles Parts
// into VOD files.
type Part struct {
	Index     int       // Part number, starting at 1
	Bytes     int64     // Number of bytes recorded in this part
	StartTime time.Time // When recording of this part started
	EndTime   time.Time // When recording of this part ended
	Path      string    // Path to the part file or directory
}

// Output is a Session's output: the parts written so far, the one
// currently being written, and where they land.
type Output struct {
	Parts        []Part       // Parts written and closed so far
	CurrentPart  Part         // The part currently being recorded
	PartIndex    int          // Index of the current part being recorded
	BaseOutDir   string       // Base output directory for this session
	WriterName   string       // Name of the writer used for this session
	Variant      string       // Quality preference recorded ("best", "worst", "audio", …)
	Platform     string       // Name of the platform hosting the live stream
	Username     string       // Username of the streamer
	SessionID    string       // ID of the Session
	Stats        Stats        // Overall recording statistics across all parts
	ProcessState ProcessState // Current processing state
	SessionState SessionState // Current session state
}

// JobKind is the work a Job asks the processing side to do. It is set
// explicitly rather than inferred from which fields happen to be populated.
type JobKind string

const (
	// JobPart is per-part work: one recorded part is ready to process.
	JobPart JobKind = "part"
	// JobGroup concatenates a closed split group into one output file.
	JobGroup JobKind = "group"
	// JobFinalize is session-end bookkeeping — state, raw-part cleanup and the
	// logs sweep. It carries no concat work.
	JobFinalize JobKind = "finalize"
)

// Job is one unit of work a session worker hands to the processing side: a
// snapshot of the session's Output, plus what to do with it.
type Job struct {
	Output
	Kind JobKind
	// GroupInputs are the part stems ("pt1_best", "pt2_best") making up a
	// closed split group. Set only on JobGroup.
	GroupInputs []string
	// GroupOutput is the frozen output file name for a closed group
	// ("vod1.mp4", "vod2.mp4", … or "vod.mp4" for a sole group). Set only on
	// JobGroup.
	GroupOutput string
}
