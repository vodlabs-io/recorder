package vodindex

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestIndexLearn covers the targeted update path: a new session folder is
// merged into a warm cache, a removed one is dropped, and neither triggers a
// rescan.
func TestIndexLearn(t *testing.T) {
	root := t.TempDir()
	first := filepath.Join(root, "p", "u", "2026-01-01_00-00-00+0000")
	writeFile(t, filepath.Join(first, "vod.mp4"), 10)

	ix := New(func() []string { return []string{root} }, nil, time.Hour)
	var changes atomic.Int32
	ix.SetOnChanged(func(int64) { changes.Add(1) })
	if got := len(waitScan(t, ix).Sessions); got != 1 {
		t.Fatalf("initial: %d sessions, want 1", got)
	}
	rev0 := ix.List(t.Context()).Rev
	if rev0 == 0 {
		t.Fatal("rev not set after first scan")
	}

	// A new session appears: Learn lists it without a rescan.
	second := filepath.Join(root, "p", "u", "2026-01-02_00-00-00+0000")
	writeFile(t, filepath.Join(second, "vod.mp4"), 20)
	ix.Learn(second)
	res := ix.List(t.Context())
	if res.Scanning {
		t.Error("Learn must not trigger a scan")
	}
	if len(res.Sessions) != 2 {
		t.Fatalf("after Learn: %d sessions, want 2", len(res.Sessions))
	}
	// Newest first: the learned session sorts by its folder datetime.
	if res.Sessions[0].Dir != second || res.Sessions[0].State != StateReady {
		t.Errorf("learned session: %+v", res.Sessions[0])
	}
	if res.Rev <= rev0 {
		t.Errorf("rev did not advance: %d -> %d", rev0, res.Rev)
	}

	// Re-learning with no change keeps the rev (and fires no announcement).
	n := changes.Load()
	revBefore := res.Rev
	ix.Learn(second)
	if got := ix.List(t.Context()); got.Rev != revBefore {
		t.Errorf("no-op Learn moved rev: %d -> %d", revBefore, got.Rev)
	}
	if changes.Load() != n {
		t.Error("no-op Learn fired onChanged")
	}

	// The folder disappears out from under us: Learn drops it.
	if err := os.RemoveAll(second); err != nil {
		t.Fatal(err)
	}
	ix.Learn(second)
	res = ix.List(t.Context())
	if len(res.Sessions) != 1 || res.Sessions[0].Dir != first {
		t.Fatalf("after removal Learn: %+v, want only %s", res.Sessions, first)
	}
	if res.Rev <= revBefore {
		t.Error("removal Learn did not advance rev")
	}
}

// TestIndexLearnActiveWork verifies Learn applies the same in-flight-work
// overlay as a full scan: a processing session with no output yet is listed as
// a placeholder, and the half-copied destination folder of a move stays
// suppressed.
func TestIndexLearnActiveWork(t *testing.T) {
	archive := t.TempDir()
	dest := t.TempDir()

	procDir := filepath.Join(archive, "p", "u", "s1")
	writeFile(t, filepath.Join(procDir, "session.log"), 1) // no vod output yet
	movDir := filepath.Join(archive, "p", "u", "s2")
	writeFile(t, filepath.Join(movDir, "vod.mp4"), 10)
	destCopy := filepath.Join(dest, "p", "u", "s2")
	writeFile(t, filepath.Join(destCopy, "vod.mp4"), 10)

	active := []ActiveWork{
		{BaseOutDir: procDir, Platform: "p", Username: "u", SessionID: "s1", State: StateProcessing},
		{BaseOutDir: movDir, Dest: dest, RelDir: filepath.Join("p", "u", "s2"), State: StateMoving},
	}
	ix := New(
		func() []string { return []string{archive, dest} },
		func(context.Context) []ActiveWork { return active },
		time.Hour,
	)
	waitScan(t, ix)

	ix.Learn(procDir, movDir, destCopy)
	res := ix.List(t.Context())
	p := sessionByDir(t, res, procDir)
	if p.State != StateProcessing || p.SessionID != "s1" {
		t.Errorf("processing placeholder: %+v", p)
	}
	if m := sessionByDir(t, res, movDir); m.State != StateMoving {
		t.Errorf("moving session state = %q", m.State)
	}
	for _, s := range res.Sessions {
		if s.Dir == destCopy {
			t.Error("Learn listed the suppressed half-moved destination folder")
		}
	}

	// The move finishes: the local folder is swept away, the destination is
	// real now. Learn must swap sides exactly like the next full scan would.
	active = nil
	if err := os.RemoveAll(movDir); err != nil {
		t.Fatal(err)
	}
	ix.Learn(movDir, destCopy)
	res = ix.List(t.Context())
	if d := sessionByDir(t, res, destCopy); d.State != StateReady || d.Root != dest {
		t.Errorf("post-move archive session: %+v", d)
	}
	for _, s := range res.Sessions {
		if s.Dir == movDir {
			t.Error("swept local folder still listed after Learn")
		}
	}
}

// TestIndexLearnOutsideRoots: a folder no scan root contains must never be
// listed, no matter what Learn is told.
func TestIndexLearnOutsideRoots(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "p", "u", "s", "vod.mp4"), 10)
	elsewhere := t.TempDir()
	writeFile(t, filepath.Join(elsewhere, "vod.mp4"), 10)

	ix := New(func() []string { return []string{root} }, nil, time.Hour)
	waitScan(t, ix)
	ix.Learn(elsewhere)
	for _, s := range ix.List(t.Context()).Sessions {
		if s.Dir == elsewhere {
			t.Error("Learn listed a folder outside every root")
		}
	}
}

// TestIndexRevStableAcrossUnchangedRescan: a rescan that finds identical
// content must keep the rev (so clients skip re-downloading) while still
// announcing completion (so a stale Scanning flag clears).
func TestIndexRevStableAcrossUnchangedRescan(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "p", "u", "s", "vod.mp4"), 10)

	ix := New(func() []string { return []string{root} }, nil, time.Hour)
	var announced atomic.Int32
	ix.SetOnChanged(func(int64) { announced.Add(1) })

	rev0 := waitScan(t, ix).Rev
	n := announced.Load()

	ix.Invalidate()
	if got := waitScan(t, ix).Rev; got != rev0 {
		t.Errorf("unchanged rescan moved rev: %d -> %d", rev0, got)
	}
	if announced.Load() == n {
		t.Error("unchanged rescan did not announce completion")
	}

	writeFile(t, filepath.Join(root, "p", "u", "s2", "vod.mp4"), 10)
	ix.Invalidate()
	if got := waitScan(t, ix).Rev; got <= rev0 {
		t.Errorf("changed rescan did not advance rev: %d -> %d", rev0, got)
	}
}

// TestIndexSnapshotRoundtrip: a fresh Index loading the previous run's
// snapshot serves it immediately (marked Scanning while the reconciling walk
// runs) with the same rev, and an unchanged walk keeps that rev.
func TestIndexSnapshotRoundtrip(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "p", "u", "s")
	writeFile(t, filepath.Join(dir, "vod.mp4"), 10)
	snap := filepath.Join(t.TempDir(), "vod-index.json")
	roots := func() []string { return []string{root} }

	ix1 := New(roots, nil, time.Hour)
	if err := ix1.SetSnapshotPath(snap); err != nil {
		t.Fatal(err)
	}
	rev1 := waitScan(t, ix1).Rev
	ix1.saveSnapshot() // force the debounced write now

	ix2 := New(roots, nil, time.Hour)
	if err := ix2.SetSnapshotPath(snap); err != nil {
		t.Fatal(err)
	}
	// First List: snapshot content served instantly, reconciling walk begins.
	first := ix2.List(t.Context())
	if !first.Scanning {
		t.Error("first List after snapshot load should be reconciling (Scanning)")
	}
	if len(first.Sessions) != 1 || first.Sessions[0].Dir != dir {
		t.Fatalf("snapshot sessions not served: %+v", first.Sessions)
	}
	if first.Rev != rev1 {
		t.Errorf("snapshot rev = %d, want %d", first.Rev, rev1)
	}
	if got := waitScan(t, ix2).Rev; got != rev1 {
		t.Errorf("unchanged reconcile moved rev: %d -> %d", rev1, got)
	}

	// A corrupt snapshot is reported but must not break a cold start.
	if err := os.WriteFile(snap, []byte("{nope"), 0o644); err != nil {
		t.Fatal(err)
	}
	ix3 := New(roots, nil, time.Hour)
	if err := ix3.SetSnapshotPath(snap); err == nil {
		t.Error("corrupt snapshot did not report an error")
	}
	if got := len(waitScan(t, ix3).Sessions); got != 1 {
		t.Fatalf("cold start after corrupt snapshot: %d sessions, want 1", got)
	}
}

// TestIndexLearnDuringScanSurvivesSwap: a Learn landing while a full walk is
// in flight must not be undone when the walk's result — computed from an older
// view of the world — swaps in. The walk is pinned to a stale active-work
// snapshot that still suppresses the session as "recording", so its result
// provably lacks the learned session; only the queued post-swap re-apply can
// restore it.
func TestIndexLearnDuringScanSurvivesSwap(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "p", "u", "old", "vod.mp4"), 10)
	newDir := filepath.Join(root, "p", "u", "new")

	var mu sync.Mutex
	current := []ActiveWork{{BaseOutDir: newDir, State: StateRecording}}
	setActive := func(w []ActiveWork) { mu.Lock(); current = w; mu.Unlock() }

	var calls atomic.Int32
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	activeFn := func(context.Context) []ActiveWork {
		mu.Lock()
		v := append([]ActiveWork(nil), current...)
		mu.Unlock()
		// The second scan: hold its (stale) active-work capture until the test
		// has changed the world and Learned the change mid-scan.
		if calls.Add(1) == 2 {
			started <- struct{}{}
			<-release
		}
		return v
	}

	ix := New(func() []string { return []string{root} }, activeFn, time.Hour)
	if got := len(waitScan(t, ix).Sessions); got != 1 {
		t.Fatalf("initial: %d sessions, want 1", got)
	}

	ix.Invalidate()
	ix.List(t.Context()) // kick the gated rescan
	<-started

	// Mid-scan the recording ends: output appears on disk, active work clears,
	// and processing Learns the folder.
	setActive(nil)
	writeFile(t, filepath.Join(newDir, "vod.mp4"), 10)
	ix.Learn(newDir)
	if got := len(ix.List(t.Context()).Sessions); got != 2 {
		t.Fatalf("mid-scan Learn not applied: %d sessions, want 2", got)
	}
	close(release) // the stale walk now completes and swaps in without newDir

	deadline := time.Now().Add(2 * time.Second)
	for {
		res := ix.List(t.Context())
		if !res.Scanning && len(res.Sessions) == 2 {
			return // the queued re-apply restored the learned session
		}
		if time.Now().After(deadline) {
			t.Fatalf("learned session lost to the stale swap: %+v", res.Sessions)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// A busy archive postpones an automatic reconcile: the walk that would compete
// with the move for the same disk is skipped, and the cached listing is still
// served. A cold cache is filled regardless — an empty archive is worse than
// contention.
func TestIndexBusyPostponesScan(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "p", "u", "2026-01-01_00-00-00+0000")
	writeFile(t, filepath.Join(dir, "vod.mp4"), 10)

	ix := New(func() []string { return []string{root} }, nil, time.Hour)
	var busy atomic.Bool
	var checks atomic.Int32
	ix.SetBusyCheck(func(context.Context) bool {
		checks.Add(1)
		return busy.Load()
	})

	// Busy from the outset: the cold scan must still run.
	busy.Store(true)
	if got := len(waitScan(t, ix).Sessions); got != 1 {
		t.Fatalf("cold scan while busy: %d sessions, want 1", got)
	}
	if checks.Load() != 0 {
		t.Errorf("busy check consulted %d times for a cold scan, want 0", checks.Load())
	}

	// A second session appears. While busy, the walk is postponed and the
	// existing listing is served unchanged.
	second := filepath.Join(root, "p", "u", "2026-01-02_00-00-00+0000")
	writeFile(t, filepath.Join(second, "vod.mp4"), 20)
	ix.Invalidate()
	res := waitScan(t, ix)
	if len(res.Sessions) != 1 {
		t.Errorf("busy: %d sessions, want the cached 1", len(res.Sessions))
	}
	if checks.Load() == 0 {
		t.Error("busy check was never consulted")
	}

	// Postponing must not leave the index spinning: with the stamp pushed
	// forward, a further List starts no new walk.
	before := checks.Load()
	ix.List(t.Context())
	ix.List(t.Context())
	if got := checks.Load(); got != before {
		t.Errorf("a postponed walk kept retrying: %d more checks", got-before)
	}

	// Once the move finishes, the reconcile runs.
	busy.Store(false)
	ix.Invalidate()
	if got := len(waitScan(t, ix).Sessions); got != 2 {
		t.Errorf("after busy cleared: %d sessions, want 2", got)
	}
}

// An explicitly requested walk (the Refresh button) runs even while the archive
// is busy: the user asking outranks the contention the gate exists to avoid.
func TestIndexForceRescanIgnoresBusy(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "p", "u", "2026-01-01_00-00-00+0000")
	writeFile(t, filepath.Join(dir, "vod.mp4"), 10)

	ix := New(func() []string { return []string{root} }, nil, time.Hour)
	var busy atomic.Bool
	ix.SetBusyCheck(func(context.Context) bool { return busy.Load() })
	if got := len(waitScan(t, ix).Sessions); got != 1 {
		t.Fatalf("cold scan: %d sessions, want 1", got)
	}

	second := filepath.Join(root, "p", "u", "2026-01-02_00-00-00+0000")
	writeFile(t, filepath.Join(second, "vod.mp4"), 20)

	busy.Store(true)
	ix.ForceRescan()
	if got := len(waitScan(t, ix).Sessions); got != 2 {
		t.Errorf("forced rescan while busy: %d sessions, want 2", got)
	}

	// The bypass is consumed by that walk, not sticky: the next automatic
	// invalidation is postponed again.
	third := filepath.Join(root, "p", "u", "2026-01-03_00-00-00+0000")
	writeFile(t, filepath.Join(third, "vod.mp4"), 30)
	ix.Invalidate()
	if got := len(waitScan(t, ix).Sessions); got != 2 {
		t.Errorf("plain Invalidate while busy: %d sessions, want the cached 2", got)
	}
}

// syncBuffer collects log output from the scan goroutine.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// A postponed walk must say so. Without it a listing that quietly stops
// updating is indistinguishable from one with nothing to report.
func TestIndexLogsScanOutcomes(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "p", "u", "2026-01-01_00-00-00+0000")
	writeFile(t, filepath.Join(dir, "vod.mp4"), 10)

	var out syncBuffer
	ix := New(func() []string { return []string{root} }, nil, time.Hour)
	ix.SetLogger(slog.New(slog.NewTextHandler(&out, &slog.HandlerOptions{Level: slog.LevelDebug})))
	var busy atomic.Bool
	ix.SetBusyCheck(func(context.Context) bool { return busy.Load() })

	waitScan(t, ix)
	if got := out.String(); !strings.Contains(got, "Scanned the VOD Archive") {
		t.Errorf("completed scan was not logged; got:\n%s", got)
	}
	if got := out.String(); !strings.Contains(got, "sessions=1") {
		t.Errorf("scan log did not report the session count; got:\n%s", got)
	}

	busy.Store(true)
	ix.Invalidate()
	waitScan(t, ix)
	if got := out.String(); !strings.Contains(got, "Postponed the VOD Archive scan") {
		t.Errorf("postponed scan was not logged; got:\n%s", got)
	}
}

func TestIndexNilLoggerIsSafe(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "p", "u", "s", "vod.mp4"), 10)

	ix := New(func() []string { return []string{root} }, nil, time.Hour)
	ix.SetLogger(nil) // must restore the discard default, not leave a nil deref
	if got := len(waitScan(t, ix).Sessions); got != 1 {
		t.Fatalf("scan with a nil logger: %d sessions, want 1", got)
	}
}

// TestIndexRescanPicksUpRecoveredRoot: a root that was unreachable during the
// last walk is reported in UnavailableRoots and its sessions are missing.
// Rescan must start the reconciling walk on its own — without waiting for a
// List — so a remounted drive's VODs appear and the banner clears.
func TestIndexRescanPicksUpRecoveredRoot(t *testing.T) {
	live := t.TempDir()
	writeFile(t, filepath.Join(live, "p", "u", "s1", "vod.mp4"), 10)
	// Not yet created: stands in for the unmounted volume.
	offline := filepath.Join(t.TempDir(), "volume")

	ix := New(func() []string { return []string{live, offline} }, nil, time.Hour)
	var changes atomic.Int32
	ix.SetOnChanged(func(int64) { changes.Add(1) })

	res := waitScan(t, ix)
	if len(res.UnavailableRoots) != 1 || res.UnavailableRoots[0] != offline {
		t.Fatalf("unavailable roots: %v, want [%s]", res.UnavailableRoots, offline)
	}
	if len(res.Sessions) != 1 {
		t.Fatalf("%d sessions, want 1", len(res.Sessions))
	}

	// The volume comes back, carrying a session the last walk never saw.
	writeFile(t, filepath.Join(offline, "p", "u", "s2", "vod.mp4"), 20)

	n := changes.Load()
	ix.Rescan()
	// Wait on the completion hook rather than List: a List would schedule the
	// walk itself, which would pass even if Rescan did nothing.
	deadline := time.Now().Add(2 * time.Second)
	for changes.Load() == n {
		if time.Now().After(deadline) {
			t.Fatal("Rescan did not run a walk on its own")
		}
		time.Sleep(2 * time.Millisecond)
	}

	res = waitScan(t, ix)
	if len(res.UnavailableRoots) != 0 {
		t.Errorf("unavailable roots after recovery: %v, want none", res.UnavailableRoots)
	}
	if len(res.Sessions) != 2 {
		t.Errorf("%d sessions after recovery, want 2", len(res.Sessions))
	}
}
