// Package vodindex builds the VOD Archive: it scans the Download, Archive and
// Scan folders for finished session folders (vod.mp4 / vodN.mp4), preferring
// the vod.json manifest written at processing time and falling back to a
// best-effort parse of the folder layout without one. The filesystem is the
// source of truth, so manual deletes and imports are always reflected.
//
// Not everything it lists is strictly a VOD: a session whose processing is
// still running is a Recording, and is listed in the "processing" state so a
// just-ended session never goes invisible. Those entries become VODs in place.
package vodindex

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/vodlabs-io/recorder/internal/diskspace"
	"github.com/vodlabs-io/recorder/internal/recording"
	"github.com/vodlabs-io/recorder/internal/vodmeta"
)

// Session states surfaced to clients. Ready sessions are playable; processing
// and moving sessions exist on disk but are still being worked on. Recording
// is never surfaced: sessions still being recorded are suppressed from the
// archive entirely (live activity is the dashboard's job).
const (
	StateReady      = "ready"
	StateProcessing = "processing"
	StateMoving     = "moving"
	StateEncoding   = "encoding"
	StateRecording  = "recording"
)

// maxScanDepth bounds the recursive walk below each root. The default output
// template is 3 levels ({platform}/{username}/{datetime}); custom templates go
// deeper, but nothing sensible exceeds this.
const maxScanDepth = 6

// File is one produced output file of a session (vod.mp4 or vodN.mp4).
type File struct {
	Name        string    `json:"name"`
	Bytes       int64     `json:"bytes"`
	Start       time.Time `json:"start,omitzero"`
	End         time.Time `json:"end,omitzero"`
	DurationSec float64   `json:"duration_sec,omitempty"`
	HasThumb    bool      `json:"has_thumb"`
}

// VOD is one recorded session folder, grouping every split file it holds.
type VOD struct {
	Dir  string `json:"dir"`  // absolute session folder path
	Root string `json:"root"` // scan root the folder was found under
	// DirMod is the session folder's modification time, the key a later scan
	// uses to decide it can reuse this entry instead of reading the folder
	// again. Zero when it could not be read, which never matches and so always
	// forces a re-read.
	DirMod      time.Time `json:"dir_mod,omitzero"`
	Platform    string    `json:"platform"`
	Username    string    `json:"username"`
	SessionID   string    `json:"session_id"`
	Start       time.Time `json:"start,omitzero"`
	End         time.Time `json:"end,omitzero"`
	TotalBytes  int64     `json:"total_bytes"`
	DurationSec float64   `json:"duration_sec"`
	State       string    `json:"state"`
	// Reason is why a session in the moving state is not progressing. Empty when
	// the move is simply running.
	Reason       string `json:"reason,omitempty"`
	FromManifest bool   `json:"from_manifest"`
	Files        []File `json:"files"`
	// Titles are the stream titles the platform reported while the session was
	// recording, oldest first — a long broadcast is usually retitled more than
	// once. Empty for a session whose folder has no metadata sidecar, and for
	// platforms that report no title at all.
	//
	// This is a summary: the sidecar holds the full readings (category, tags,
	// viewer counts, whatever else the platform said), and is read from disk by
	// whoever wants them. Only the titles are carried in the listing, which is
	// paged by byte size.
	Titles []Title `json:"titles,omitempty"`
}

// Title is one stream title a session ran under, with when the recorder first
// saw it.
type Title struct {
	Text string    `json:"text"`
	At   time.Time `json:"at,omitzero"`
}

// maxListedTitles bounds how many titles one session contributes to the
// listing. A stream retitled past this is unusual, and the sidecar on disk
// still holds every one of them.
const maxListedTitles = 50

// ActiveWork describes a session the processing service is still working on
// (post-recording): concat/finalize in flight, or files moving to a
// destination. The scanner uses it to badge sessions and to suppress the
// half-populated destination folder of an in-flight move.
type ActiveWork struct {
	BaseOutDir string
	Dest       string // archive folder root ("" when not moving)
	RelDir     string // session dir relative to Dest
	Platform   string
	Username   string
	SessionID  string
	State      string // StateProcessing or StateMoving
	// Reason says why a move is not progressing (no_space, unreachable, …), so
	// the archive can show a stuck VOD's cause instead of an endless "Moving…".
	// Empty while a move is simply in progress.
	Reason string
}

// Result is one scan of every root.
type Result struct {
	Sessions         []VOD     `json:"sessions"`
	UnavailableRoots []string  `json:"unavailable_roots"`
	ScannedAt        time.Time `json:"scanned_at"`
	// Rev identifies this listing's content generation: it increases whenever
	// the sessions or unavailable roots actually change (scan, Learn, Forget)
	// and is otherwise stable across rescans and daemon restarts, so clients
	// can skip re-downloading an unchanged archive.
	Rev int64 `json:"rev"`
	// Scanning is true when a background rescan is in flight and this listing
	// may be stale or (before the first scan finishes) empty. Clients show a
	// "scanning" state instead of "no recordings" so a cold archive doesn't look
	// empty while the first scan runs.
	Scanning bool `json:"scanning"`
}

// vodFileRe matches produced output files: vod.mp4 (sole group) or vodN.mp4,
// or the same as .webm for a VP8 recording.
var vodFileRe = regexp.MustCompile(`^vod(\d*)\.(mp4|webm)$`)

// datetimeFormat is the {datetime} output-template token's layout; session
// folders without a manifest commonly use it as the folder name, so the
// scanner tries it to recover the session start time.
const datetimeFormat = "2006-01-02_15-04-05-0700"

// overlay is the scanner's view of in-flight work: session folders to suppress
// entirely (still recording, or the half-copied destination side of a move),
// and post-recording work (processing/moving) keyed by session folder for
// state badges and placeholder listings.
type overlay struct {
	suppressed map[string]bool
	work       map[string]ActiveWork
}

// buildOverlay indexes active work by cleaned session folder.
//
// A moving session exists in two places at once (local files not yet swept,
// destination files already copied): only the local, authoritative folder is
// listed; the destination side is suppressed until the sweep completes. A
// still-recording session's folder (which may already hold vodN.mp4 from
// mid-stream splits) stays out of the VOD Archive until it ends.
func buildOverlay(active []ActiveWork) overlay {
	o := overlay{suppressed: make(map[string]bool), work: make(map[string]ActiveWork)}
	for _, w := range active {
		dir := filepath.Clean(w.BaseOutDir)
		if w.State == StateRecording {
			o.suppressed[dir] = true
			continue
		}
		w.BaseOutDir = dir
		o.work[dir] = w
		if w.State == StateMoving && w.Dest != "" && w.RelDir != "" {
			o.suppressed[filepath.Clean(filepath.Join(w.Dest, w.RelDir))] = true
		}
	}
	return o
}

// Scan walks every root and returns the sessions found. Roots that can't be
// read are reported in UnavailableRoots rather than failing the scan (a NAS
// archive folder may simply be offline). active badges in-flight sessions
// and suppresses their half-copied destination folders.
//
// Reachability is probed under a timeout: an unreachable network drive must be
// reported as unavailable, not stall the whole scan waiting for it.
func Scan(ctx context.Context, roots []string, active []ActiveWork) Result {
	return ScanReusing(ctx, roots, active, nil)
}

// ScanReusing is Scan with the previous scan's sessions available for reuse.
// A folder that the last walk listed as a session, whose modification time has
// not moved since, is carried over on the strength of one stat instead of
// being read again — on a networked archive that is the difference between
// milliseconds and a tenth of a second per session, over thousands of them.
//
// Only folders the previous walk actually listed are eligible, so anything new
// is still discovered by reading it. Sessions with work in flight are never
// reused: a file being appended to does not change its folder's modification
// time, so their sizes and durations must come from the filesystem.
func ScanReusing(ctx context.Context, roots []string, active []ActiveWork, prev map[string]VOD) Result {
	res := Result{ScannedAt: time.Now()}
	o := buildOverlay(active)

	// A session the queue is working on must be read, whatever its folder's
	// timestamp says.
	reuse := prev
	if len(prev) > 0 && len(active) > 0 {
		reuse = make(map[string]VOD, len(prev))
		for dir, v := range prev {
			reuse[dir] = v
		}
		for _, w := range active {
			delete(reuse, filepath.Clean(w.BaseOutDir))
			if w.Dest != "" && w.RelDir != "" {
				delete(reuse, filepath.Clean(filepath.Join(w.Dest, w.RelDir)))
			}
		}
	}

	seenDir := make(map[string]bool)
	for _, root := range diskspace.Normalize(roots) {
		if err := diskspace.Reachable(ctx, root, diskspace.DefaultProbeTimeout); err != nil {
			res.UnavailableRoots = append(res.UnavailableRoots, root)
			continue
		}
		scanDir(root, root, 0, o.suppressed, seenDir, reuse, &res.Sessions)
	}

	// Sessions still processing may not have produced any output file yet; make
	// sure they appear (with what the queue knows about them) so a just-ended
	// recording is never invisible.
	for _, w := range active {
		dir := filepath.Clean(w.BaseOutDir)
		if w.State == StateRecording || seenDir[dir] {
			continue
		}
		if _, err := os.Stat(dir); err != nil {
			continue
		}
		seenDir[dir] = true
		res.Sessions = append(res.Sessions, VOD{
			Dir:       dir,
			Platform:  w.Platform,
			Username:  w.Username,
			SessionID: w.SessionID,
			State:     w.State,
			Reason:    w.Reason,
		})
	}

	// State and Reason travel together: a moving session whose reason is dropped
	// here reads as an ordinary in-progress move forever, which is the symptom
	// the reason exists to remove.
	for i := range res.Sessions {
		if w, ok := o.work[res.Sessions[i].Dir]; ok {
			res.Sessions[i].State = w.State
			res.Sessions[i].Reason = w.Reason
		} else if res.Sessions[i].State == "" {
			res.Sessions[i].State = StateReady
		}
	}

	sortSessions(res.Sessions)
	return res
}

// scanOne rescans a single session folder and reports whether it currently
// holds a listable session. It is the targeted counterpart of Scan for changes
// whose folder the daemon already knows (a recording produced, a move completed):
// Index.Learn merges its result into the cached listing without walking every
// root. ok=false means dir should not be listed — folder gone or empty,
// suppressed as in-flight work, or outside every scan root — exactly the cases
// where a full Scan would not list it either.
func scanOne(roots []string, dir string, active []ActiveWork) (VOD, bool) {
	dir = filepath.Clean(dir)
	o := buildOverlay(active)
	if o.suppressed[dir] {
		return VOD{}, false
	}
	root, ok := rootOf(roots, dir)
	if !ok {
		return VOD{}, false
	}

	c, err := readDirContents(dir)
	if err != nil {
		return VOD{}, false
	}
	if !c.hasManifest && len(c.vodFiles) == 0 {
		// Not (or no longer) a session folder by contents. Still list the
		// placeholder Scan would when the queue is working on it (a just-ended
		// recording with no output produced yet is never invisible).
		if w, ok := o.work[dir]; ok {
			return VOD{
				Dir:       dir,
				Platform:  w.Platform,
				Username:  w.Username,
				SessionID: w.SessionID,
				State:     w.State,
				Reason:    w.Reason,
			}, true
		}
		return VOD{}, false
	}

	s := buildSession(root, dir, c)
	if w, ok := o.work[dir]; ok {
		s.State = w.State
		s.Reason = w.Reason
	} else {
		s.State = StateReady
	}
	return s, true
}

// rootOf resolves which scan root contains dir (the deepest match, mirroring
// how the walk would reach it) and enforces Scan's depth bound so a Learn
// never lists a folder a full walk wouldn't.
func rootOf(roots []string, dir string) (string, bool) {
	best := ""
	for _, root := range diskspace.Normalize(roots) {
		if dir != root && !strings.HasPrefix(dir, root+string(filepath.Separator)) {
			continue
		}
		if len(root) > len(best) {
			best = root
		}
	}
	if best == "" {
		return "", false
	}
	if rel, err := filepath.Rel(best, dir); err != nil ||
		(rel != "." && strings.Count(rel, string(filepath.Separator))+1 > maxScanDepth) {
		return "", false
	}
	return best, true
}

// sortSessions orders newest first with a stable tiebreak on path for
// deterministic output. Order by start, falling back to end so a session whose
// manifest records only `end` still sorts by when it happened.
func sortSessions(sessions []VOD) {
	sort.Slice(sessions, func(i, j int) bool {
		ta, tb := sessionTime(sessions[i]), sessionTime(sessions[j])
		if !ta.Equal(tb) {
			return ta.After(tb)
		}
		return sessions[i].Dir < sessions[j].Dir
	})
}

// sessionTime is the timestamp a session is ordered by: its start, falling back
// to its end when start is unknown (a manifest without one, or the filesystem
// fallback where only file mtimes are available).
func sessionTime(s VOD) time.Time {
	if !s.Start.IsZero() {
		return s.Start
	}
	return s.End
}

// dirContents classifies one folder's entries for the session heuristic.
type dirContents struct {
	hasManifest bool
	hasMetadata bool
	vodFiles    []os.DirEntry
	thumbs      map[string]bool
	subdirs     []string
}

// readDirContents reads dir and sorts its entries into the parts the session
// heuristic cares about: a manifest, produced vod*.mp4 files, their thumbnails,
// and descendable subfolders (pt* raw part dirs and hidden dirs are never
// sessions).
func readDirContents(dir string) (dirContents, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return dirContents{}, err
	}
	c := dirContents{thumbs: make(map[string]bool)}
	for _, e := range entries {
		name := e.Name()
		switch {
		case e.IsDir():
			if !strings.HasPrefix(name, "pt") && !strings.HasPrefix(name, ".") {
				c.subdirs = append(c.subdirs, name)
			}
		case name == recording.ManifestName:
			c.hasManifest = true
		case name == vodmeta.FileName:
			// Noted but not a session marker on its own: the sidecar is written
			// as soon as a recording starts, long before it has produced
			// anything to list.
			c.hasMetadata = true
		case vodFileRe.MatchString(name):
			c.vodFiles = append(c.vodFiles, e)
		case strings.HasSuffix(name, ".jpg"):
			c.thumbs[strings.TrimSuffix(name, ".jpg")] = true
		}
	}
	return c, nil
}

// scanDir recursively looks for session folders under dir. A folder containing
// a manifest or any vod*.mp4 is a session folder; its subtree is not descended
// further (raw pt* part dirs live inside it).
func scanDir(root, dir string, depth int, suppressed, seenDir map[string]bool, reuse map[string]VOD, out *[]VOD) {
	if depth > maxScanDepth || suppressed[dir] || seenDir[dir] {
		return
	}
	// The previous walk already established this folder is a session; if
	// nothing has been added to or removed from it since, its listing still
	// holds. One stat replaces a read of the folder, its manifest and every
	// file in it.
	if prev, ok := reuse[dir]; ok && !prev.DirMod.IsZero() {
		if info, err := os.Stat(dir); err == nil && info.ModTime().Equal(prev.DirMod) {
			seenDir[dir] = true
			*out = append(*out, prev)
			return
		}
	}
	c, err := readDirContents(dir)
	if err != nil {
		return
	}
	if c.hasManifest || len(c.vodFiles) > 0 {
		seenDir[dir] = true
		*out = append(*out, buildSession(root, dir, c))
		return
	}
	for _, sub := range c.subdirs {
		scanDir(root, filepath.Join(dir, sub), depth+1, suppressed, seenDir, reuse, out)
	}
}

// buildSession assembles a VOD from a folder's contents, preferring the
// manifest and falling back to the folder layout ({platform}/{username}/
// {datetime}) plus file stats for folders without one.
func buildSession(root, dir string, c dirContents) VOD {
	hasManifest, vodFiles, thumbs := c.hasManifest, c.vodFiles, c.thumbs
	s := VOD{Dir: dir, Root: root}
	if c.hasMetadata {
		s.Titles = readTitles(dir)
	}
	// Stamp the folder so a later walk can decide to reuse this entry. Left
	// zero on error, which simply means it will be read again.
	if info, err := os.Stat(dir); err == nil {
		s.DirMod = info.ModTime()
	}

	// Index the files actually on disk.
	type diskFile struct {
		bytes   int64
		modTime time.Time
	}
	onDisk := make(map[string]diskFile, len(vodFiles))
	for _, e := range vodFiles {
		df := diskFile{}
		if info, err := e.Info(); err == nil {
			df.bytes = info.Size()
			df.modTime = info.ModTime()
		}
		onDisk[e.Name()] = df
	}

	if hasManifest {
		if m, err := recording.ReadManifest(dir); err == nil {
			s.FromManifest = true
			s.Platform = m.Platform
			s.Username = m.Username
			s.SessionID = m.SessionID
			s.Start = m.Start
			s.End = m.End
			s.TotalBytes = m.TotalBytes
			s.DurationSec = m.DurationSec

			// Start from the manifest's file list (files mid-move may not be on
			// disk here yet) and fold in any extra vod*.mp4 present on disk.
			listed := make(map[string]bool, len(m.Files))
			for _, mf := range m.Files {
				listed[mf.Name] = true
				f := File{
					Name:        mf.Name,
					Bytes:       mf.Bytes,
					Start:       mf.Start,
					End:         mf.End,
					DurationSec: mf.DurationSec,
					HasThumb:    thumbs[trimExt(mf.Name)],
				}
				if df, ok := onDisk[mf.Name]; ok && df.bytes > 0 {
					f.Bytes = df.bytes
				}
				s.Files = append(s.Files, f)
			}
			for name, df := range onDisk {
				if !listed[name] {
					s.Files = append(s.Files, File{
						Name:     name,
						Bytes:    df.bytes,
						HasThumb: thumbs[trimExt(name)],
					})
				}
			}
			sortFiles(s.Files)
			return s
		}
	}

	// No (readable) manifest: recover what the layout offers.
	// The default template nests sessions as {platform}/{username}/{datetime},
	// so the parent is the username and the grandparent the platform.
	if rel, err := filepath.Rel(root, dir); err == nil && rel != "." {
		comps := strings.Split(rel, string(filepath.Separator))
		if len(comps) >= 2 {
			s.Username = comps[len(comps)-2]
		}
		if len(comps) >= 3 {
			s.Platform = comps[len(comps)-3]
		}
	}
	if t, err := time.ParseInLocation(datetimeFormat, filepath.Base(dir), time.Local); err == nil {
		s.Start = t
	}

	for name, df := range onDisk {
		s.Files = append(s.Files, File{
			Name:     name,
			Bytes:    df.bytes,
			HasThumb: thumbs[trimExt(name)],
		})
		s.TotalBytes += df.bytes
		if df.modTime.After(s.End) {
			s.End = df.modTime
		}
	}
	if s.Start.IsZero() {
		// Last resort: the earliest output file's mtime approximates the
		// session end, which still beats no date at all for filtering.
		for _, df := range onDisk {
			if !df.modTime.IsZero() && (s.Start.IsZero() || df.modTime.Before(s.Start)) {
				s.Start = df.modTime
			}
		}
	}
	sortFiles(s.Files)
	return s
}

// sortFiles orders split files numerically: vod.mp4, vod1.mp4, vod2.mp4, …
// vod10.mp4 (lexical order would put vod10 before vod2).
func sortFiles(files []File) {
	sort.Slice(files, func(i, j int) bool {
		return vodFileNum(files[i].Name) < vodFileNum(files[j].Name)
	})
}

// vodFileNum extracts the split index of a vodN.mp4 or vodN.webm name; the
// undecorated vod.mp4 or vod.webm sorts first, unmatched names last.
func vodFileNum(name string) int {
	m := vodFileRe.FindStringSubmatch(name)
	if m == nil {
		return 1 << 30
	}
	if m[1] == "" {
		return 0
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return 1 << 30
	}
	return n
}

func trimExt(name string) string {
	return strings.TrimSuffix(name, filepath.Ext(name))
}

// readTitles lifts the title timeline out of a session's metadata sidecar. A
// missing or unreadable sidecar yields nothing: the titles decorate a listing
// that stands on its own without them.
func readTitles(dir string) []Title {
	doc, err := vodmeta.Read(dir)
	if err != nil {
		return nil
	}
	seen := doc.Titles()
	if len(seen) == 0 {
		return nil
	}
	if len(seen) > maxListedTitles {
		seen = seen[:maxListedTitles]
	}
	titles := make([]Title, 0, len(seen))
	for _, t := range seen {
		titles = append(titles, Title{Text: t.Text, At: t.At})
	}
	return titles
}
