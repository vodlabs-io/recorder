package encoding

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

func TestEnqueueMergeValidation(t *testing.T) {
	s := newTestService(t)
	ctx := context.Background()

	t.Run("too few parts", func(t *testing.T) {
		dir := makeSession(t, "vod1.mp4")
		_, err := s.EnqueueMerge(ctx, dir, SessionMeta{}, true, "auto")
		if !errors.Is(err, ErrInvalid) {
			t.Fatalf("want ErrInvalid, got %v", err)
		}
	})

	t.Run("existing merged output", func(t *testing.T) {
		dir := makeSession(t, "vod1.mp4", "vod2.mp4", "vod.mp4")
		_, err := s.EnqueueMerge(ctx, dir, SessionMeta{}, true, "auto")
		if !errors.Is(err, ErrInvalid) {
			t.Fatalf("want ErrInvalid for pre-existing vod.mp4, got %v", err)
		}
	})

	t.Run("valid enqueue and duplicate conflict", func(t *testing.T) {
		dir := makeSession(t, "vod1.mp4", "vod2.mp4", "vod3.mp4")
		job, err := s.EnqueueMerge(ctx, dir, SessionMeta{Platform: "tiktok", Username: "bob"}, false, "auto")
		if err != nil {
			t.Fatalf("enqueue: %v", err)
		}
		if job.Status != statusQueued {
			t.Errorf("status = %q, want queued", job.Status)
		}
		if job.Output != mergedOutputName {
			t.Errorf("output = %q, want %q", job.Output, mergedOutputName)
		}
		if len(job.Inputs) != 3 {
			t.Errorf("inputs = %v, want 3 parts", job.Inputs)
		}
		// Parts must be sorted by number.
		if job.Inputs[0] != "vod1.mp4" || job.Inputs[2] != "vod3.mp4" {
			t.Errorf("inputs not sorted: %v", job.Inputs)
		}
		if job.DeleteParts {
			t.Errorf("delete_parts = true, want false")
		}
		// "auto" is normalized to "" on the row.
		if job.Encoder != "" {
			t.Errorf("encoder = %q, want empty (auto)", job.Encoder)
		}
		if job.Platform != "tiktok" || job.Username != "bob" {
			t.Errorf("identity not persisted: %+v", job)
		}

		// A second enqueue for the same dir conflicts.
		if _, err := s.EnqueueMerge(ctx, dir, SessionMeta{}, true, "auto"); !errors.Is(err, ErrConflict) {
			t.Fatalf("want ErrConflict on duplicate, got %v", err)
		}
	})
}

func TestListOrdering(t *testing.T) {
	s := newTestService(t)
	ctx := context.Background()

	running := insertJob(t, s, "/s/run", statusRunning, stageEncode)
	_ = running
	// Two queued with explicit positions.
	q1 := insertJob(t, s, "/s/q1", statusQueued, "")
	q2 := insertJob(t, s, "/s/q2", statusQueued, "")
	_, _ = s.db.Exec(`UPDATE encode_jobs SET position = 5 WHERE id = ?`, q1)
	_, _ = s.db.Exec(`UPDATE encode_jobs SET position = 2 WHERE id = ?`, q2)
	done := insertJob(t, s, "/s/done", statusDone, "")

	queue, err := s.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(queue.Jobs) != 4 {
		t.Fatalf("got %d jobs, want 4", len(queue.Jobs))
	}
	// running first, then queued by position (q2 pos2 before q1 pos5), then terminal.
	if queue.Jobs[0].Status != statusRunning {
		t.Errorf("first job = %q, want running", queue.Jobs[0].Status)
	}
	if queue.Jobs[1].ID != q2 || queue.Jobs[2].ID != q1 {
		t.Errorf("queued order = [%d,%d], want [%d,%d]", queue.Jobs[1].ID, queue.Jobs[2].ID, q2, q1)
	}
	if queue.Jobs[3].ID != done {
		t.Errorf("last job = %d, want done job %d", queue.Jobs[3].ID, done)
	}
}

func TestRemoveRefusesRunning(t *testing.T) {
	s := newTestService(t)
	ctx := context.Background()

	run := insertJob(t, s, "/s/run", statusRunning, stageEncode)
	if err := s.Remove(ctx, run); !errors.Is(err, ErrConflict) {
		t.Fatalf("removing running job: want ErrConflict, got %v", err)
	}

	done := insertJob(t, s, "/s/done", statusDone, "")
	if err := s.Remove(ctx, done); err != nil {
		t.Fatalf("removing terminal job: %v", err)
	}
	if countJobs(t, s.db, "WHERE id = ?", done) != 0 {
		t.Errorf("done job not removed")
	}

	if err := s.Remove(ctx, 9999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("removing missing job: want ErrNotFound, got %v", err)
	}
}

func TestReorder(t *testing.T) {
	s := newTestService(t)
	ctx := context.Background()

	a := insertJob(t, s, "/s/a", statusQueued, "")
	b := insertJob(t, s, "/s/b", statusQueued, "")
	c := insertJob(t, s, "/s/c", statusQueued, "")
	// Normalize starting positions 1,2,3.
	for i, id := range []int64{a, b, c} {
		if _, err := s.db.Exec(`UPDATE encode_jobs SET position = ? WHERE id = ?`, i+1, id); err != nil {
			t.Fatal(err)
		}
	}

	queuedOrder := func() []int64 {
		q, err := s.List(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var ids []int64
		for _, j := range q.Jobs {
			if j.Status == statusQueued {
				ids = append(ids, j.ID)
			}
		}
		return ids
	}

	// Move c up: a, c, b.
	if err := s.Reorder(ctx, c, "up"); err != nil {
		t.Fatal(err)
	}
	if got := queuedOrder(); got[0] != a || got[1] != c || got[2] != b {
		t.Errorf("after up: %v, want [%d %d %d]", got, a, c, b)
	}

	// Move a down: c, a, b.
	if err := s.Reorder(ctx, a, "down"); err != nil {
		t.Fatal(err)
	}
	if got := queuedOrder(); got[0] != c || got[1] != a || got[2] != b {
		t.Errorf("after down: %v, want [%d %d %d]", got, c, a, b)
	}

	// Move b to top: b, c, a.
	if err := s.Reorder(ctx, b, "top"); err != nil {
		t.Fatal(err)
	}
	if got := queuedOrder(); got[0] != b || got[1] != c || got[2] != a {
		t.Errorf("after top: %v, want [%d %d %d]", got, b, c, a)
	}

	// Bad direction and non-queued targets.
	if err := s.Reorder(ctx, b, "sideways"); !errors.Is(err, ErrInvalid) {
		t.Errorf("bad direction: want ErrInvalid, got %v", err)
	}
	run := insertJob(t, s, "/s/run", statusRunning, stageEncode)
	if err := s.Reorder(ctx, run, "up"); !errors.Is(err, ErrNotFound) {
		t.Errorf("reorder running: want ErrNotFound, got %v", err)
	}
}

func TestCancelQueued(t *testing.T) {
	s := newTestService(t)
	ctx := context.Background()

	q := insertJob(t, s, "/s/q", statusQueued, "")
	if err := s.Cancel(ctx, q); err != nil {
		t.Fatalf("cancel queued: %v", err)
	}
	job, err := s.getJob(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	if job.Status != statusCancelled {
		t.Errorf("status = %q, want cancelled", job.Status)
	}

	// Cancelling a terminal job conflicts.
	if err := s.Cancel(ctx, q); !errors.Is(err, ErrConflict) {
		t.Errorf("cancel terminal: want ErrConflict, got %v", err)
	}
}

func TestRetry(t *testing.T) {
	s := newTestService(t)
	ctx := context.Background()

	failed := insertJob(t, s, "/s/f", statusFailed, "")
	_, _ = s.db.Exec(`UPDATE encode_jobs SET error = 'boom', warning = 'w' WHERE id = ?`, failed)

	if err := s.Retry(ctx, failed, "h264_nvenc"); err != nil {
		t.Fatalf("retry: %v", err)
	}
	job, err := s.getJob(ctx, failed)
	if err != nil {
		t.Fatal(err)
	}
	if job.Status != statusQueued {
		t.Errorf("status = %q, want queued", job.Status)
	}
	if job.Error != "" || job.Warning != "" {
		t.Errorf("error/warning not cleared: %q/%q", job.Error, job.Warning)
	}
	if job.Encoder != "h264_nvenc" {
		t.Errorf("encoder = %q, want re-snapshotted h264_nvenc", job.Encoder)
	}

	// Retrying a non-terminal job conflicts.
	if err := s.Retry(ctx, job.ID, "auto"); !errors.Is(err, ErrConflict) {
		t.Errorf("retry queued: want ErrConflict, got %v", err)
	}
}

func TestPauseResume(t *testing.T) {
	s := newTestService(t)
	ctx := context.Background()

	if s.paused(ctx) {
		t.Fatal("new queue should not be paused")
	}
	if err := s.SetPaused(ctx, true); err != nil {
		t.Fatal(err)
	}
	if !s.paused(ctx) {
		t.Error("SetPaused(true) did not persist")
	}
	q, _ := s.List(ctx)
	if !q.Paused {
		t.Error("List did not reflect paused")
	}
	if err := s.SetPaused(ctx, false); err != nil {
		t.Fatal(err)
	}
	if s.paused(ctx) {
		t.Error("SetPaused(false) did not clear")
	}
}

func TestClaimNext(t *testing.T) {
	s := newTestService(t)
	ctx := context.Background()

	first := insertJob(t, s, "/s/1", statusQueued, "")
	second := insertJob(t, s, "/s/2", statusQueued, "")
	_, _ = s.db.Exec(`UPDATE encode_jobs SET position = 1 WHERE id = ?`, first)
	_, _ = s.db.Exec(`UPDATE encode_jobs SET position = 2 WHERE id = ?`, second)

	job, err := s.claimNext(ctx)
	if err != nil {
		t.Fatalf("claimNext: %v", err)
	}
	if job == nil || job.ID != first {
		t.Fatalf("claimed %v, want %d", job, first)
	}
	if job.Status != statusRunning || job.Stage != stageEncode {
		t.Errorf("claimed job not running/encode: %q/%q", job.Status, job.Stage)
	}

	// Second claim gets the next queued job; a third returns nil.
	if job2, _ := s.claimNext(ctx); job2 == nil || job2.ID != second {
		t.Fatalf("second claim = %v, want %d", job2, second)
	}
	if job3, _ := s.claimNext(ctx); job3 != nil {
		t.Fatalf("third claim = %v, want nil (queue drained)", job3)
	}
}

func TestStartupRecoveryRequeuesRunning(t *testing.T) {
	dir := t.TempDir()
	s := newTestServiceIn(t, dir)
	run := insertJob(t, s, "/s/run", statusRunning, stageCopy)

	// Reopen the same DB file: initDB's recovery must reset running→queued.
	s.db.Close()
	s.logger.Close()

	logger := discardLogger(t)
	defer logger.Close()
	db, err := initDB(context.Background(), logger, filepath.Join(dir, "processing.db"))
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db.Close()

	var status, stage string
	if err := db.QueryRow(`SELECT status, stage FROM encode_jobs WHERE id = ?`, run).Scan(&status, &stage); err != nil {
		t.Fatal(err)
	}
	if status != statusQueued || stage != "" {
		t.Errorf("after recovery: %q/%q, want queued/''", status, stage)
	}
}

func TestPartNumberAndNormalize(t *testing.T) {
	cases := map[string]int{"vod1.mp4": 1, "vod12.mp4": 12, "vod0.mp4": 0}
	for name, want := range cases {
		if got := partNumber(name); got != want {
			t.Errorf("partNumber(%q) = %d, want %d", name, got, want)
		}
	}
	if got := normalizeEncoderPref("auto"); got != "" {
		t.Errorf("normalize(auto) = %q, want empty", got)
	}
	if got := normalizeEncoderPref("h264_qsv"); got != "h264_qsv" {
		t.Errorf("normalize(h264_qsv) = %q, want unchanged", got)
	}
}

func TestEnqueueMissingDir(t *testing.T) {
	s := newTestService(t)
	_, err := s.EnqueueMerge(context.Background(), filepath.Join(t.TempDir(), "nope"), SessionMeta{}, true, "auto")
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("missing dir: want ErrInvalid, got %v", err)
	}
}

// The sidebar badge counts what a user would call outstanding: the running
// job plus everything still waiting, and nothing that has already settled.
func TestPendingCount(t *testing.T) {
	s := newTestService(t)
	ctx := context.Background()

	if got := s.pendingCount(ctx); got != 0 {
		t.Errorf("empty queue: pendingCount = %d, want 0", got)
	}

	dir := makeSession(t, "vod1.mp4", "vod2.mp4")
	insertJob(t, s, dir, statusQueued, "")
	insertJob(t, s, dir, statusQueued, "")
	insertJob(t, s, dir, statusRunning, stageEncode)
	insertJob(t, s, dir, statusDone, "")
	insertJob(t, s, dir, statusFailed, "")
	insertJob(t, s, dir, statusCancelled, "")

	if got := s.pendingCount(ctx); got != 3 {
		t.Errorf("pendingCount = %d, want 3 (2 queued + 1 running)", got)
	}
}
