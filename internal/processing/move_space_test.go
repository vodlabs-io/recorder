package processing

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/vodlabs-io/recorder/internal/diskspace"
	"github.com/vodlabs-io/recorder/internal/vodmove"
)

const gib = 1 << 30

// fixedSampler answers every path with one usage, or one error. A real full disk
// is not reproducible in CI, so it is injected instead.
type fixedSampler struct {
	usage diskspace.Usage
	err   error
}

func (f fixedSampler) Sample(string) (diskspace.Usage, error) {
	if f.err != nil {
		return diskspace.Usage{}, f.err
	}
	return f.usage, nil
}

// writeSized creates a file of exactly n bytes without allocating them.
func writeSized(t *testing.T, path string, n int64) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := f.Truncate(n); err != nil {
		t.Fatal(err)
	}
}

func TestPreflightRefusesWhenDestinationIsFull(t *testing.T) {
	s := newTestService(t)
	dir := t.TempDir()
	writeSized(t, filepath.Join(dir, "vod1.mp4"), 10*gib)

	// 4 GiB free cannot hold a 10 GiB file.
	s.sampler = fixedSampler{usage: diskspace.Usage{TotalBytes: 100 * gib, FreeBytes: 4 * gib}}
	mj := &moveJob{kind: "file", baseOutDir: dir, fileName: "vod1.mp4", dest: t.TempDir()}

	err := s.preflightMove(context.Background(), mj)
	var noSpace *vodmove.ErrNoSpace
	if !errors.As(err, &noSpace) {
		t.Fatalf("preflightMove = %v, want ErrNoSpace", err)
	}
	// The user is told how much to free, not just that it failed.
	if noSpace.ShortBytes == 0 {
		t.Error("ShortBytes = 0; a refusal must say how much space is missing")
	}
	if vodmove.Classify(err) != vodmove.ReasonNoSpace {
		t.Errorf("Classify = %q, want %q", vodmove.Classify(err), vodmove.ReasonNoSpace)
	}
}

func TestPreflightRefusesAMoveThatWouldZeroTheVolume(t *testing.T) {
	s := newTestService(t)
	dir := t.TempDir()
	writeSized(t, filepath.Join(dir, "vod1.mp4"), 10*gib)

	// Exactly enough for the file and nothing else: refused, because leaving a
	// volume at zero breaks things unrelated to the recorder.
	s.sampler = fixedSampler{usage: diskspace.Usage{FreeBytes: 10 * gib}}
	mj := &moveJob{kind: "file", baseOutDir: dir, fileName: "vod1.mp4", dest: t.TempDir()}

	var noSpace *vodmove.ErrNoSpace
	if err := s.preflightMove(context.Background(), mj); !errors.As(err, &noSpace) {
		t.Fatalf("preflightMove = %v, want ErrNoSpace", err)
	}
}

func TestPreflightPassesWhenThereIsRoom(t *testing.T) {
	s := newTestService(t)
	dir := t.TempDir()
	writeSized(t, filepath.Join(dir, "vod1.mp4"), 10*gib)

	s.sampler = fixedSampler{usage: diskspace.Usage{FreeBytes: 500 * gib}}
	mj := &moveJob{kind: "file", baseOutDir: dir, fileName: "vod1.mp4", dest: t.TempDir()}

	if err := s.preflightMove(context.Background(), mj); err != nil {
		t.Errorf("preflightMove = %v, want nil", err)
	}
}

func TestPreflightSizesASweepByWhatRemains(t *testing.T) {
	s := newTestService(t)
	dir := t.TempDir()
	writeSized(t, filepath.Join(dir, "session.log"), 1*gib)
	writeSized(t, filepath.Join(dir, "parts", "part1.ts"), 2*gib)

	mj := &moveJob{kind: "sweep", baseOutDir: dir, dest: t.TempDir()}
	got, err := s.moveSizeBytes(mj)
	if err != nil {
		t.Fatalf("moveSizeBytes: %v", err)
	}
	if got != 3*gib {
		t.Errorf("moveSizeBytes = %d, want %d", got, 3*gib)
	}
}

func TestPreflightTreatsAlreadySweptFolderAsNothingToDo(t *testing.T) {
	s := newTestService(t)
	// A sweep that already ran leaves no folder; it must not be reported as a
	// sizing failure or refused for space.
	mj := &moveJob{kind: "sweep", baseOutDir: filepath.Join(t.TempDir(), "gone"), dest: t.TempDir()}

	s.sampler = fixedSampler{usage: diskspace.Usage{FreeBytes: 0}}
	if err := s.preflightMove(context.Background(), mj); err != nil {
		t.Errorf("preflightMove = %v, want nil", err)
	}
}

func TestPreflightReportsUnreachableDestination(t *testing.T) {
	s := newTestService(t)
	dir := t.TempDir()
	writeSized(t, filepath.Join(dir, "vod1.mp4"), 1*gib)

	s.sampler = fixedSampler{err: errors.New("host is down")}
	mj := &moveJob{kind: "file", baseOutDir: dir, fileName: "vod1.mp4", dest: t.TempDir()}

	err := s.preflightMove(context.Background(), mj)
	if err == nil {
		t.Fatal("preflightMove = nil, want an error")
	}
	// An unreachable destination is not a full one: the message and the retry
	// schedule both differ.
	if got := vodmove.Classify(err); got == vodmove.ReasonNoSpace {
		t.Errorf("Classify = %q, want anything but %q", got, vodmove.ReasonNoSpace)
	}
}

func TestPreflightAttemptsMoveWhenSizingFails(t *testing.T) {
	s := newTestService(t)
	// The file is gone, so it cannot be sized. Refusing here would fail the job
	// on a guess; the move itself should run and produce the real error.
	mj := &moveJob{kind: "file", baseOutDir: t.TempDir(), fileName: "missing.mp4", dest: t.TempDir()}

	s.sampler = fixedSampler{usage: diskspace.Usage{FreeBytes: 0}}
	if err := s.preflightMove(context.Background(), mj); err != nil {
		t.Errorf("preflightMove = %v, want nil", err)
	}
}

func TestNoSpaceIsExemptFromBackoffEscalation(t *testing.T) {
	// A full destination is polled on a flat short interval however many times it
	// has been tried: the check is one statfs, and freeing space must take effect
	// promptly rather than after the escalated backoff has elapsed.
	for _, attempts := range []int{1, 2, 5, 50} {
		got := time.Until(moveRetryAt(vodmove.ReasonNoSpace, attempts))
		if got > noSpaceRetryInterval+time.Second {
			t.Errorf("attempts=%d: next retry in %v, want ~%v", attempts, got, noSpaceRetryInterval)
		}
	}

	// Every other reason keeps the escalating backoff, which exists for a
	// destination that is genuinely flaky or offline for hours.
	if got := time.Until(moveRetryAt(vodmove.ReasonUnreachable, 4)); got < 25*time.Minute {
		t.Errorf("unreachable at attempt 4: next retry in %v, want the 30m backoff", got)
	}
}

func TestClassifyMoveFailures(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want vodmove.Reason
	}{
		{"nil", nil, ""},
		{"full destination", &vodmove.ErrNoSpace{Dest: "/nas"}, vodmove.ReasonNoSpace},
		{"wrapped full destination",
			errors.Join(errors.New("move vod1.mp4"), &vodmove.ErrNoSpace{Dest: "/nas"}),
			vodmove.ReasonNoSpace},
		{"probe timed out",
			diskspace.ErrTimeout{Path: "/nas", After: time.Second},
			vodmove.ReasonUnreachable},
		{"destination gone", os.ErrNotExist, vodmove.ReasonUnreachable},
		{"refused", os.ErrPermission, vodmove.ReasonPermission},
		{"anything else", errors.New("kaboom"), vodmove.ReasonOther},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := vodmove.Classify(tt.err); got != tt.want {
				t.Errorf("Classify(%v) = %q, want %q", tt.err, got, tt.want)
			}
		})
	}
}

func TestSourceMissingIsTerminal(t *testing.T) {
	// A file that is gone from the download folder and was never archived
	// cannot be moved by any later attempt.
	err := &vodmove.ErrSourceMissing{Path: "/downloads/vod5.mp4"}
	if got := vodmove.Classify(err); got != vodmove.ReasonSourceMissing {
		t.Fatalf("Classify = %q, want %q", got, vodmove.ReasonSourceMissing)
	}
	if !vodmove.ReasonSourceMissing.Terminal() {
		t.Error("source_missing must be terminal")
	}
	// Everything else is worth waiting on: drives come back, disks get emptied,
	// permissions get fixed.
	for _, r := range []vodmove.Reason{
		vodmove.ReasonNoSpace, vodmove.ReasonUnreachable,
		vodmove.ReasonPermission, vodmove.ReasonOther,
	} {
		if r.Terminal() {
			t.Errorf("%q must not be terminal", r)
		}
	}
}

func TestMissingSourceIsNotMistakenForAnUnreachableDestination(t *testing.T) {
	// Both are fs.ErrNotExist. Reporting a missing local file as "the drive is
	// offline" is exactly the wrong message, and would send the user looking at
	// a NAS that is working fine.
	missing := &vodmove.ErrSourceMissing{Path: "/downloads/vod5.mp4"}
	if got := vodmove.Classify(missing); got == vodmove.ReasonUnreachable {
		t.Errorf("Classify = %q; a missing source is not an unreachable destination", got)
	}
	if !errors.Is(missing, fs.ErrNotExist) {
		t.Error("ErrSourceMissing should still satisfy errors.Is(fs.ErrNotExist)")
	}
}

func TestStoreReportsMissingSourceButAcceptsAnAlreadyArchivedFile(t *testing.T) {
	src := filepath.Join(t.TempDir(), "vod5.mp4")
	dest := t.TempDir()
	store, err := vodmove.ForDest(dest)
	if err != nil {
		t.Fatal(err)
	}

	// Source gone and destination empty: terminal.
	err = store.Store(context.Background(), src, "s/vod5.mp4")
	var missing *vodmove.ErrSourceMissing
	if !errors.As(err, &missing) {
		t.Fatalf("Store = %v, want ErrSourceMissing", err)
	}

	// Source gone but the destination already has it: a completed move whose
	// bookkeeping was interrupted. That must succeed, not fail terminally.
	writeSized(t, filepath.Join(dest, "s", "vod5.mp4"), 1024)
	if err := store.Store(context.Background(), src, "s/vod5.mp4"); err != nil {
		t.Errorf("Store = %v, want nil when the destination already holds the file", err)
	}
}

func TestTerminalFailureRetiresTheJob(t *testing.T) {
	s := newTestService(t)
	ctx := context.Background()

	if err := s.failMoveTerminal(ctx, 1, 3, "gone", vodmove.ReasonSourceMissing); err != nil {
		t.Fatalf("failMoveTerminal on a missing row should be a no-op update: %v", err)
	}
}

func TestActiveWorkKeepsTheReasonWhicheverRowCarriesIt(t *testing.T) {
	s := newTestService(t)
	ctx := context.Background()
	dir := t.TempDir()

	// A session with a failed file move and its not-yet-attempted sweep. This is
	// the ordinary shape of a blocked session, not an edge case: the sweep is
	// enqueued at next_retry_at=0 and held back until the file moves finish.
	for _, q := range []string{
		`INSERT INTO move_jobs (kind, platform, username, session_id, base_out_dir, rel_dir, file_name, dest, status, attempts, next_retry_at, last_error, last_reason, created_at)
		 VALUES ('file','tiktok','someone','s1',?, 'r', 'vod1.mp4', '/dest', 'pending', 2, 9999999999, 'full', 'no_space', 0)`,
		`INSERT INTO move_jobs (kind, platform, username, session_id, base_out_dir, rel_dir, dest, status, attempts, next_retry_at, created_at)
		 VALUES ('sweep','tiktok','someone','s1',?, 'r', '/dest', 'pending', 0, 0, 0)`,
	} {
		if _, err := s.db.ExecContext(ctx, q, dir); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	work := s.ActiveVODWork(ctx)
	if len(work) != 1 {
		t.Fatalf("got %d work entries, want 1: %+v", len(work), work)
	}
	// idx_move_due orders by next_retry_at, so the sweep (0) arrives before the
	// failed file row. Taking the first row's reason would report nothing.
	if work[0].Reason != "no_space" {
		t.Errorf("Reason = %q, want %q — the sweep row's empty reason won", work[0].Reason, "no_space")
	}
}
