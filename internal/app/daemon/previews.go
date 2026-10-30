package daemon

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"image/jpeg"
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

// previewFlight runs at most one generation per VOD file at a time; callers
// asking meanwhile wait for its result.
type previewFlight struct {
	mu    sync.Mutex
	calls map[string]*previewCall
}

type previewCall struct {
	done chan struct{}
	m    processing.PreviewsManifest
	err  error
}

func (f *previewFlight) do(key string, fn func() (processing.PreviewsManifest, error)) (processing.PreviewsManifest, error) {
	f.mu.Lock()
	if f.calls == nil {
		f.calls = map[string]*previewCall{}
	}
	if c, ok := f.calls[key]; ok {
		f.mu.Unlock()
		<-c.done
		return c.m, c.err
	}
	c := &previewCall{done: make(chan struct{})}
	f.calls[key] = c
	f.mu.Unlock()
	c.m, c.err = fn()
	close(c.done)
	f.mu.Lock()
	delete(f.calls, key)
	f.mu.Unlock()
	return c.m, c.err
}

// previewTarget resolves one file of a finished, listed VOD and the folder
// its previews live in: beside it, or for a read-only scan folder a local
// cache folder, since scan folders are promised never to be written to.
func (a *App) previewTarget(ctx context.Context, dir, file string) (vodindex.VOD, string, error) {
	if file == "" || file != filepath.Base(file) || strings.ContainsAny(file, `/\`) {
		return vodindex.VOD{}, "", fmt.Errorf("%w: bad file name", vlapi.ErrNotFound)
	}
	session, ok := a.vodIndex.Find(ctx, dir)
	if !ok {
		return vodindex.VOD{}, "", fmt.Errorf("%w: not an archived session: %s", vlapi.ErrNotFound, dir)
	}
	if session.State != vodindex.StateReady {
		return vodindex.VOD{}, "", fmt.Errorf("%w: session is still being worked on", vlapi.ErrConflict)
	}
	listed := false
	for _, f := range session.Files {
		listed = listed || f.Name == file
	}
	if !listed {
		return vodindex.VOD{}, "", fmt.Errorf("%w: no such file in session: %s", vlapi.ErrNotFound, file)
	}
	if !a.isReadOnlyVODRoot(session.Root) {
		return session, session.Dir, nil
	}
	sum := sha256.Sum256([]byte(thumbCacheKey(session) + "\x00" + file))
	out := filepath.Join(vlconfig.DefaultCacheFilePath("previews"), hex.EncodeToString(sum[:16]))
	if err := os.MkdirAll(out, 0o755); err != nil {
		return vodindex.VOD{}, "", err
	}
	return session, out, nil
}

// ensurePreviews returns a VOD file's previews, generating them on first
// request. A new three-up thumbnail replaces the cached screenshot.
func (a *App) ensurePreviews(ctx context.Context, dir, file string) (processing.PreviewsManifest, string, error) {
	session, out, err := a.previewTarget(ctx, dir, file)
	if err != nil {
		return processing.PreviewsManifest{}, "", err
	}
	manifest := filepath.Join(out, processing.PreviewsManifestName(file))
	if m, err := processing.ReadPreviewsManifest(manifest); err == nil && m.Version == processing.PreviewsVersion {
		return m, out, nil
	}
	m, err := a.previews.do(out+"\x00"+file, func() (processing.PreviewsManifest, error) {
		if m, err := processing.ReadPreviewsManifest(manifest); err == nil && m.Version == processing.PreviewsVersion {
			return m, nil
		}
		hdr := processing.PreviewHeader{Platform: session.Platform, Username: session.Username, Start: session.Start}
		m, err := processing.WritePreviews(context.WithoutCancel(ctx), a.log.With("service", "previews"), session.Dir, out, file, hdr)
		if err != nil {
			return m, err
		}
		if m.Portrait && a.thumbCache != nil {
			if jpeg, err := os.ReadFile(filepath.Join(out, processing.ThumbnailName(file))); err == nil {
				_ = a.thumbCache.Put(thumbCacheKey(session), file, jpeg)
			}
		}
		return m, nil
	})
	return m, out, err
}

// VODPreviews implements vlapi.VODController: a VOD file's preview layout and
// its hover strip.
func (a *App) VODPreviews(ctx context.Context, dir, file string) (processing.PreviewsManifest, []byte, error) {
	m, out, err := a.ensurePreviews(ctx, dir, file)
	if err != nil {
		return m, nil, err
	}
	strip, err := os.ReadFile(filepath.Join(out, processing.ScrubStripName(file)))
	return m, strip, err
}

// VODContactSheet implements vlapi.VODController.
func (a *App) VODContactSheet(ctx context.Context, dir, file string) ([]byte, error) {
	_, out, err := a.ensurePreviews(ctx, dir, file)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(filepath.Join(out, processing.ContactSheetName(file)))
}

// OpenVODPreview implements webui.PreviewOpener: the timeline-preview track
// of a VOD file (kind "vtt") or one of its sprite sheets ("sheet", n).
func (a *App) OpenVODPreview(ctx context.Context, dir, file, kind string, n int) (*os.File, error) {
	if !a.vodReady.Load() {
		return nil, webui.ErrNotReady
	}
	_, out, err := a.ensurePreviews(ctx, dir, file)
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
		name = fmt.Sprintf("%s%03d.jpg", processing.StoryboardSheetPrefix(file), n)
	default:
		return nil, webui.ErrNotFound
	}
	f, err := os.OpenInRoot(out, name)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", webui.ErrNotFound, err)
	}
	return f, nil
}

// portraitJPEG reports whether a screenshot is taller than wide: one taken
// before three-up thumbnails existed.
func portraitJPEG(b []byte) bool {
	cfg, err := jpeg.DecodeConfig(bytes.NewReader(b))
	return err == nil && cfg.Height > cfg.Width
}

// Compile-time check that App serves the Web UI's timeline previews.
var _ webui.PreviewOpener = (*App)(nil)
