package vodindex

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// scanActiveTimeout bounds the context handed to the active-work lookup during
// a background scan or Learn (KV + SQLite queries). The filesystem walk itself
// isn't context-bound; it always runs to completion.
const scanActiveTimeout = 15 * time.Second

// busyRetryAfter is how long a TTL walk waits after being postponed for a busy
// archive. Short enough that a reconcile is not put off for another whole TTL,
// long enough that a stream of Lists does not re-ask on every request.
const busyRetryAfter = 5 * time.Minute

// snapshotSaveDelay debounces snapshot writes so a burst of changes (a
// multi-part move landing file by file, several deletes) coalesces into one
// file write.
const snapshotSaveDelay = time.Second

// snapshotVersion tags the snapshot file format; a mismatch (older daemon's
// file, future format change) makes the snapshot load a no-op and the first
// scan rebuilds from the filesystem.
const snapshotVersion = 2

// indexSnapshot is the on-disk form of the cached listing. Rev rides inside
// the Result, so a restarted daemon resumes the same content generation and
// clients holding that rev skip re-downloading an unchanged archive.
type indexSnapshot struct {
	Version int    `json:"version"`
	Result  Result `json:"result"`
}

// Index caches scans of the VOD Archive's roots. A scan can walk slow network drives and
// take far longer than a client's request timeout, so List never runs one
// inline: it serves the cached result immediately (even if stale) and, when the
// cache is missing or expired, kicks off a single background rescan. Changes
// the daemon makes itself are folded in incrementally — Learn rescans just the
// affected session folders, Forget drops a deleted one — so full walks only
// happen at TTL to reconcile out-of-band edits. Every content change bumps Rev
// and fires the onChanged hook; with a snapshot path set, the listing is also
// persisted so a restarted daemon serves its last known archive instantly.
type Index struct {
	roots  func() []string
	active func(ctx context.Context) []ActiveWork
	ttl    time.Duration

	// logger records scan outcomes. A walk over a network archive can take
	// minutes or be postponed entirely, and without a record there is nothing
	// to distinguish that from a listing that simply never updates.
	logger *slog.Logger

	// busy reports that the archive volume is being written to. Consulted from
	// the scan goroutine, never under mu, since it may touch a database.
	busy func(ctx context.Context) bool

	mu        sync.Mutex
	cached    *Result
	cachedAt  time.Time
	scanning  bool
	onChanged func(rev int64)
	rev       int64  // content generation of cached (unix ns, monotonic)
	fp        string // content fingerprint of cached (sessions + roots)

	// Mutations arriving while a walk is in flight would be silently undone
	// when the walk's (possibly older) view of those folders swaps in; they're
	// queued here and re-applied (via Learn) after the swap.
	pendingLearn map[string]bool
	// An Invalidate during an in-flight walk must survive the swap: the walk
	// that just finished began before the invalidation.
	invalidatedMidScan bool
	// forceScan marks the pending walk as explicitly asked for, so the busy
	// gate does not postpone it. Consumed when that walk starts.
	forceScan bool

	snapshotPath string
	savePending  bool
}

// New builds an Index. roots returns the current scan roots (the Download,
// Archive and Scan folders); active reports sessions the processing service is
// still working on. ttl bounds how stale a served scan may be before a
// background refresh is scheduled.
func New(roots func() []string, active func(ctx context.Context) []ActiveWork, ttl time.Duration) *Index {
	return &Index{
		roots:  roots,
		active: active,
		ttl:    ttl,
		logger: slog.New(slog.DiscardHandler),
	}
}

// SetLogger installs the logger for scan outcomes. Call once before the first
// List; nil restores the default, which discards.
func (ix *Index) SetLogger(l *slog.Logger) {
	if l == nil {
		l = slog.New(slog.DiscardHandler)
	}
	ix.logger = l
}

// SetOnChanged registers a callback fired with the new Rev whenever the cached
// content changes (scan swap, Learn, Forget) — and also, with an unchanged
// Rev, when a scan completes without finding changes, so clients showing a
// stale Scanning flag re-check. Fired off-lock.
func (ix *Index) SetOnChanged(fn func(rev int64)) {
	ix.mu.Lock()
	ix.onChanged = fn
	ix.mu.Unlock()
}

// SetBusyCheck installs fn to report whether the archive volume is currently
// being written to. A full walk competes for the same disk as that work — on a
// network archive or a slow spindle, badly enough to stall both — so while fn
// reports true, TTL refreshes are postponed. It never blocks the first scan:
// an empty archive listing is worse than contention. Call once before the
// first List.
func (ix *Index) SetBusyCheck(fn func(ctx context.Context) bool) {
	ix.busy = fn
}

// SetSnapshotPath enables persistence: the cached listing is saved to path
// (debounced, atomic rename) whenever its content changes, and any existing
// snapshot is loaded now so a freshly started daemon serves its last known
// archive immediately while the first reconciling walk runs (List marks the
// served listing Scanning). Call once before the first List. A missing
// snapshot is not an error; an unreadable one is returned for logging but
// otherwise just means a cold start.
func (ix *Index) SetSnapshotPath(path string) error {
	ix.mu.Lock()
	ix.snapshotPath = path
	ix.mu.Unlock()

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var snap indexSnapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return fmt.Errorf("parse vod index snapshot: %w", err)
	}
	if snap.Version != snapshotVersion {
		return nil
	}

	ix.mu.Lock()
	defer ix.mu.Unlock()
	if ix.cached != nil {
		return nil // a scan already produced fresher data
	}
	res := snap.Result
	res.Scanning = false
	ix.cached = &res
	ix.rev = res.Rev
	ix.fp = fingerprint(&res)
	// cachedAt stays zero: the snapshot serves immediately but counts as
	// expired, so the first List schedules the reconciling walk.
	return nil
}

// List returns the current VOD Archive contents without blocking on a scan. It
// serves the last completed scan (even past its TTL) and, when the cache is
// missing or expired, schedules a background rescan. The returned Result's
// Scanning flag reports whether a refresh is in flight. Before the very first
// scan completes it returns an empty, Scanning result rather than blocking.
func (ix *Index) List(ctx context.Context) Result {
	ix.mu.Lock()
	defer ix.mu.Unlock()

	if ix.cached == nil || time.Since(ix.cachedAt) >= ix.ttl {
		ix.startScanLocked()
	}
	if ix.cached != nil {
		res := *ix.cached
		res.Scanning = ix.scanning
		return res
	}
	return Result{Scanning: ix.scanning}
}

// startScanLocked launches one background rescan unless one is already running.
// The caller must hold ix.mu. The scan reads roots()/active() off-lock so those
// (KV/config/DB) calls never block List callers.
func (ix *Index) startScanLocked() {
	if ix.scanning {
		return
	}
	ix.scanning = true
	go ix.runScan()
}

// runScan performs one scan off the lock, then swaps it into the cache and
// announces completion. A scan whose content matches the cache leaves the Rev
// (and snapshot) untouched so clients holding that rev skip re-downloading;
// the completion announcement still fires for their Scanning flags.
func (ix *Index) runScan() {
	ctx, cancel := context.WithTimeout(context.Background(), scanActiveTimeout)

	// A cold cache must be filled whatever the disk is doing; anything else is
	// a reconcile that can wait for the archive to go quiet.
	ix.mu.Lock()
	cold, forced := ix.cached == nil, ix.forceScan
	ix.forceScan = false
	ix.mu.Unlock()
	if !cold && !forced && ix.busy != nil && ix.busy(ctx) {
		cancel()
		ix.postponeScan()
		return
	}

	var active []ActiveWork
	if ix.active != nil {
		active = ix.active(ctx)
	}
	roots := ix.roots()
	started := time.Now()
	res := ScanReusing(ctx, roots, active, ix.reusableSessions())
	cancel()
	took := time.Since(started)
	fp := fingerprint(&res)

	ix.logger.Info("Scanned the VOD Archive",
		"duration", took.Round(time.Millisecond),
		"sessions", len(res.Sessions),
		"roots", len(roots),
		"changed", fp != ix.fingerprintSnapshot(),
		"unavailable_roots", len(res.UnavailableRoots))
	if len(res.UnavailableRoots) > 0 {
		ix.logger.Warn("Some storage roots were unreachable during the scan",
			"roots", res.UnavailableRoots)
	}

	ix.mu.Lock()
	if fp != ix.fp {
		ix.bumpRevLocked()
		res.Rev = ix.rev
		ix.cached = &res
		ix.fp = fp
		ix.scheduleSaveLocked()
	} else if ix.cached != nil {
		// Identical content: keep the cache and its rev, refresh the stamp.
		upd := *ix.cached
		upd.ScannedAt = res.ScannedAt
		ix.cached = &upd
	}
	ix.scanning = false
	if ix.invalidatedMidScan {
		ix.invalidatedMidScan = false
		ix.cachedAt = time.Time{} // the next List schedules another walk
	} else {
		ix.cachedAt = time.Now()
	}
	pending := ix.pendingLearn
	ix.pendingLearn = nil
	onChanged, rev := ix.onChanged, ix.rev
	ix.mu.Unlock()

	if onChanged != nil {
		onChanged(rev)
	}
	if len(pending) > 0 {
		dirs := make([]string, 0, len(pending))
		for d := range pending {
			dirs = append(dirs, d)
		}
		go ix.Learn(dirs...)
	}
}

// reusableSessions indexes the cached listing by folder so the next walk can
// carry entries over. Returns nil when there is nothing cached, which makes the
// walk read everything — the correct behaviour for a cold start.
func (ix *Index) reusableSessions() map[string]VOD {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	if ix.cached == nil {
		return nil
	}
	m := make(map[string]VOD, len(ix.cached.Sessions))
	for _, sess := range ix.cached.Sessions {
		m[sess.Dir] = sess
	}
	return m
}

// fingerprintSnapshot reads the cached fingerprint under the lock, so the scan
// log can say whether the walk found anything new without racing the swap.
func (ix *Index) fingerprintSnapshot() string {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	return ix.fp
}

// postponeScan ends a walk that never ran because the archive was busy. The
// cache keeps its contents and its rev; only the stamp moves, so the next List
// re-asks after busyRetryAfter instead of immediately. The change hook still
// fires so clients drop the Scanning flag this attempt raised.
func (ix *Index) postponeScan() {
	ix.mu.Lock()
	ix.scanning = false
	// Clear any invalidation this attempt was answering. Leaving it set would
	// leave the stamp expired, so the next List would start another walk that
	// postpones in turn — a spin for as long as the archive stays busy. The
	// retry stamp already guarantees the reconcile happens.
	ix.invalidatedMidScan = false
	ix.cachedAt = time.Now().Add(busyRetryAfter - ix.ttl)
	onChanged, rev := ix.onChanged, ix.rev
	ix.mu.Unlock()
	ix.logger.Info("Postponed the VOD Archive scan; the archive is busy",
		"retry_in", busyRetryAfter)
	if onChanged != nil {
		onChanged(rev)
	}
}

// Invalidate schedules a background rescan on the next List while still
// serving the last completed scan until it finishes. Call after anything that
// changes the VOD Archive that a targeted Learn/Forget can't express. Safe during
// an in-flight scan: that scan began before the invalidation, so another walk
// is scheduled behind it. Subject to the busy gate — use ForceRescan for a walk
// a user asked for.
func (ix *Index) Invalidate() {
	ix.invalidate(false)
}

// ForceRescan is Invalidate for a walk someone explicitly asked for — the VOD
// Archive's Refresh button. It bypasses the busy gate: a user who asks for a
// reconcile while a move is running has said which they would rather have.
func (ix *Index) ForceRescan() {
	ix.invalidate(true)
}

// Rescan invalidates the cache and starts the walk now instead of waiting for
// the next List. Use it for a change the daemon spots on its own — a storage
// root coming back online — where no client request is on its way to trigger
// the reconcile. Subject to the busy gate.
func (ix *Index) Rescan() {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	ix.cachedAt = time.Time{} // force-expire; keep the data for stale-serving
	if ix.scanning {
		// The in-flight walk began before this change; the completion hook
		// nudges clients, whose next List starts the follow-up walk.
		ix.invalidatedMidScan = true
		return
	}
	ix.startScanLocked()
}

func (ix *Index) invalidate(force bool) {
	ix.mu.Lock()
	ix.cachedAt = time.Time{} // force-expire; keep the data for stale-serving
	if force {
		ix.forceScan = true
	}
	if ix.scanning {
		ix.invalidatedMidScan = true
	}
	ix.mu.Unlock()
}

// Learn rescans just the given session folders and merges them into the cached
// listing — the targeted counterpart of a full rescan for changes the daemon
// makes itself (a recording produced, a move completed). A folder that no longer
// holds a listable session is dropped; a new or changed one is (re)listed with
// its current processing/moving state. Runs the same per-folder logic as Scan
// (ScanOne), so a later full walk reaches the identical result.
func (ix *Index) Learn(dirs ...string) {
	if len(dirs) == 0 {
		return
	}

	ix.mu.Lock()
	if ix.scanning {
		// The in-flight walk may have read these folders before the change
		// landed on disk; queue them to re-apply after its result swaps in.
		if ix.pendingLearn == nil {
			ix.pendingLearn = make(map[string]bool)
		}
		for _, d := range dirs {
			ix.pendingLearn[filepath.Clean(d)] = true
		}
	}
	if ix.cached == nil {
		// Nothing to merge into yet; the initial scan picks the folders up.
		ix.startScanLocked()
		ix.mu.Unlock()
		return
	}
	ix.mu.Unlock()

	// Resolve the folders' current state off-lock (KV/SQLite + disk I/O).
	ctx, cancel := context.WithTimeout(context.Background(), scanActiveTimeout)
	var active []ActiveWork
	if ix.active != nil {
		active = ix.active(ctx)
	}
	cancel()
	roots := ix.roots()

	learned := make(map[string]*VOD, len(dirs))
	for _, d := range dirs {
		d = filepath.Clean(d)
		if s, ok := scanOne(roots, d, active); ok {
			learned[d] = &s
		} else {
			learned[d] = nil
		}
	}

	ix.mu.Lock()
	if ix.cached == nil {
		ix.mu.Unlock()
		return
	}
	next := *ix.cached
	sessions := make([]VOD, 0, len(next.Sessions)+len(learned))
	for _, s := range next.Sessions {
		if _, relearned := learned[s.Dir]; !relearned {
			sessions = append(sessions, s)
		}
	}
	for _, s := range learned {
		if s != nil {
			sessions = append(sessions, *s)
		}
	}
	sortSessions(sessions)
	next.Sessions = sessions
	ix.swapIfChangedLocked(&next)
}

// Forget drops one session from the cached scan without rescanning, keeping the
// served listing correct right after a delete (a full rescan of a large archive
// is too slow to run inline, and would briefly re-list or blank the VOD Archive).
// The cache stays "fresh", so no background scan is triggered; the TTL refresh
// still reconciles with the filesystem.
func (ix *Index) Forget(dir string) {
	dir = filepath.Clean(dir)
	ix.mu.Lock()
	if ix.scanning {
		// An in-flight walk may still have listed the folder; re-check it
		// after the swap (a Learn of a deleted folder is a removal).
		if ix.pendingLearn == nil {
			ix.pendingLearn = make(map[string]bool)
		}
		ix.pendingLearn[dir] = true
	}
	if ix.cached == nil {
		ix.mu.Unlock()
		return
	}
	kept := make([]VOD, 0, len(ix.cached.Sessions))
	for _, s := range ix.cached.Sessions {
		if s.Dir != dir {
			kept = append(kept, s)
		}
	}
	next := *ix.cached
	next.Sessions = kept
	ix.swapIfChangedLocked(&next)
}

// swapIfChangedLocked installs next as the cached listing if its content
// differs from what's cached, bumping Rev, scheduling a snapshot save, and
// firing onChanged. The caller must hold ix.mu; it is released either way.
func (ix *Index) swapIfChangedLocked(next *Result) {
	fp := fingerprint(next)
	if fp == ix.fp {
		ix.mu.Unlock()
		return
	}
	ix.bumpRevLocked()
	next.Rev = ix.rev
	ix.cached = next
	ix.fp = fp
	ix.scheduleSaveLocked()
	onChanged, rev := ix.onChanged, ix.rev
	ix.mu.Unlock()
	if onChanged != nil {
		onChanged(rev)
	}
}

// Find returns the listed session whose folder is dir. Clients pass session
// folders back for thumbnails and deletion; resolving them against the scan
// keeps those endpoints from touching arbitrary filesystem paths. It serves the
// cached scan (a client only knows a dir from a prior successful List, so the
// cache is warm by the time these arrive).
func (ix *Index) Find(ctx context.Context, dir string) (VOD, bool) {
	dir = filepath.Clean(dir)
	for _, s := range ix.List(ctx).Sessions {
		if s.Dir == dir {
			return s, true
		}
	}
	return VOD{}, false
}

// bumpRevLocked advances the content generation. Wall-clock based so it stays
// monotonic across restarts (the snapshot restores the last rev); the +1 guard
// keeps it strictly increasing even under clock skew or same-nanosecond bumps.
func (ix *Index) bumpRevLocked() {
	now := time.Now().UnixNano()
	if now <= ix.rev {
		now = ix.rev + 1
	}
	ix.rev = now
}

// fingerprint hashes the listing's client-visible content — sessions and
// unavailable roots, not the scan stamp — so unchanged rescans are detectable.
// Sessions are sorted deterministically before any fingerprint is taken.
func fingerprint(res *Result) string {
	h := sha256.New()
	enc := json.NewEncoder(h)
	// DirMod is bookkeeping for the next walk, not content: a folder's
	// timestamp moves for things the listing does not show (a rewritten log),
	// and hashing it would make clients re-download an unchanged listing.
	bare := make([]VOD, len(res.Sessions))
	copy(bare, res.Sessions)
	for i := range bare {
		bare[i].DirMod = time.Time{}
	}
	// Encoding to a hash never fails; errors are impossible for these types.
	_ = enc.Encode(bare)
	_ = enc.Encode(res.UnavailableRoots)
	return hex.EncodeToString(h.Sum(nil))
}

// scheduleSaveLocked arranges a debounced snapshot write. The caller must hold
// ix.mu.
func (ix *Index) scheduleSaveLocked() {
	if ix.snapshotPath == "" || ix.savePending {
		return
	}
	ix.savePending = true
	time.AfterFunc(snapshotSaveDelay, ix.saveSnapshot)
}

// saveSnapshot writes the cached listing atomically (temp file + rename) so a
// crash mid-write can't corrupt the snapshot a future start would load.
func (ix *Index) saveSnapshot() {
	ix.mu.Lock()
	ix.savePending = false
	path := ix.snapshotPath
	var snap *indexSnapshot
	if path != "" && ix.cached != nil {
		res := *ix.cached
		res.Scanning = false
		snap = &indexSnapshot{Version: snapshotVersion, Result: res}
	}
	ix.mu.Unlock()
	if snap == nil {
		return
	}

	data, err := json.Marshal(snap)
	if err != nil {
		ix.logger.Warn("Failed to encode the VOD Archive snapshot", "error", err)
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		ix.logger.Warn("Failed to create the VOD Archive snapshot dir", "path", path, "error", err)
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		ix.logger.Warn("Failed to write the VOD Archive snapshot", "path", tmp, "error", err)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		ix.logger.Warn("Failed to commit the VOD Archive snapshot", "path", path, "error", err)
	}
}
