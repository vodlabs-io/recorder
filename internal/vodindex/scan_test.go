package vodindex

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vodlabs-io/recorder/internal/recording"
	"github.com/vodlabs-io/recorder/internal/streamer"
	"github.com/vodlabs-io/recorder/internal/vodmeta"
)

func writeFile(t *testing.T, path string, size int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, make([]byte, size), 0o644); err != nil {
		t.Fatal(err)
	}
}

func sessionByDir(t *testing.T, res Result, dir string) VOD {
	t.Helper()
	for _, s := range res.Sessions {
		if s.Dir == dir {
			return s
		}
	}
	t.Fatalf("session %s not found in %d sessions", dir, len(res.Sessions))
	return VOD{}
}

func TestScanManifestSession(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "tiktok", "alice", "2026-01-02_03-04-05-0700")
	writeFile(t, filepath.Join(dir, "vod1.mp4"), 2048)
	writeFile(t, filepath.Join(dir, "vod2.mp4"), 1024)
	writeFile(t, filepath.Join(dir, "vod1.jpg"), 10)
	writeFile(t, filepath.Join(dir, "session.log"), 5)

	start := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	err := recording.WriteManifest(dir, recording.Manifest{
		Platform:    "tiktok",
		Username:    "alice",
		SessionID:   "sess-1",
		Writer:      "flvwriter",
		Start:       start,
		End:         start.Add(2 * time.Hour),
		TotalBytes:  3072,
		DurationSec: 7200,
		Files: []recording.ManifestFile{
			{Name: "vod1.mp4", Bytes: 1, DurationSec: 3600},
			{Name: "vod2.mp4", Bytes: 1, DurationSec: 3600},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	res := Scan(context.Background(), []string{root}, nil)
	if len(res.Sessions) != 1 {
		t.Fatalf("got %d sessions, want 1", len(res.Sessions))
	}
	s := res.Sessions[0]
	if !s.FromManifest || s.SessionID != "sess-1" || s.Platform != "tiktok" || s.Username != "alice" {
		t.Errorf("manifest metadata not applied: %+v", s)
	}
	if s.State != StateReady {
		t.Errorf("state = %q, want ready", s.State)
	}
	if len(s.Files) != 2 {
		t.Fatalf("got %d files, want 2", len(s.Files))
	}
	// Disk sizes override the manifest's, and thumbnails are detected per file.
	if s.Files[0].Name != "vod1.mp4" || s.Files[0].Bytes != 2048 || !s.Files[0].HasThumb {
		t.Errorf("vod1: %+v", s.Files[0])
	}
	if s.Files[1].Name != "vod2.mp4" || s.Files[1].Bytes != 1024 || s.Files[1].HasThumb {
		t.Errorf("vod2: %+v", s.Files[1])
	}
}

func TestScanLegacyFallback(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "twitch", "bob", "2025-06-07_08-09-10-0700")
	writeFile(t, filepath.Join(dir, "vod.mp4"), 4096)

	res := Scan(context.Background(), []string{root}, nil)
	s := sessionByDir(t, res, dir)
	if s.FromManifest {
		t.Error("legacy folder claimed manifest")
	}
	if s.Platform != "twitch" || s.Username != "bob" {
		t.Errorf("layout parse: platform=%q username=%q", s.Platform, s.Username)
	}
	want := time.Date(2025, 6, 7, 8, 9, 10, 0, time.FixedZone("", -7*3600))
	if !s.Start.Equal(want) {
		t.Errorf("start = %v, want %v", s.Start, want)
	}
	if s.TotalBytes != 4096 || len(s.Files) != 1 || s.Files[0].Name != "vod.mp4" {
		t.Errorf("files: %+v", s.Files)
	}
}

// TestScanOrdersByEndWhenStartMissing covers manifests that carry only `end`:
// a session must sort by its end rather than collapsing to the zero time. The
// newer session (later end) must come first.
func TestScanOrdersByEndWhenStartMissing(t *testing.T) {
	root := t.TempDir()
	// Dir names are chosen so the older session sorts FIRST lexically: if the
	// scanner fell back to the path tiebreak (both starts zero) it would rank
	// the older one first, so passing proves the end drives the order.
	older := filepath.Join(root, "tiktok", "alice", "aaa")
	newer := filepath.Join(root, "tiktok", "alice", "zzz")
	writeFile(t, filepath.Join(older, "vod.mp4"), 100)
	writeFile(t, filepath.Join(newer, "vod.mp4"), 100)

	// Both manifests omit Start (zero); only End differs.
	end := time.Date(2026, 7, 12, 20, 0, 0, 0, time.UTC)
	for dir, e := range map[string]time.Time{older: end, newer: end.Add(time.Hour)} {
		if err := recording.WriteManifest(dir, recording.Manifest{
			Platform: "tiktok", Username: "alice", End: e,
			Files: []recording.ManifestFile{{Name: "vod.mp4"}},
		}); err != nil {
			t.Fatal(err)
		}
	}

	res := Scan(context.Background(), []string{root}, nil)
	if len(res.Sessions) != 2 {
		t.Fatalf("got %d sessions, want 2", len(res.Sessions))
	}
	if res.Sessions[0].Dir != filepath.Clean(newer) {
		t.Errorf("newest-first ordering broken: first session = %s, want %s",
			res.Sessions[0].Dir, newer)
	}
}

func TestScanSplitOrderingAndPartSkip(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "tiktok", "carol", "sess")
	// Out-of-order creation; vod10 must sort after vod2.
	for _, name := range []string{"vod10.mp4", "vod1.mp4", "vod2.mp4"} {
		writeFile(t, filepath.Join(dir, name), 100)
	}
	// Raw part leftovers must neither be listed nor treated as session dirs.
	writeFile(t, filepath.Join(dir, "pt1_best.ts"), 100)
	writeFile(t, filepath.Join(dir, "pt2_best", "index.m3u8"), 10)

	res := Scan(context.Background(), []string{root}, nil)
	s := sessionByDir(t, res, dir)
	got := []string{}
	for _, f := range s.Files {
		got = append(got, f.Name)
	}
	want := []string{"vod1.mp4", "vod2.mp4", "vod10.mp4"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("file order = %v, want %v", got, want)
		}
	}
}

func TestScanUnavailableRoot(t *testing.T) {
	root := t.TempDir()
	missing := filepath.Join(root, "nope")
	res := Scan(context.Background(), []string{root, missing, missing}, nil)
	if len(res.UnavailableRoots) != 1 || res.UnavailableRoots[0] != missing {
		t.Errorf("unavailable roots: %v", res.UnavailableRoots)
	}
}

func TestScanActiveWork(t *testing.T) {
	archive := t.TempDir()
	dest := t.TempDir()

	// A processing session: folder exists, no output produced yet.
	procDir := filepath.Join(archive, "tiktok", "dave", "s1")
	writeFile(t, filepath.Join(procDir, "pt1_best.ts"), 100)

	// A moving session: local folder still authoritative, destination folder
	// already holds a copied file and must be suppressed.
	movDir := filepath.Join(archive, "tiktok", "erin", "s2")
	writeFile(t, filepath.Join(movDir, "vod.mp4"), 100)
	destCopy := filepath.Join(dest, "tiktok", "erin", "s2")
	writeFile(t, filepath.Join(destCopy, "vod.mp4"), 100)

	active := []ActiveWork{
		{BaseOutDir: procDir, Platform: "tiktok", Username: "dave", SessionID: "s1", State: StateProcessing},
		{BaseOutDir: movDir, Dest: dest, RelDir: filepath.Join("tiktok", "erin", "s2"), State: StateMoving},
	}
	res := Scan(context.Background(), []string{archive, dest}, active)

	p := sessionByDir(t, res, procDir)
	if p.State != StateProcessing || p.Username != "dave" || p.SessionID != "s1" {
		t.Errorf("processing session: %+v", p)
	}
	m := sessionByDir(t, res, movDir)
	if m.State != StateMoving {
		t.Errorf("moving session state = %q", m.State)
	}
	for _, s := range res.Sessions {
		if s.Dir == destCopy {
			t.Error("half-moved destination folder was listed")
		}
	}
}

// waitScan polls List until the in-flight background scan has completed
// (Scanning is false), returning the settled result. Scans run in a goroutine,
// so a fresh List returns an empty/stale, Scanning result first.
func waitScan(t *testing.T, ix *Index) Result {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		res := ix.List(t.Context())
		if !res.Scanning {
			return res
		}
		if time.Now().After(deadline) {
			t.Fatal("background scan did not complete in time")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func TestIndexCacheAndInvalidate(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "p", "u", "s")
	writeFile(t, filepath.Join(dir, "vod.mp4"), 10)

	ix := New(func() []string { return []string{root} }, nil, time.Hour)

	// Cold cache: the first List returns immediately with Scanning set and no
	// sessions yet; the real listing arrives once the background scan finishes.
	if first := ix.List(t.Context()); !first.Scanning || len(first.Sessions) != 0 {
		t.Fatalf("first list: scanning=%v sessions=%d, want scanning with 0", first.Scanning, len(first.Sessions))
	}
	if got := len(waitScan(t, ix).Sessions); got != 1 {
		t.Fatalf("after first scan: %d sessions, want 1", got)
	}

	writeFile(t, filepath.Join(root, "p", "u", "s2", "vod.mp4"), 10)
	if got := len(ix.List(t.Context()).Sessions); got != 1 {
		t.Fatalf("cached list should not see new session, got %d", got)
	}
	ix.Invalidate()
	if got := len(waitScan(t, ix).Sessions); got != 2 {
		t.Fatalf("post-invalidate list: %d sessions, want 2", got)
	}

	if _, ok := ix.Find(t.Context(), dir); !ok {
		t.Error("Find failed for listed session")
	}
	if _, ok := ix.Find(t.Context(), filepath.Join(root, "elsewhere")); ok {
		t.Error("Find matched an unlisted dir")
	}
}

// TestIndexServesStaleWhileRescanning verifies List never blocks on a rescan:
// once invalidated it keeps serving the previous scan (with Scanning set) until
// the background scan replaces it, and announces completion via SetOnChanged.
// The rescan is gated inside the roots func so the stale window is observable.
func TestIndexServesStaleWhileRescanning(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "p", "u", "s", "vod.mp4"), 10)

	var refreshed, scans atomic.Int32
	started := make(chan struct{}, 1)
	release := make(chan struct{})

	ix := New(func() []string {
		if scans.Add(1) == 2 {
			// The rescan: signal it began, then block until the test has
			// observed that List still serves the stale listing meanwhile.
			started <- struct{}{}
			<-release
		}
		return []string{root}
	}, nil, time.Hour)
	ix.SetOnChanged(func(int64) { refreshed.Add(1) })

	waitScan(t, ix) // prime the cache with the first session

	writeFile(t, filepath.Join(root, "p", "u", "s2", "vod.mp4"), 10)
	ix.Invalidate()

	// Triggers the (blocked) rescan and returns the stale listing immediately.
	stale := ix.List(t.Context())
	<-started
	if len(stale.Sessions) != 1 || !stale.Scanning {
		t.Fatalf("stale serve: sessions=%d scanning=%v, want 1 and scanning", len(stale.Sessions), stale.Scanning)
	}
	// A concurrent List during the rescan also gets stale data, never blocks.
	if mid := ix.List(t.Context()); len(mid.Sessions) != 1 || !mid.Scanning {
		t.Fatalf("mid-rescan serve: sessions=%d scanning=%v, want 1 and scanning", len(mid.Sessions), mid.Scanning)
	}
	close(release) // let the rescan finish

	if got := len(waitScan(t, ix).Sessions); got != 2 {
		t.Fatalf("after rescan: %d sessions, want 2", got)
	}
	if refreshed.Load() == 0 {
		t.Error("SetOnChanged callback never fired")
	}
}

// TestIndexForget drops a session from the cache without a rescan.
func TestIndexForget(t *testing.T) {
	root := t.TempDir()
	keep := filepath.Join(root, "p", "u", "keep")
	drop := filepath.Join(root, "p", "u", "drop")
	writeFile(t, filepath.Join(keep, "vod.mp4"), 10)
	writeFile(t, filepath.Join(drop, "vod.mp4"), 10)

	ix := New(func() []string { return []string{root} }, nil, time.Hour)
	if got := len(waitScan(t, ix).Sessions); got != 2 {
		t.Fatalf("initial: %d sessions, want 2", got)
	}

	ix.Forget(drop)
	// No rescan (cache still fresh): the forgotten session is gone, the other
	// stays, and Scanning is not set.
	res := ix.List(t.Context())
	if res.Scanning {
		t.Error("Forget should not trigger a scan")
	}
	if len(res.Sessions) != 1 || res.Sessions[0].Dir != filepath.Clean(keep) {
		t.Fatalf("after Forget: %+v, want only %s", res.Sessions, keep)
	}
}

// The full-walk path applies the overlay separately from scanOne's Learn path,
// so a field added to one can silently be absent from the other.
func TestScanCarriesTheMoveReason(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "tiktok", "someone", "2026-01-01_00-00-00-0000")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "vod.mp4"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	active := []ActiveWork{{
		BaseOutDir: dir,
		State:      StateMoving,
		Reason:     "no_space",
		Dest:       filepath.Join(t.TempDir(), "archive"),
		RelDir:     "tiktok/someone/2026-01-01_00-00-00-0000",
	}}

	res := Scan(context.Background(), []string{root}, active)
	if len(res.Sessions) != 1 {
		t.Fatalf("got %d sessions, want 1", len(res.Sessions))
	}
	if got := res.Sessions[0].State; got != StateMoving {
		t.Errorf("State = %q, want %q", got, StateMoving)
	}
	if got := res.Sessions[0].Reason; got != "no_space" {
		t.Errorf("Reason = %q, want %q — the walk dropped it", got, "no_space")
	}
}

// An unchanged session folder is carried over from the previous scan on the
// strength of its timestamp, without the folder being read again.
func TestScanReusesUnchangedSessions(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "p", "u", "2026-01-01_00-00-00+0000")
	writeFile(t, filepath.Join(dir, "vod.mp4"), 10)

	first := Scan(t.Context(), []string{root}, nil)
	if len(first.Sessions) != 1 {
		t.Fatalf("first scan: %d sessions, want 1", len(first.Sessions))
	}
	if first.Sessions[0].DirMod.IsZero() {
		t.Fatal("first scan did not stamp the session folder")
	}

	prev := map[string]VOD{dir: first.Sessions[0]}
	// Mark the carried-over entry so a reuse is distinguishable from a re-read.
	marked := prev[dir]
	marked.Username = "carried-over"
	prev[dir] = marked

	res := ScanReusing(t.Context(), []string{root}, nil, prev)
	if len(res.Sessions) != 1 {
		t.Fatalf("reusing scan: %d sessions, want 1", len(res.Sessions))
	}
	if res.Sessions[0].Username != "carried-over" {
		t.Error("session was re-read from disk instead of reused")
	}
}

// Adding a file moves the folder's timestamp, so the entry must be re-read.
func TestScanRereadsChangedSessions(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "p", "u", "2026-01-01_00-00-00+0000")
	writeFile(t, filepath.Join(dir, "vod.mp4"), 10)

	first := Scan(t.Context(), []string{root}, nil)
	stale := first.Sessions[0]
	stale.Username = "carried-over"

	// A second part lands: the folder's timestamp moves with it.
	writeFile(t, filepath.Join(dir, "vod2.mp4"), 20)
	touch := time.Now().Add(time.Second)
	if err := os.Chtimes(dir, touch, touch); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	res := ScanReusing(t.Context(), []string{root}, nil, map[string]VOD{dir: stale})
	if len(res.Sessions) != 1 {
		t.Fatalf("%d sessions, want 1", len(res.Sessions))
	}
	if res.Sessions[0].Username == "carried-over" {
		t.Error("a changed folder was reused instead of re-read")
	}
	if got := len(res.Sessions[0].Files); got != 2 {
		t.Errorf("re-read session has %d files, want 2", got)
	}
}

// A session the queue is working on is never reused: appending to a file does
// not move its folder's timestamp, so its size must come from the filesystem.
func TestScanNeverReusesActiveSessions(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "p", "u", "2026-01-01_00-00-00+0000")
	writeFile(t, filepath.Join(dir, "vod.mp4"), 10)

	first := Scan(t.Context(), []string{root}, nil)
	stale := first.Sessions[0]
	stale.Username = "carried-over"

	// The file grows without the folder changing — the case the guard exists
	// for.
	writeFile(t, filepath.Join(dir, "vod.mp4"), 5000)

	active := []ActiveWork{{BaseOutDir: dir, State: StateProcessing}}
	res := ScanReusing(t.Context(), []string{root}, active, map[string]VOD{dir: stale})
	if len(res.Sessions) != 1 {
		t.Fatalf("%d sessions, want 1", len(res.Sessions))
	}
	if res.Sessions[0].Username == "carried-over" {
		t.Error("an active session was reused instead of re-read")
	}
	if got := res.Sessions[0].TotalBytes; got != 5000 {
		t.Errorf("reused a stale size: %d bytes, want 5000", got)
	}
}

// A folder listed last time that has since been deleted must drop out.
func TestScanReusingDropsDeletedSessions(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "p", "u", "2026-01-01_00-00-00+0000")
	writeFile(t, filepath.Join(dir, "vod.mp4"), 10)
	first := Scan(t.Context(), []string{root}, nil)

	if err := os.RemoveAll(dir); err != nil {
		t.Fatalf("RemoveAll: %v", err)
	}
	res := ScanReusing(t.Context(), []string{root}, nil, map[string]VOD{dir: first.Sessions[0]})
	if len(res.Sessions) != 0 {
		t.Errorf("%d sessions, want 0 after the folder was deleted", len(res.Sessions))
	}
}

// Reuse must not hide a session that appeared since the last walk.
func TestScanReusingStillFindsNewSessions(t *testing.T) {
	root := t.TempDir()
	first := filepath.Join(root, "p", "u", "2026-01-01_00-00-00+0000")
	writeFile(t, filepath.Join(first, "vod.mp4"), 10)
	prevRes := Scan(t.Context(), []string{root}, nil)

	second := filepath.Join(root, "p", "u", "2026-01-02_00-00-00+0000")
	writeFile(t, filepath.Join(second, "vod.mp4"), 20)

	res := ScanReusing(t.Context(), []string{root}, nil,
		map[string]VOD{first: prevRes.Sessions[0]})
	if len(res.Sessions) != 2 {
		t.Errorf("%d sessions, want 2", len(res.Sessions))
	}
}

// The fingerprint must ignore DirMod: a folder timestamp that moves without
// changing anything the listing shows must not look like new content, or every
// client re-downloads the archive for nothing.
func TestFingerprintIgnoresDirMod(t *testing.T) {
	a := Result{Sessions: []VOD{{Dir: "/x", Username: "u", DirMod: time.Unix(1, 0)}}}
	b := Result{Sessions: []VOD{{Dir: "/x", Username: "u", DirMod: time.Unix(9999, 0)}}}
	if fingerprint(&a) != fingerprint(&b) {
		t.Error("DirMod changed the fingerprint")
	}

	c := Result{Sessions: []VOD{{Dir: "/x", Username: "other", DirMod: time.Unix(1, 0)}}}
	if fingerprint(&a) == fingerprint(&c) {
		t.Error("a real content change did not change the fingerprint")
	}
}

// The metadata sidecar's title timeline is lifted into the listing, so the
// archive can label a session with what the streamer called it.
func TestScanCarriesStreamTitles(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "twitch", "streamer_four", "2026-09-04_12-00-00-0000")
	writeFile(t, filepath.Join(dir, "vod.mp4"), 4096)

	first := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	err := vodmeta.Write(dir, vodmeta.Document{
		Platform:  "twitch",
		Username:  "streamer_four",
		SessionID: "sess-1",
		Observations: []vodmeta.Observation{
			{At: first, LiveMetadata: streamer.LiveMetadata{Title: "warmup", Viewers: 10}},
			// Only the viewer count moved: the same title, not a second one.
			{At: first.Add(time.Hour), LiveMetadata: streamer.LiveMetadata{Title: "warmup", Viewers: 900}},
			{At: first.Add(2 * time.Hour), LiveMetadata: streamer.LiveMetadata{Title: "ranked grind"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	res := Scan(context.Background(), []string{root}, nil)
	s := sessionByDir(t, res, dir)
	if len(s.Titles) != 2 {
		t.Fatalf("Titles = %+v, want 2 entries", s.Titles)
	}
	if s.Titles[0].Text != "warmup" || !s.Titles[0].At.Equal(first) {
		t.Errorf("Titles[0] = %+v", s.Titles[0])
	}
	if s.Titles[1].Text != "ranked grind" {
		t.Errorf("Titles[1] = %+v", s.Titles[1])
	}
}

// The sidecar's latest display name names the streamer in the listing, for
// platforms that key streamers by a numeric id.
func TestScanCarriesDisplayName(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "kwai", "150000024312774", "2026-10-16_12-00-00-0000")
	writeFile(t, filepath.Join(dir, "vod.mp4"), 4096)
	at := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	err := vodmeta.Write(dir, vodmeta.Document{
		Platform: "kwai",
		Username: "150000024312774",
		Observations: []vodmeta.Observation{
			{At: at, LiveMetadata: streamer.LiveMetadata{DisplayName: "Old Name", Viewers: 1}},
			{At: at.Add(time.Minute), LiveMetadata: streamer.LiveMetadata{Viewers: 2, Title: "t"}},
			{At: at.Add(2 * time.Minute), LiveMetadata: streamer.LiveMetadata{DisplayName: "New Name"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	s := sessionByDir(t, Scan(context.Background(), []string{root}, nil), dir)
	if s.DisplayName != "New Name" {
		t.Errorf("DisplayName = %q, want New Name", s.DisplayName)
	}
}

// A session folder with no sidecar (e.g. a platform that reports no title)
// still lists normally.
func TestScanWithoutMetadataSidecar(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "twitch", "bob", "2026-09-04_12-00-00-0000")
	writeFile(t, filepath.Join(dir, "vod.mp4"), 4096)

	s := sessionByDir(t, Scan(context.Background(), []string{root}, nil), dir)
	if s.Titles != nil {
		t.Errorf("Titles = %+v, want none", s.Titles)
	}
}

// The sidecar alone does not make a folder a session: it is written as soon as
// recording starts, well before there is anything to list.
func TestScanIgnoresSidecarOnlyFolder(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "twitch", "carol", "2026-09-04_12-00-00-0000")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	err := vodmeta.Write(dir, vodmeta.Document{
		Platform: "twitch",
		Username: "carol",
		Observations: []vodmeta.Observation{
			{At: time.Now(), LiveMetadata: streamer.LiveMetadata{Title: "starting soon"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	if res := Scan(context.Background(), []string{root}, nil); len(res.Sessions) != 0 {
		t.Errorf("got %d sessions, want 0: %+v", len(res.Sessions), res.Sessions)
	}
}
