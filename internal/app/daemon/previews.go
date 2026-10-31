package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/vodlabs-io/recorder/internal/processing"
	"github.com/vodlabs-io/recorder/internal/vlapi"
	"github.com/vodlabs-io/recorder/internal/vlconfig"
	"github.com/vodlabs-io/recorder/internal/vodindex"
	"github.com/vodlabs-io/recorder/internal/webui"
)

// sheetFlight builds at most one contact sheet per session at a time;
// callers asking meanwhile wait for its result.
type sheetFlight struct {
	mu    sync.Mutex
	calls map[string]*sheetCall
}

type sheetCall struct {
	done chan struct{}
	b    []byte
	err  error
}

func (f *sheetFlight) do(key string, fn func() ([]byte, error)) ([]byte, error) {
	f.mu.Lock()
	if f.calls == nil {
		f.calls = map[string]*sheetCall{}
	}
	if c, ok := f.calls[key]; ok {
		f.mu.Unlock()
		<-c.done
		return c.b, c.err
	}
	c := &sheetCall{done: make(chan struct{})}
	f.calls[key] = c
	f.mu.Unlock()
	c.b, c.err = fn()
	close(c.done)
	f.mu.Lock()
	delete(f.calls, key)
	f.mu.Unlock()
	return c.b, c.err
}

// readySession resolves a finished, listed VOD.
func (a *App) readySession(ctx context.Context, dir string) (vodindex.VOD, error) {
	session, ok := a.vodIndex.Find(ctx, dir)
	if !ok {
		return vodindex.VOD{}, fmt.Errorf("%w: not an archived session: %s", vlapi.ErrNotFound, dir)
	}
	if session.State != vodindex.StateReady {
		return vodindex.VOD{}, fmt.Errorf("%w: session is still being worked on", vlapi.ErrConflict)
	}
	return session, nil
}

// readPreviews returns the previews processing made for one file of a
// finished VOD, and the folder they are in. A file recorded before previews
// existed has none.
func (a *App) readPreviews(ctx context.Context, dir, file string) (processing.PreviewsManifest, string, error) {
	if file == "" || file != filepath.Base(file) || strings.ContainsAny(file, `/\`) {
		return processing.PreviewsManifest{}, "", fmt.Errorf("%w: bad file name", vlapi.ErrNotFound)
	}
	session, err := a.readySession(ctx, dir)
	if err != nil {
		return processing.PreviewsManifest{}, "", err
	}
	listed := false
	for _, f := range session.Files {
		listed = listed || f.Name == file
	}
	if !listed {
		return processing.PreviewsManifest{}, "", fmt.Errorf("%w: no such file in session: %s", vlapi.ErrNotFound, file)
	}
	m, err := processing.ReadPreviewsManifest(session.Dir, file)
	if err != nil || m.Version != processing.PreviewsVersion {
		return m, "", fmt.Errorf("%w: no previews for %s", vlapi.ErrNotFound, file)
	}
	return m, session.Dir, nil
}

// VODPreviews implements vlapi.VODController: a VOD file's preview layout and
// its hover strip.
func (a *App) VODPreviews(ctx context.Context, dir, file string) (processing.PreviewsManifest, []byte, error) {
	m, out, err := a.readPreviews(ctx, dir, file)
	if err != nil {
		return m, nil, err
	}
	strip, err := processing.ReadSidecar(out, processing.ScrubStripName(file))
	return m, strip, err
}

// VODContactSheet implements vlapi.VODController: one sheet across all of a
// session's files, built on first view for a session whose files have
// previews and kept beside them (or, for a read-only scan folder, in a local
// cache folder). Older sessions have none.
func (a *App) VODContactSheet(ctx context.Context, dir string) ([]byte, error) {
	session, err := a.readySession(ctx, dir)
	if err != nil {
		return nil, err
	}
	var files []processing.SheetFile
	withPreviews := false
	for _, f := range session.Files {
		files = append(files, processing.SheetFile{Name: f.Name, Bytes: f.Bytes})
		withPreviews = withPreviews || processing.HasPreviews(session.Dir, f.Name)
	}
	if !withPreviews {
		return nil, fmt.Errorf("%w: no previews in session", vlapi.ErrNotFound)
	}
	out, err := a.sessionPreviewsDir(session)
	if err != nil {
		return nil, err
	}
	return a.sheets.do(out, func() ([]byte, error) {
		if processing.SessionSheetCurrent(out, files) {
			return os.ReadFile(filepath.Join(out, processing.SessionSheetName))
		}
		hdr := processing.PreviewHeader{Platform: session.Platform, Username: session.Username, Start: session.Start}
		return processing.WriteSessionSheet(context.WithoutCancel(ctx), a.log.With("service", "previews"), session.Dir, out, files, hdr)
	})
}

// VODSessionThumbnail implements vlapi.VODController: the thumbnail of a
// whole VOD. A portrait VOD of several files with previews gets three frames
// across all of them, built on first request and kept like its contact
// sheet; any other has its first file's.
func (a *App) VODSessionThumbnail(ctx context.Context, dir string) ([]byte, error) {
	session, ok := a.vodIndex.Find(ctx, dir)
	if !ok {
		return nil, fmt.Errorf("%w: not an archived session: %s", vlapi.ErrNotFound, dir)
	}
	first := func() ([]byte, error) {
		if len(session.Files) == 0 {
			return a.VODThumbnail(ctx, dir, "")
		}
		return a.VODThumbnail(ctx, dir, session.Files[0].Name)
	}
	if session.State != vodindex.StateReady || len(session.Files) < 2 {
		return first()
	}
	m, err := processing.ReadPreviewsManifest(session.Dir, session.Files[0].Name)
	if err != nil || !m.Portrait {
		return first()
	}
	var files []processing.SheetFile
	for _, f := range session.Files {
		files = append(files, processing.SheetFile{Name: f.Name, Bytes: f.Bytes})
	}
	key, file := thumbCacheKey(session), sessionThumbCacheFile(files)
	if jpeg, ok := a.thumbCache.Get(key, file); ok {
		return jpeg, nil
	}
	out, err := a.sessionPreviewsDir(session)
	if err != nil {
		return nil, err
	}
	jpeg, err := a.sheets.do(out+"\x00thumbnail", func() ([]byte, error) {
		if processing.SessionThumbnailCurrent(out, files) {
			return os.ReadFile(filepath.Join(out, processing.SessionThumbnailName))
		}
		return processing.WriteSessionThumbnail(context.WithoutCancel(ctx), a.log.With("service", "previews"), session.Dir, out, files)
	})
	if err != nil {
		a.log.Debug("Falling back to the first file's thumbnail", "dir", dir, "error", err)
		return first()
	}
	if err := a.thumbCache.Put(key, file, jpeg); err != nil {
		a.log.Debug("Failed to cache thumbnail", "session", key, "file", file, "error", err)
	}
	return jpeg, nil
}

// sessionThumbCacheFile names a session thumbnail in the thumbnail cache,
// apart from any one file's and from one built from other files.
func sessionThumbCacheFile(files []processing.SheetFile) string {
	h := sha256.New()
	for _, f := range files {
		fmt.Fprintf(h, "%s\x00%d\x00", f.Name, f.Bytes)
	}
	return processing.SessionThumbnailName + "#" + hex.EncodeToString(h.Sum(nil)[:8])
}

// sessionPreviewsDir is where a session's own previews go: its folder, or
// for a read-only scan folder, a local cache folder.
func (a *App) sessionPreviewsDir(session vodindex.VOD) (string, error) {
	if !a.isReadOnlyVODRoot(session.Root) {
		return session.Dir, nil
	}
	sum := sha256.Sum256([]byte(thumbCacheKey(session)))
	out := filepath.Join(vlconfig.DefaultCacheFilePath("previews"), hex.EncodeToString(sum[:16]))
	return out, os.MkdirAll(out, 0o755)
}

// OpenVODPreview implements webui.PreviewOpener: the timeline-preview track
// of a VOD file (kind "vtt") or one of its sprite sheets ("sheet", n).
func (a *App) OpenVODPreview(ctx context.Context, dir, file, kind string, n int) (*os.File, error) {
	if !a.vodReady.Load() {
		return nil, webui.ErrNotReady
	}
	_, out, err := a.readPreviews(ctx, dir, file)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", webui.ErrNotFound, err)
	}
	var name string
	switch kind {
	case "vtt":
		name = processing.StoryboardName(file)
	case "sheet":
		if n < 0 || n > 999 {
			return nil, webui.ErrNotFound
		}
		name = processing.StoryboardSheetName(file, n)
	default:
		return nil, webui.ErrNotFound
	}
	f, err := processing.OpenSidecar(out, name)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", webui.ErrNotFound, err)
	}
	return f, nil
}

// Compile-time check that App serves the Web UI's timeline previews.
var _ webui.PreviewOpener = (*App)(nil)
