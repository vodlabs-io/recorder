package daemon

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/vodlabs-io/recorder/internal/vlconfig"
	"github.com/vodlabs-io/recorder/internal/vllog"
	"github.com/vodlabs-io/recorder/internal/vodindex"
)

// emptyKV is a jetstream.KeyValue holding nothing. vodScanRoots consults the
// one-shot archive overrides, which a test archive has none of; oneShotKV in
// events_test.go serves Get only, and the roots walk lists keys.
type emptyKV struct{ jetstream.KeyValue }

func (emptyKV) ListKeysFiltered(context.Context, ...string) (jetstream.KeyLister, error) {
	return nil, jetstream.ErrKeyNotFound
}

// writeManifest makes a session folder recognisable to the index.
func writeManifest(t *testing.T, dir string) {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"version": 1, "platform": "kick", "username": "someone",
		"session_id": "S1", "total_bytes": 7,
		"files": []map[string]any{{"name": "vod.mp4", "bytes": 7}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "vod.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// newSession writes a session folder with one file and returns its path.
func newSession(t *testing.T, root, rel string) string {
	t.Helper()
	dir := filepath.Join(root, rel)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "vod.mp4"), []byte("payload"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// The reason the delete goes through a rename: os.RemoveAll erases as it walks,
// so a refusal partway leaves a gutted session behind while reporting failure.
// A refused trashVOD must leave the recording byte-for-byte intact.
func TestTrashVODLeavesSessionIntactWhenRefused(t *testing.T) {
	root := t.TempDir()
	dir := newSession(t, root, "tiktok/someone/2026-08-30_12-00-00-0700")
	parent := filepath.Dir(dir)

	// r-x: the session folder cannot be unlinked from, or renamed out of, its
	// parent — the shape of a locked-down or read-only archive folder.
	if err := os.Chmod(parent, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(parent, 0o755) })

	var a App
	if _, err := a.trashVOD(root, dir); err == nil {
		t.Fatal("trashVOD succeeded on an unwritable parent, want error")
	}
	got, err := os.ReadFile(filepath.Join(dir, "vod.mp4"))
	if err != nil {
		t.Fatalf("recording was destroyed by a refused delete: %v", err)
	}
	if string(got) != "payload" {
		t.Errorf("vod.mp4 = %q, want %q", got, "payload")
	}
}

// A successful trashVOD is the commit point: the session is gone from every
// path the archive scans, and what is left is parked for the sweep.
func TestTrashVODMovesSessionToTrash(t *testing.T) {
	root := t.TempDir()
	dir := newSession(t, root, "kick/someone/2026-08-30_12-00-00-0700")

	var a App
	doomed, err := a.trashVOD(root, dir)
	if err != nil {
		t.Fatalf("trashVOD: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("session folder still at its archive path, err = %v", err)
	}
	if filepath.Dir(doomed) != filepath.Join(root, vodTrashDirName) {
		t.Errorf("doomed = %q, want it under %q", doomed, filepath.Join(root, vodTrashDirName))
	}
	if _, err := os.Stat(filepath.Join(doomed, "vod.mp4")); err != nil {
		t.Errorf("trashed session lost its files: %v", err)
	}
}

// Two deletes of same-named sessions under different streamers must not
// collide in the one shared trash folder.
func TestTrashVODNamesAreUnique(t *testing.T) {
	root := t.TempDir()
	var a App
	first, err := a.trashVOD(root, newSession(t, root, "kick/a/2026-08-30_12-00-00-0700"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := a.trashVOD(root, newSession(t, root, "kick/b/2026-08-30_12-00-00-0700"))
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatalf("both sessions trashed to %q", first)
	}
}

// newVODTestApp is an App with just enough wired up to run DeleteVOD: the index
// over one root, and a logger that writes nowhere. procSvc and encSvc stay nil,
// which DeleteVOD reads as "no work in flight".
func newVODTestApp(t *testing.T, root string) *App {
	t.Helper()
	lg, err := vllog.NewFileLogger(vlconfig.LoggerConfig{Mode: "off", Level: "info"}, "test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lg.Close() })
	// vodScanRoots reads the one-shot overrides and streamers.toml as well as
	// the config, so both have to be present even when empty.
	a := &App{log: lg, natsKV: emptyKV{}, strFile: loadStrFile(t, "")}
	a.appCfg.Session.DownloadDir = root
	a.vodIndex = vodindex.New(
		func() []string { return []string{root} },
		func(context.Context) []vodindex.ActiveWork { return nil },
		time.Hour,
	)
	return a
}

// waitIndexed settles the index's background scan and returns its sessions.
func waitIndexed(t *testing.T, a *App) []vodindex.VOD {
	t.Helper()
	ctx := context.Background()
	for range 100 {
		res := a.vodIndex.List(ctx)
		if !res.Scanning && res.Sessions != nil {
			return res.Sessions
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("index never settled")
	return nil
}

// The whole point of the rename: a delete the filesystem refuses must leave the
// recording playable and still listed, not gutted and reported as failed.
func TestDeleteVODRefusedKeepsRecordingPlayable(t *testing.T) {
	root := t.TempDir()
	dir := newSession(t, root, "kick/someone/2026-08-30_12-00-00-0700")
	writeManifest(t, dir)
	a := newVODTestApp(t, root)

	ctx := context.Background()
	if got := len(waitIndexed(t, a)); got != 1 {
		t.Fatalf("index listed %d sessions before the delete, want 1", got)
	}

	parent := filepath.Dir(dir)
	if err := os.Chmod(parent, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(parent, 0o755) })

	if err := a.DeleteVOD(ctx, dir); err == nil {
		t.Fatal("DeleteVOD succeeded on an unwritable parent, want error")
	}
	if _, err := os.Stat(filepath.Join(dir, "vod.mp4")); err != nil {
		t.Errorf("recording was destroyed by a refused delete: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "vod.json")); err != nil {
		t.Errorf("manifest was destroyed by a refused delete: %v", err)
	}
	if _, ok := a.vodIndex.Find(ctx, dir); !ok {
		t.Error("session dropped from the index despite the delete failing")
	}
}

// A delete that goes through leaves nothing behind: not the session, not the
// folders that only existed to hold it, and not the trash it passed through.
func TestDeleteVODRemovesSessionAndTrash(t *testing.T) {
	root := t.TempDir()
	dir := newSession(t, root, "kick/someone/2026-08-30_12-00-00-0700")
	writeManifest(t, dir)
	a := newVODTestApp(t, root)

	ctx := context.Background()
	waitIndexed(t, a) // populate, so Find resolves the session
	if err := a.DeleteVOD(ctx, dir); err != nil {
		t.Fatalf("DeleteVOD: %v", err)
	}
	for _, p := range []string{dir, filepath.Dir(dir), filepath.Join(root, vodTrashDirName)} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s survived the delete, err = %v", p, err)
		}
	}
	if _, ok := a.vodIndex.Find(ctx, dir); ok {
		t.Error("deleted session still listed in the index")
	}
}

// sweepVODTrash reclaims what an interrupted delete parked, and takes the trash
// folder with it once it is empty.
func TestSweepVODTrashErasesLeftovers(t *testing.T) {
	root := t.TempDir()
	a := newVODTestApp(t, root)

	leftover := filepath.Join(root, vodTrashDirName, "123-2026-08-30_12-00-00-0700")
	if err := os.MkdirAll(leftover, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(leftover, "vod.mp4"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	a.sweepVODTrash()

	if _, err := os.Stat(filepath.Join(root, vodTrashDirName)); !os.IsNotExist(err) {
		t.Errorf("trash folder survived the sweep, err = %v", err)
	}
}

// Scan folders must be walked for VODs but must never reach the write-side
// roots: an unplugged retired disk raising a low-space warning about a volume
// nothing records to is exactly the noise the split exists to avoid.
func TestScanFoldersAreScannedButNotWritten(t *testing.T) {
	download := t.TempDir()
	archive := t.TempDir()
	scan := t.TempDir()

	a := &App{natsKV: emptyKV{}, strFile: loadStrFile(t, "")}
	a.appCfg.Session.DownloadDir = download
	a.appCfg.Processing.ArchiveDir = archive
	a.appCfg.Processing.ScanDirs = []string{scan}

	if got := a.vodScanRoots(); !contains(got, scan) {
		t.Errorf("vodScanRoots() = %v, want it to include the scan folder %s", got, scan)
	}
	if got := a.vodArchiveRoots(); contains(got, scan) {
		t.Errorf("vodArchiveRoots() = %v, must not include the scan folder %s", got, scan)
	}
	if !a.isReadOnlyVODRoot(scan) {
		t.Errorf("isReadOnlyVODRoot(%s) = false, want true", scan)
	}
	for _, writable := range []string{download, archive} {
		if a.isReadOnlyVODRoot(writable) {
			t.Errorf("isReadOnlyVODRoot(%s) = true, want false", writable)
		}
	}
}

// The scan root a VOD is stamped with has to match the configured scan folder
// exactly, since that string equality is what marks the VOD read-only for both
// the merge guard and the client.
func TestScanFolderRootMatchesTheConfiguredPath(t *testing.T) {
	download := t.TempDir()
	scan := t.TempDir()
	newSession(t, scan, "kick/someone/2026-08-30_12-00-00-0700")

	a := &App{natsKV: emptyKV{}, strFile: loadStrFile(t, "")}
	a.appCfg.Session.DownloadDir = download
	// Stored with a trailing separator, as a hand-edited config.toml may hold it.
	a.appCfg.Processing.ScanDirs = []string{scan + string(filepath.Separator)}

	res := vodindex.Scan(context.Background(), a.vodScanRoots(), nil)
	if len(res.Sessions) != 1 {
		t.Fatalf("Scan found %d sessions, want 1", len(res.Sessions))
	}
	if !a.isReadOnlyVODRoot(res.Sessions[0].Root) {
		t.Errorf("session root %q not recognised as a scan folder (configured %q)",
			res.Sessions[0].Root, scan)
	}
}

func contains(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}

// jpegBytes is the smallest byte string completeJPEG accepts.
var jpegBytes = []byte{0xFF, 0xD8, 0x00, 0x00, 0xFF, 0xD9}

// A busy session serves any part's screenshot, not just the one asked for:
// the part named may not exist yet, or may already have moved away.
func TestBusyThumbnailFallsBackToAnotherPartsSidecar(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "vod1.jpg"), jpegBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	// A sidecar still being written is passed over.
	if err := os.WriteFile(filepath.Join(dir, "vod2.jpg"), jpegBytes[:3], 0o644); err != nil {
		t.Fatal(err)
	}
	a := newVODTestApp(t, dir)
	session := vodindex.VOD{
		Dir:   dir,
		State: vodindex.StateMoving,
		Files: []vodindex.File{{Name: "vod1.mp4"}, {Name: "vod2.mp4"}},
	}
	for _, file := range []string{"vod2.mp4", ""} {
		got, err := a.busyVODThumbnail(context.Background(), session, file)
		if err != nil || string(got) != string(jpegBytes) {
			t.Errorf("busyVODThumbnail(%q) = %v, %v; want vod1's sidecar", file, got, err)
		}
	}
}

// Raw Parts are the fallback when nothing has been produced yet: finished
// captures before converted intermediates, each in Part order.
func TestBusyThumbSourcesOrder(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"pt10_best.ts", "pt2_best.ts", "pt1_best.mp4", "session.log", "vod1.mp4"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(dir, "pt3_720p"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pt3_720p", "index.m3u8"), []byte("#EXTM3U"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := busyThumbSources(dir, []string{"vod2.mp4", "vod1.mp4"})
	want := []string{
		filepath.Join(dir, "vod1.mp4"),
		filepath.Join(dir, "pt2_best.ts"),
		filepath.Join(dir, "pt3_720p", "index.m3u8"),
	}
	if len(got) != len(want) {
		t.Fatalf("busyThumbSources = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("busyThumbSources = %q, want %q", got, want)
		}
	}
}
