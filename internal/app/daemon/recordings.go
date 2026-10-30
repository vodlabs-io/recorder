package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/vodlabs-io/recorder/internal/webui"

	"github.com/vodlabs-io/recorder/internal/diskspace"
	"github.com/vodlabs-io/recorder/internal/processing"
	"github.com/vodlabs-io/recorder/internal/streamer"
	"github.com/vodlabs-io/recorder/internal/thumbcache"
	"github.com/vodlabs-io/recorder/internal/vlapi"
	"github.com/vodlabs-io/recorder/internal/vlconfig"
	"github.com/vodlabs-io/recorder/internal/vlstate"
	"github.com/vodlabs-io/recorder/internal/vodindex"
)

// vodsRevKey is the KV key holding the VOD Archive listing's content rev
// (vodindex.Result.Rev), rewritten whenever the contents change (a VOD
// produced, moved, or deleted) and when a rescan completes. It rides the
// clients' existing "daemon.>" KV watch, so an open VOD Archive screen re-checks
// the listing without any new subscription machinery.
const vodsRevKey = "daemon.vods_rev"

// vodIndexTTL bounds how stale a served archive scan may be before a List
// schedules a (background, non-blocking) full rescan. It's deliberately long:
// every change the daemon makes itself (a VOD produced, moved, or deleted)
// updates the index incrementally via Learn/Forget, so the TTL walk — which
// can take a minute or more on large or networked archives — only reconciles
// out-of-band changes (files deleted in a file manager, a NAS coming back
// online). Those also get picked up sooner by the walk any client refresh
// past the TTL schedules.
const vodIndexTTL = 60 * time.Minute

// initVODIndex builds the VOD Archive index, restores its persisted snapshot (so
// the VOD Archive is served instantly on a fresh start while the first reconciling
// walk runs), and hooks processing's change events to it. Call after procSvc
// and sessionReg exist.
func (a *App) initVODIndex() {
	// Screenshots are cached locally because the archive folder may be a NAS or
	// external disk whose metadata round trips cost seconds under load — enough
	// that even reading an already-generated sidecar can outlast a request.
	if tc, err := thumbcache.New(vlconfig.DefaultCacheFilePath("thumbs"), 0); err != nil {
		a.log.Warn("Failed to open thumbnail cache; screenshots will be read from the archive each time", "error", err)
	} else {
		a.thumbCache = tc
		// Processing writes a screenshot beside the video while it is still on
		// local disk. Copying it now means the common case never reads one back
		// from the archive at all.
		seed := func(sessionKey, file, localPath string) {
			if err := a.thumbCache.PutFile(sessionKey, file, localPath); err != nil {
				a.log.Debug("Failed to seed thumbnail cache", "session", sessionKey, "file", file, "error", err)
			}
		}
		a.procSvc.SetThumbnailCacher(seed)
		// A merge produces a new screenshot and retires its parts'; keep both
		// sides of that in step so the cache holds only files still listed.
		a.encSvc.SetThumbnailCachers(seed, func(sessionKey, file string) {
			if err := a.thumbCache.Delete(sessionKey, file); err != nil {
				a.log.Debug("Failed to drop cached thumbnail", "session", sessionKey, "file", file, "error", err)
			}
		})
	}

	a.vodIndex = vodindex.New(a.vodScanRoots, a.vodActiveWork, vodIndexTTL)
	a.vodIndex.SetLogger(a.log.With("component", "vodindex"))
	if err := a.vodIndex.SetSnapshotPath(vlconfig.DefaultCacheFilePath("vod-index.json")); err != nil {
		a.log.Warn("Failed to load Recording index snapshot; will rescan from scratch", "error", err)
	}
	// Content changes and scan completions announce themselves so clients
	// re-check the listing. Its own context, since it can fire long after the
	// triggering request has returned.
	a.vodIndex.SetOnChanged(func(rev int64) { a.putVODsRev(context.Background(), rev) })
	// Moves are the work that competes with a walk: both read and write the
	// archive volume. Recordings write to the download folder instead, so they
	// do not postpone a reconcile.
	a.vodIndex.SetBusyCheck(func(ctx context.Context) bool {
		for _, w := range a.vodActiveWork(ctx) {
			if w.State == vodindex.StateMoving {
				return true
			}
		}
		return false
	})
	// Processing and encoding name the session folders they touched; the
	// index re-checks just those instead of walking every root.
	vodsChanged := func(dirs ...string) {
		if len(dirs) == 0 {
			a.vodIndex.Invalidate()
			return
		}
		a.vodIndex.Learn(dirs...)
	}
	a.procSvc.SetVODsChangedNotifier(vodsChanged)
	a.encSvc.SetVODsChangedNotifier(vodsChanged)
}

// vodsRevCoalesce bounds how often the archive rev is republished. The index
// bumps its rev once per session it learns or forgets, so work that touches
// many sessions — a bulk delete, a move sweep — fires a burst of them, and
// every one makes each watching client re-download the whole listing, over the
// same connection the work itself is using.
const vodsRevCoalesce = 400 * time.Millisecond

// putVODsRev publishes the VOD Archive listing's content rev, signalling watching
// clients to re-check (and, if the rev moved, re-pull) the listing.
//
// Bursts are coalesced: the first rev goes out immediately, and any that follow
// inside the window collapse into a single publish of the newest when it
// closes. Revs are monotonic and clients only ever compare them against their
// own, so dropping the intermediate values of a burst costs a client nothing —
// the one it receives is strictly newer than the ones it did not.
func (a *App) putVODsRev(ctx context.Context, rev int64) {
	if a.natsKV == nil {
		return
	}

	a.revMu.Lock()
	if wait := vodsRevCoalesce - time.Since(a.revSentAt); wait > 0 {
		a.revPending = rev
		if a.revTimer == nil {
			// Its own context: the timer fires long after the request that
			// triggered this returned, and often after its context is cancelled.
			a.revTimer = time.AfterFunc(wait, func() { a.flushVODsRev(context.Background()) })
		}
		a.revMu.Unlock()
		return
	}
	a.revSentAt = time.Now()
	a.revMu.Unlock()

	a.publishVODsRev(ctx, rev)
}

// flushVODsRev publishes the rev a coalescing window held back.
func (a *App) flushVODsRev(ctx context.Context) {
	a.revMu.Lock()
	rev, pending := a.revPending, a.revPending != 0
	a.revPending = 0
	a.revTimer = nil
	if pending {
		a.revSentAt = time.Now()
	}
	a.revMu.Unlock()

	if pending {
		a.publishVODsRev(ctx, rev)
	}
}

func (a *App) publishVODsRev(ctx context.Context, rev int64) {
	if _, err := a.natsKV.PutString(ctx, vodsRevKey,
		strconv.FormatInt(rev, 10)); err != nil {
		a.log.Warn("Failed to publish vods rev", "error", err)
	}
}

// vodScanRoots returns every folder finished VODs can live in: the download
// folder, all currently-configured archive folders (global, per-streamer rules,
// and active one-shot overrides), and the read-only scan folders. Invalid or
// disabled destinations are skipped; unreachable ones are reported by the scan
// itself.
func (a *App) vodScanRoots() []string {
	a.cfgMu.RLock()
	downloadDir := a.appCfg.Session.DownloadDir
	a.cfgMu.RUnlock()
	roots := append([]string{downloadDir}, a.vodArchiveRoots()...)
	// Scan folders come last so that, were one ever to overlap a write
	// destination despite validation, the walk still attributes the shared
	// sessions to the folder the daemon actually writes to.
	roots = append(roots, a.vodScanOnlyRoots()...)
	// Normalize here rather than at each consumer: two Streamers configured with
	// the same archive folder must yield one Storage root, not two.
	return diskspace.Normalize(roots)
}

// vodScanOnlyRoots returns the configured read-only scan folders. They are
// deliberately absent from vodArchiveRoots: nothing is ever moved into one, so
// they are not Storage roots and must not appear in free-space reporting.
func (a *App) vodScanOnlyRoots() []string {
	a.cfgMu.RLock()
	dirs := append([]string(nil), a.appCfg.Processing.ScanDirs...)
	a.cfgMu.RUnlock()
	return vlconfig.NormalizeScanDirs(dirs)
}

// isReadOnlyVODRoot reports whether root is a scan folder. Compared against the
// normalized form the scan itself records as a VOD's root, so the match is
// exact rather than a prefix test.
func (a *App) isReadOnlyVODRoot(root string) bool {
	root = filepath.Clean(root)
	for _, d := range a.vodScanOnlyRoots() {
		if d == root {
			return true
		}
	}
	return false
}

// vodArchiveRoots returns just the configured archive folders — global,
// per-streamer rules, and active one-shot overrides — without the download
// folder. Kept separate from vodScanRoots because callers that need the two
// roles apart must not have to guess which entry is which after normalization
// has deduplicated them.
func (a *App) vodArchiveRoots() []string {
	a.cfgMu.RLock()
	dests := []string{a.appCfg.Processing.ArchiveDir}
	a.cfgMu.RUnlock()

	if a.strFile != nil {
		for _, id := range a.strFile.AllIDs() {
			if opts := a.strFile.OptionsFor(id); opts.ArchiveDir != "" {
				dests = append(dests, opts.ArchiveDir)
			}
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, id := range vlstate.GetOneShotList(ctx, a.natsKV) {
		if opts, ok := vlstate.GetOneShotOptions(ctx, a.natsKV, id.StreamerKey); ok && opts.ArchiveDir != "" {
			dests = append(dests, opts.ArchiveDir)
		}
	}

	var roots []string
	for _, d := range dests {
		d = strings.TrimSpace(d)
		if d == "" || vlconfig.IsArchiveOff(d) || vlconfig.ValidateArchiveDir(d) != nil {
			continue
		}
		roots = append(roots, d)
	}
	return diskspace.Normalize(roots)
}

// vodActiveWork combines processing's and encoding's in-flight work with the
// folders of sessions still being recorded (suppressed from the VOD Archive
// entirely). Processing/moving wins over an encode badge when both queues
// touch the same folder.
func (a *App) vodActiveWork(ctx context.Context) []vodindex.ActiveWork {
	var work []vodindex.ActiveWork
	if a.procSvc != nil {
		work = a.procSvc.ActiveVODWork(ctx)
	}
	if a.encSvc != nil {
		seen := make(map[string]bool, len(work))
		for _, w := range work {
			seen[w.BaseOutDir] = true
		}
		for _, w := range a.encSvc.ActiveVODWork(ctx) {
			if !seen[w.BaseOutDir] {
				work = append(work, w)
			}
		}
	}
	if a.sessionReg != nil {
		for _, dir := range a.sessionReg.ActiveSessionDirs() {
			work = append(work, vodindex.ActiveWork{
				BaseOutDir: dir,
				State:      vodindex.StateRecording,
			})
		}
	}
	return work
}

// ListVODs implements vlapi.VODController.
func (a *App) ListVODs(ctx context.Context) vodindex.Result {
	return a.vodIndex.List(ctx)
}

// RescanVODs implements vlapi.VODController: it force-expires the cached scan
// so the next List reconciles against the filesystem. Concurrent scans
// coalesce, so a rapid double-click can't stack walks. This is the one walk
// that runs even while the archive is busy — it is the user asking.
func (a *App) RescanVODs(ctx context.Context) {
	a.vodIndex.ForceRescan()
}

// VODThumbnail implements vlapi.VODController: it returns the JPEG screenshot
// for one file of a listed session, generating and caching it on first request
// when processing left none. Only files the scan actually listed are touched,
// so the endpoint can't read arbitrary paths. An empty file asks for any
// screenshot of a session still being processed or moved.
func (a *App) VODThumbnail(ctx context.Context, dir, file string) ([]byte, error) {
	session, ok := a.vodIndex.Find(ctx, dir)
	if !ok {
		return nil, fmt.Errorf("%w: not an archived session: %s", vlapi.ErrNotFound, dir)
	}
	found := false
	for _, f := range session.Files {
		if f.Name == file {
			found = true
			break
		}
	}
	if file != "" && !found {
		return nil, fmt.Errorf("%w: no such file in session: %s", vlapi.ErrNotFound, file)
	}
	if session.State != vodindex.StateReady {
		return a.busyVODThumbnail(ctx, session, file)
	}
	if file == "" {
		return nil, fmt.Errorf("%w: no file named for a finished session", vlapi.ErrNotFound)
	}

	// Local cache first: everything below this point touches the archive
	// volume, which is exactly what the cache exists to avoid.
	key := thumbCacheKey(session)
	if jpeg, ok := a.thumbCache.Get(key, file); ok {
		return a.upgradeThumbnail(ctx, dir, file, jpeg), nil
	}

	thumbPath := filepath.Join(session.Dir, processing.ThumbnailName(file))
	if _, err := os.Stat(thumbPath); os.IsNotExist(err) {
		videoPath := filepath.Join(session.Dir, file)
		if _, err := os.Stat(videoPath); err != nil {
			return nil, fmt.Errorf("%w: video file missing: %s", vlapi.ErrNotFound, file)
		}
		// A scan folder may be a genuinely read-only mount, and is promised not
		// to be written to either way, so its thumbnails are generated into a
		// temp file and kept only in the local cache. The cost is regenerating
		// after an eviction, against never scattering sidecars into a folder the
		// user asked us only to read.
		if a.isReadOnlyVODRoot(session.Root) {
			jpeg, err := a.thumbnailWithoutSidecar(ctx, key, file, videoPath)
			if err != nil {
				return nil, err
			}
			return a.upgradeThumbnail(ctx, dir, file, jpeg), nil
		}
		if err := processing.GenerateThumbnail(ctx, a.log.With("service", "recordings"), videoPath, thumbPath); err != nil {
			return nil, err
		}
	}
	jpeg, err := os.ReadFile(thumbPath)
	if err != nil {
		return nil, err
	}
	if err := a.thumbCache.Put(key, file, jpeg); err != nil {
		a.log.Debug("Failed to cache thumbnail", "session", key, "file", file, "error", err)
	}
	return a.upgradeThumbnail(ctx, dir, file, jpeg), nil
}

// upgradeThumbnail turns a portrait screenshot into the three-up thumbnail
// that previews generation writes, generating previews on this first view.
// Any failure keeps the screenshot it was given.
func (a *App) upgradeThumbnail(ctx context.Context, dir, file string, jpeg []byte) []byte {
	if !portraitJPEG(jpeg) {
		return jpeg
	}
	_, out, err := a.ensurePreviews(ctx, dir, file)
	if err != nil {
		a.log.Debug("Could not upgrade portrait thumbnail", "dir", dir, "file", file, "error", err)
		return jpeg
	}
	if b, err := os.ReadFile(filepath.Join(out, processing.ThumbnailName(file))); err == nil && completeJPEG(b) && !portraitJPEG(b) {
		return b
	}
	return jpeg
}

// thumbnailWithoutSidecar generates one thumbnail for a VOD on a read-only scan
// folder. The JPEG goes to a temp file, into the local cache, and back to the
// caller; nothing is left in the session folder.
func (a *App) thumbnailWithoutSidecar(ctx context.Context, key, file, videoPath string) ([]byte, error) {
	jpeg, err := a.grabThumbnail(ctx, videoPath)
	if err != nil {
		return nil, err
	}
	if err := a.thumbCache.Put(key, file, jpeg); err != nil {
		a.log.Debug("Failed to cache thumbnail", "session", key, "file", file, "error", err)
	}
	return jpeg, nil
}

// grabThumbnail generates a screenshot of videoPath through a temp file and
// returns its bytes.
func (a *App) grabThumbnail(ctx context.Context, videoPath string) ([]byte, error) {
	tmp, err := os.CreateTemp("", "vlrec-thumb-*.jpg")
	if err != nil {
		return nil, err
	}
	tmpPath := tmp.Name()
	tmp.Close()
	defer os.Remove(tmpPath)

	if err := processing.GenerateThumbnail(ctx, a.log.With("service", "recordings"), videoPath, tmpPath); err != nil {
		return nil, err
	}
	return os.ReadFile(tmpPath)
}

// Bounds for screenshots of a busy session's media. Each attempt is capped
// because a Part left unterminated by a crash reads as a live playlist, which
// ffmpeg waits on; the source count is capped so an ffmpeg that cannot write
// JPEGs at all fails fast rather than once per Part.
const (
	busyThumbAttemptTimeout = 15 * time.Second
	busyThumbMaxSources     = 3
)

// busyVODThumbnail returns a screenshot for a session the queue is still
// processing or moving. The file asked for may not exist yet or may already
// have moved, so any screenshot of the session will do: a cached or sidecar
// one first, then a frame from whatever media the folder still holds. Nothing
// is written into the folder or the cache while the queue is working there, so
// processing's own screenshot is the one that sticks.
func (a *App) busyVODThumbnail(ctx context.Context, session vodindex.VOD, file string) ([]byte, error) {
	key := thumbCacheKey(session)
	names := make([]string, 0, len(session.Files)+1)
	if file != "" {
		names = append(names, file)
	}
	for _, f := range session.Files {
		if f.Name != file {
			names = append(names, f.Name)
		}
	}
	for _, name := range names {
		if jpeg, ok := a.thumbCache.Get(key, name); ok {
			return jpeg, nil
		}
		// A sidecar processing is still writing is skipped until it is whole.
		jpeg, err := os.ReadFile(filepath.Join(session.Dir, processing.ThumbnailName(name)))
		if err == nil && completeJPEG(jpeg) {
			return jpeg, nil
		}
	}

	err := fmt.Errorf("%w: no local media to take a screenshot from", vlapi.ErrNotFound)
	for _, src := range busyThumbSources(session.Dir, names) {
		attemptCtx, cancel := context.WithTimeout(ctx, busyThumbAttemptTimeout)
		jpeg, grabErr := a.grabThumbnail(attemptCtx, src)
		cancel()
		if grabErr == nil {
			return jpeg, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		err = grabErr
	}
	return nil, err
}

// busyThumbSources lists the local media a busy session's screenshot can come
// from, best first: its produced files (one still being written fails and is
// passed over), then its raw Parts — FLV and WebM captures, HLS playlists, then
// converted intermediates, which may be half written — in Part order.
func busyThumbSources(dir string, names []string) []string {
	var sources []string
	for _, name := range names {
		if info, err := os.Stat(filepath.Join(dir, name)); err == nil && info.Mode().IsRegular() && info.Size() > 0 {
			sources = append(sources, filepath.Join(dir, name))
		}
	}

	entries, err := os.ReadDir(dir)
	if err == nil {
		var raw, converted []string
		for _, e := range entries {
			name := e.Name()
			if !strings.HasPrefix(name, "pt") {
				continue
			}
			p := filepath.Join(dir, name)
			switch {
			case e.IsDir():
				if _, err := os.Stat(filepath.Join(p, "index.m3u8")); err == nil {
					raw = append(raw, filepath.Join(p, "index.m3u8"))
				}
			case strings.HasSuffix(name, ".ts"), strings.HasSuffix(name, ".webm"):
				raw = append(raw, p)
			case strings.HasSuffix(name, ".mp4"):
				converted = append(converted, p)
			}
		}
		byPart := func(s []string) {
			sort.SliceStable(s, func(i, j int) bool { return partIndex(s[i], dir) < partIndex(s[j], dir) })
		}
		byPart(raw)
		byPart(converted)
		sources = append(sources, raw...)
		sources = append(sources, converted...)
	}

	if len(sources) > busyThumbMaxSources {
		sources = sources[:busyThumbMaxSources]
	}
	return sources
}

// partIndex reads N from a Part path under dir named ptN_<quality>…; paths
// that don't parse sort last.
func partIndex(p, dir string) int {
	rel, err := filepath.Rel(dir, p)
	if err != nil {
		return int(^uint(0) >> 1)
	}
	name := strings.SplitN(filepath.ToSlash(rel), "/", 2)[0]
	digits := strings.TrimPrefix(name, "pt")
	if i := strings.IndexFunc(digits, func(r rune) bool { return r < '0' || r > '9' }); i >= 0 {
		digits = digits[:i]
	}
	n, err := strconv.Atoi(digits)
	if err != nil {
		return int(^uint(0) >> 1)
	}
	return n
}

// completeJPEG reports whether b is a whole JPEG: it starts with SOI and ends
// with EOI.
func completeJPEG(b []byte) bool {
	return len(b) > 4 && b[0] == 0xFF && b[1] == 0xD8 && b[len(b)-2] == 0xFF && b[len(b)-1] == 0xD9
}

// thumbCacheKey identifies a session for the thumbnail cache. The session ID is
// preferred because a session's folder changes when it moves to the archive,
// and an entry seeded before the move must still be found after it. Sessions
// without a manifest have no ID; those fall back to the folder and simply
// re-cache once, under the new path, after a move.
func thumbCacheKey(s vodindex.VOD) string {
	if s.SessionID != "" {
		return s.SessionID
	}
	return s.Dir
}

// vodTrashDirName is the per-root folder a session is moved into before it is
// erased. It is dot-prefixed so the scanner never descends into it or mistakes
// its contents for sessions (see vodindex.readDirContents).
const vodTrashDirName = ".vlrec-trash"

// CancelMoves retires queued moves so they stop retrying, for one streamer or
// for the whole queue. Implements vlapi.VODController.
func (a *App) CancelMoves(ctx context.Context, key *streamer.StreamerKey) (int, error) {
	if a.procSvc == nil {
		return 0, fmt.Errorf("%w: processing service unavailable", vlapi.ErrConflict)
	}
	return a.procSvc.CancelMoves(ctx, key)
}

// DeleteVOD implements vlapi.VODController: it removes a listed session's
// entire folder (all parts, logs, sidecars), then prunes emptied parent folders
// up to, but never including, the scan root.
//
// The folder is first renamed into the root's trash, because os.RemoveAll
// deletes as it walks and a refusal partway would leave a gutted session. The
// atomic rename is the commit point: before it a failure leaves the session
// whole, after it the session is gone whatever the erase does.
func (a *App) DeleteVOD(ctx context.Context, dir string) error {
	session, ok := a.vodIndex.Find(ctx, dir)
	if !ok {
		return fmt.Errorf("%w: not an archived session: %s", vlapi.ErrNotFound, dir)
	}
	if session.State != vodindex.StateReady ||
		(a.procSvc != nil && a.procSvc.SessionBusy(ctx, session.Dir)) ||
		(a.encSvc != nil && a.encSvc.SessionBusy(ctx, session.Dir)) {
		return fmt.Errorf("%w: session is still being processed, moved, or encoded", vlapi.ErrConflict)
	}

	root := filepath.Clean(session.Root)
	doomed, err := a.trashVOD(root, session.Dir)
	if err != nil {
		return fmt.Errorf("delete session folder: %w", err)
	}

	// Past the commit point: the session no longer exists at a path the archive
	// lists, so it is reported deleted even if erasing what is left fails. The
	// bytes it still holds are freed by the next sweep instead.
	if err := os.RemoveAll(doomed); err != nil {
		a.log.Warn("Deleted Recording from the archive but could not erase its files; "+
			"they will be retried on the next start",
			"dir", session.Dir, "pending", doomed, "error", err)
	} else {
		for parent := filepath.Dir(session.Dir); len(parent) > len(root) && parent != root; parent = filepath.Dir(parent) {
			if err := os.Remove(parent); err != nil {
				break // non-empty (or gone): nothing above it can be empty either
			}
		}
		// Take the trash with it, so a delete leaves no folder of ours behind
		// in an archive the user also browses. Refused while another delete is
		// mid-flight, which is the next one's problem to tidy.
		_ = os.Remove(filepath.Join(root, vodTrashDirName))
	}
	a.log.Info("Deleted Recording session folder", "dir", session.Dir,
		"platform", session.Platform, "username", session.Username)

	// Drop just this session from the cache so the next List is instantly
	// correct without a full (slow) rescan; the TTL refresh reconciles later.
	// Forget announces the change (rev bump) to watching clients itself.
	a.vodIndex.Forget(session.Dir)
	return nil
}

// trashVOD moves a session folder to its root's trash and returns where it
// landed, so the erase that follows cannot leave a half-deleted session at a
// path the archive still lists.
func (a *App) trashVOD(root, dir string) (string, error) {
	trash := filepath.Join(root, vodTrashDirName)
	if err := os.MkdirAll(trash, 0o755); err != nil {
		return "", err
	}
	doomed := filepath.Join(trash, fmt.Sprintf("%d-%s", time.Now().UnixNano(), filepath.Base(dir)))
	err := os.Rename(dir, doomed)
	if err == nil {
		return doomed, nil
	}
	// A rename cannot cross a mount point, and no atomic delete exists there;
	// erase in place rather than refuse the delete.
	if errors.Is(err, syscall.EXDEV) {
		return dir, nil
	}
	return "", err
}

// sweepVODTrash erases whatever an interrupted delete left behind, on every
// scan root. One ReadDir per root: the trash holds only folders already removed
// from the archive, so nothing here needs deciding.
func (a *App) sweepVODTrash() {
	for _, root := range a.vodScanRoots() {
		trash := filepath.Join(root, vodTrashDirName)
		entries, err := os.ReadDir(trash)
		if err != nil {
			continue // no trash on this root, or it is unreachable
		}
		for _, e := range entries {
			path := filepath.Join(trash, e.Name())
			if err := os.RemoveAll(path); err != nil {
				a.log.Warn("Failed to erase a pending deleted Recording", "path", path, "error", err)
			}
		}
		// Leave the trash folder itself only while it still holds something.
		_ = os.Remove(trash)
	}
}

// Compile-time check that App satisfies the API's controller contract.
var _ vlapi.VODController = (*App)(nil)

// OpenVODFile implements webui.FileOpener: it opens one file of a finished,
// listed session for the Web UI to play or download. Like VODThumbnail it
// only touches files the scan listed, and it opens them inside the session
// folder, so neither a crafted name nor a symlink reaches anything else.
func (a *App) OpenVODFile(ctx context.Context, dir, file string) (*os.File, error) {
	if !a.vodReady.Load() {
		return nil, webui.ErrNotReady
	}
	if file == "" || file != filepath.Base(file) || strings.ContainsAny(file, `/\`) ||
		file == "." || file == ".." {
		return nil, webui.ErrNotFound
	}
	session, ok := a.vodIndex.Find(ctx, dir)
	if !ok || session.State != vodindex.StateReady {
		return nil, webui.ErrNotFound
	}
	listed := false
	for _, f := range session.Files {
		if f.Name == file {
			listed = true
			break
		}
	}
	if !listed {
		return nil, webui.ErrNotFound
	}
	f, err := os.OpenInRoot(session.Dir, file)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", webui.ErrNotFound, err)
	}
	return f, nil
}
