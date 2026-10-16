package encoding

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"time"
)

// Sentinel errors mapped onto vlapi error codes by the API layer.
var (
	ErrNotFound = errors.New("encode job not found")
	ErrConflict = errors.New("conflicting encode job state")
	ErrInvalid  = errors.New("invalid merge request")
)

// Job statuses. A job is exactly one of these; stage further qualifies a
// running job.
const (
	statusQueued    = "queued"
	statusRunning   = "running"
	statusDone      = "done"
	statusFailed    = "failed"
	statusCancelled = "cancelled"
)

// Stages of a running merge job. stageCopy is surfaced to the VOD Archive as
// "moving" (encode finished, result being copied to possibly-slow storage).
const (
	stageEncode   = "encode"
	stageCopy     = "copy"
	stageFinalize = "finalize"
)

// Job is one queue row. JSON tags are the wire contract shared with the UI
// and CLI (mirrored in shared/types.ts as EncodeJob).
type Job struct {
	ID          int64    `json:"id"`
	Type        string   `json:"type"`
	Source      string   `json:"source"`
	Dir         string   `json:"dir"`
	Platform    string   `json:"platform,omitempty"`
	Username    string   `json:"username,omitempty"`
	SessionID   string   `json:"session_id,omitempty"`
	Inputs      []string `json:"inputs"`
	Output      string   `json:"output"`
	DeleteParts bool     `json:"delete_parts"`
	Encoder     string   `json:"encoder,omitempty"`
	Position    int      `json:"position"`
	Status      string   `json:"status"`
	Stage       string   `json:"stage,omitempty"`
	Error       string   `json:"error,omitempty"`
	Warning     string   `json:"warning,omitempty"`
	LogPath     string   `json:"log_path,omitempty"`
	CreatedAt   int64    `json:"created_at"`
	StartedAt   int64    `json:"started_at,omitempty"`
	CompletedAt int64    `json:"completed_at,omitempty"`
}

// Queue is the full queue view returned to clients.
type Queue struct {
	Jobs   []Job `json:"jobs"`
	Paused bool  `json:"paused"`
}

// SessionMeta carries the session identity onto the job row (best-effort;
// used for display and manifest reconstruction).
type SessionMeta struct {
	Platform  string
	Username  string
	SessionID string
}

// mergeFileRe matches numbered VOD files only — a session that already has a
// plain vod.mp4 or vod.webm is single-group and never mergeable.
var mergeFileRe = regexp.MustCompile(`^vod(\d+)\.(mp4|webm)$`)

// EnqueueMerge snapshots the session's numbered VOD files and appends a merge job
// to the queue. Session-level validation (archive state, other services'
// busy checks) is the caller's job; this validates the folder contents.
func (s *Service) EnqueueMerge(ctx context.Context, dir string, meta SessionMeta, deleteParts bool, encoderPref string) (Job, error) {
	dir = filepath.Clean(dir)

	entries, err := os.ReadDir(dir)
	if err != nil {
		return Job{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	var inputs []string
	for _, e := range entries {
		if !e.IsDir() && mergeFileRe.MatchString(e.Name()) {
			inputs = append(inputs, e.Name())
		}
	}
	sort.Slice(inputs, func(i, j int) bool { return partNumber(inputs[i]) < partNumber(inputs[j]) })

	if len(inputs) < 2 {
		return Job{}, fmt.Errorf("%w: need at least 2 numbered parts, found %d", ErrInvalid, len(inputs))
	}
	for _, name := range []string{mergedOutputName, mergedWebMName} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			return Job{}, fmt.Errorf("%w: %s already exists in the session folder", ErrInvalid, name)
		}
	}
	output := mergeOutputName(inputs)

	// One active job per session folder.
	var active int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM encode_jobs WHERE session_dir = ? AND status IN ('queued', 'running')`,
		dir).Scan(&active); err != nil {
		return Job{}, err
	}
	if active > 0 {
		return Job{}, fmt.Errorf("%w: a merge for this session is already queued or running", ErrConflict)
	}

	inputsJSON, err := json.Marshal(inputs)
	if err != nil {
		return Job{}, err
	}

	now := time.Now().Unix()
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO encode_jobs (
			job_type, source, session_dir, platform, username, session_id,
			inputs, output, delete_parts, encoder, position, status, created_at
		)
		VALUES ('merge', 'manual', ?, ?, ?, ?, ?, ?, ?, ?,
			(SELECT COALESCE(MAX(position), 0) + 1 FROM encode_jobs), 'queued', ?)`,
		dir, meta.Platform, meta.Username, meta.SessionID,
		string(inputsJSON), output, boolToInt(deleteParts), normalizeEncoderPref(encoderPref), now)
	if err != nil {
		return Job{}, err
	}
	id, _ := res.LastInsertId()

	s.publishQueueChange(ctx)
	s.signal()
	s.vodsChanged(dir) // the session badges as "encoding" immediately

	job, err := s.getJob(ctx, id)
	if err != nil {
		return Job{}, err
	}
	return job, nil
}

// List returns every retained job: running first, queued in queue order, then
// finished history newest-first.
func (s *Service) List(ctx context.Context) (Queue, error) {
	// Read the paused flag BEFORE opening the jobs cursor: the pool is capped
	// at one connection (SetMaxOpenConns(1)), so querying while rows are open
	// would deadlock waiting for the connection the cursor holds.
	paused := s.paused(ctx)

	rows, err := s.db.QueryContext(ctx, jobColumns+`
		FROM encode_jobs
		ORDER BY
			CASE status WHEN 'running' THEN 0 WHEN 'queued' THEN 1 ELSE 2 END,
			CASE WHEN status IN ('running', 'queued') THEN position ELSE 0 END ASC,
			completed_at DESC, id DESC`)
	if err != nil {
		return Queue{}, err
	}
	defer rows.Close()

	q := Queue{Jobs: []Job{}, Paused: paused}
	for rows.Next() {
		job, err := scanJob(rows)
		if err != nil {
			return Queue{}, err
		}
		q.Jobs = append(q.Jobs, job)
	}
	return q, rows.Err()
}

// Remove deletes a job row. Running jobs must be cancelled first.
func (s *Service) Remove(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM encode_jobs WHERE id = ? AND status != 'running'`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		if _, err := s.getJob(ctx, id); err == nil {
			return fmt.Errorf("%w: cancel the running job before removing it", ErrConflict)
		}
		return ErrNotFound
	}
	s.publishQueueChange(ctx)
	return nil
}

// Reorder moves a queued job within the queue: direction is "up", "down" or
// "top". The whole queued set is renumbered transactionally.
func (s *Service) Reorder(ctx context.Context, id int64, direction string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx,
		`SELECT id FROM encode_jobs WHERE status = 'queued' ORDER BY position ASC, id ASC`)
	if err != nil {
		return err
	}
	var ids []int64
	for rows.Next() {
		var jid int64
		if err := rows.Scan(&jid); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, jid)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	idx := -1
	for i, jid := range ids {
		if jid == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		return fmt.Errorf("%w: only queued jobs can be reordered", ErrNotFound)
	}

	switch direction {
	case "up":
		if idx > 0 {
			ids[idx-1], ids[idx] = ids[idx], ids[idx-1]
		}
	case "down":
		if idx < len(ids)-1 {
			ids[idx+1], ids[idx] = ids[idx], ids[idx+1]
		}
	case "top":
		moved := ids[idx]
		ids = append(ids[:idx], ids[idx+1:]...)
		ids = append([]int64{moved}, ids...)
	default:
		return fmt.Errorf("%w: unknown direction %q", ErrInvalid, direction)
	}

	for pos, jid := range ids {
		if _, err := tx.ExecContext(ctx,
			`UPDATE encode_jobs SET position = ? WHERE id = ?`, pos+1, jid); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.publishQueueChange(ctx)
	return nil
}

// Cancel stops a job: a running job's ffmpeg/copy is killed, a queued job is
// marked cancelled directly.
func (s *Service) Cancel(ctx context.Context, id int64) error {
	s.mu.Lock()
	if r := s.running; r != nil && r.id == id {
		r.userCancelled.Store(true)
		r.cancel()
		s.mu.Unlock()
		return nil
	}
	s.mu.Unlock()

	res, err := s.db.ExecContext(ctx,
		`UPDATE encode_jobs SET status = 'cancelled', completed_at = ? WHERE id = ? AND status = 'queued'`,
		time.Now().Unix(), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		if _, err := s.getJob(ctx, id); err == nil {
			return fmt.Errorf("%w: job is not queued or running", ErrConflict)
		}
		return ErrNotFound
	}
	if job, err := s.getJob(ctx, id); err == nil {
		s.vodsChanged(job.Dir) // clears the "encoding" badge
	}
	s.publishQueueChange(ctx)
	s.pruneTerminal(ctx)
	return nil
}

// Retry re-queues a failed or cancelled job at the tail, clearing its error
// state and re-snapshotting the encoder preference.
func (s *Service) Retry(ctx context.Context, id int64, encoderPref string) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE encode_jobs SET
			status = 'queued', stage = '', error = NULL, warning = NULL,
			started_at = NULL, completed_at = NULL, encoder = ?,
			position = (SELECT COALESCE(MAX(position), 0) + 1 FROM encode_jobs)
		WHERE id = ? AND status IN ('failed', 'cancelled')`,
		normalizeEncoderPref(encoderPref), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		if _, err := s.getJob(ctx, id); err == nil {
			return fmt.Errorf("%w: only failed or cancelled jobs can be retried", ErrConflict)
		}
		return ErrNotFound
	}
	if job, err := s.getJob(ctx, id); err == nil {
		s.vodsChanged(job.Dir)
	}
	s.publishQueueChange(ctx)
	s.signal()
	return nil
}

// SetPaused persists the queue's paused flag. Pausing does not interrupt the
// running job; it stops the worker from claiming the next one.
func (s *Service) SetPaused(ctx context.Context, paused bool) error {
	v := "0"
	if paused {
		v = "1"
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO encode_meta (key, value) VALUES ('paused', ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`, v); err != nil {
		return err
	}
	s.publishQueueChange(ctx)
	if !paused {
		s.signal()
	}
	return nil
}

func (s *Service) paused(ctx context.Context) bool {
	var v string
	err := s.db.QueryRowContext(ctx,
		`SELECT value FROM encode_meta WHERE key = 'paused'`).Scan(&v)
	return err == nil && v == "1"
}

// jobColumns is the shared SELECT list; keep in sync with scanJob.
const jobColumns = `
	SELECT id, job_type, source, session_dir, platform, username, session_id,
		inputs, output, delete_parts, encoder, position, status, stage,
		error, warning, log_path, created_at, started_at, completed_at`

func scanJob(row interface{ Scan(...any) error }) (Job, error) {
	var j Job
	var platform, username, sessionID, stage, errMsg, warning, logPath sql.NullString
	var inputsJSON string
	var deleteParts int
	var startedAt, completedAt sql.NullInt64
	if err := row.Scan(
		&j.ID, &j.Type, &j.Source, &j.Dir, &platform, &username, &sessionID,
		&inputsJSON, &j.Output, &deleteParts, &j.Encoder, &j.Position, &j.Status, &stage,
		&errMsg, &warning, &logPath, &j.CreatedAt, &startedAt, &completedAt,
	); err != nil {
		return Job{}, err
	}
	j.Platform = platform.String
	j.Username = username.String
	j.SessionID = sessionID.String
	j.Stage = stage.String
	j.Error = errMsg.String
	j.Warning = warning.String
	j.LogPath = logPath.String
	j.DeleteParts = deleteParts != 0
	j.StartedAt = startedAt.Int64
	j.CompletedAt = completedAt.Int64
	if err := json.Unmarshal([]byte(inputsJSON), &j.Inputs); err != nil {
		return Job{}, fmt.Errorf("decode job %d inputs: %w", j.ID, err)
	}
	return j, nil
}

func (s *Service) getJob(ctx context.Context, id int64) (Job, error) {
	row := s.db.QueryRowContext(ctx, jobColumns+` FROM encode_jobs WHERE id = ?`, id)
	job, err := scanJob(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Job{}, ErrNotFound
	}
	return job, err
}

// claimNext atomically picks the front queued job and marks it running.
// Returns nil when the queue is empty.
func (s *Service) claimNext(ctx context.Context) (*Job, error) {
	row := s.db.QueryRowContext(ctx, jobColumns+`
		FROM encode_jobs WHERE status = 'queued' ORDER BY position ASC, id ASC LIMIT 1`)
	job, err := scanJob(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE encode_jobs SET status = 'running', stage = ?, started_at = ? WHERE id = ?`,
		stageEncode, time.Now().Unix(), job.ID); err != nil {
		return nil, err
	}
	job.Status = statusRunning
	job.Stage = stageEncode
	s.publishQueueChange(ctx)
	return &job, nil
}

// setStage records a running job's stage transition (encode → copy →
// finalize) so the queue list and archive overlay reflect it.
func (s *Service) setStage(ctx context.Context, id int64, stage string) {
	if _, err := s.db.ExecContext(ctx,
		`UPDATE encode_jobs SET stage = ? WHERE id = ?`, stage, id); err != nil {
		s.logger.Warn("Failed to update job stage", "job_id", id, "stage", stage, "error", err)
	}
	s.publishQueueChange(ctx)
}

// finishJob records a job's terminal state.
func (s *Service) finishJob(ctx context.Context, id int64, status, errMsg, warning, logPath string) {
	if _, err := s.db.ExecContext(ctx, `
		UPDATE encode_jobs SET status = ?, stage = '', error = ?, warning = ?,
			log_path = ?, completed_at = ? WHERE id = ?`,
		status, nullIfEmpty(errMsg), nullIfEmpty(warning), nullIfEmpty(logPath),
		time.Now().Unix(), id); err != nil {
		s.logger.Error("Failed to record job result", "job_id", id, "status", status, "error", err)
	}
	s.publishQueueChange(ctx)
	s.pruneTerminal(ctx)
}

// requeueJob puts an interrupted (shutdown) job back to queued so the next
// daemon start resumes it.
func (s *Service) requeueJob(ctx context.Context, id int64) {
	if _, err := s.db.ExecContext(ctx,
		`UPDATE encode_jobs SET status = 'queued', stage = '' WHERE id = ?`, id); err != nil {
		s.logger.Error("Failed to requeue interrupted job", "job_id", id, "error", err)
	}
	s.publishQueueChange(ctx)
}

// publishQueueChange announces that the queue's contents moved: the revision
// marker clients re-fetch on, plus the running/queued count the sidebar badge
// reads directly.
func (s *Service) publishQueueChange(ctx context.Context) {
	if s.store == nil {
		return
	}
	s.store.PutEncodeQueueRev(ctx)
	s.store.PutEncodePending(ctx, s.pendingCount(ctx))
}

// pendingCount is the number of jobs a user would call outstanding: the one
// running plus everything still waiting. Returns 0 when the count can't be
// read, which reads as "nothing outstanding" — the next queue change
// republishes it.
func (s *Service) pendingCount(ctx context.Context) int {
	var n int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM encode_jobs WHERE status IN ('queued', 'running')`).Scan(&n); err != nil {
		s.logger.Warn("Failed to count pending encode jobs", "error", err)
		return 0
	}
	return n
}

func partNumber(name string) int {
	m := mergeFileRe.FindStringSubmatch(name)
	if m == nil {
		return 0
	}
	n, _ := strconv.Atoi(m[1])
	return n
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func nullIfEmpty(v string) any {
	if v == "" {
		return nil
	}
	return v
}

// normalizeEncoderPref maps config spellings onto job-row values: "auto" and
// "" both mean automatic selection.
func normalizeEncoderPref(pref string) string {
	if pref == "auto" {
		return ""
	}
	return pref
}
