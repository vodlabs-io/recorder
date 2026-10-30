package webui

import (
	"context"
	"errors"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// FileOpener opens one file of a finished recording.
type FileOpener interface {
	// OpenVODFile opens file, a bare file name, in the recording folder dir.
	// It returns ErrNotFound for anything it won't serve and ErrNotReady
	// before the recordings index is up.
	OpenVODFile(ctx context.Context, dir, file string) (*os.File, error)
}

var (
	ErrNotFound = errors.New("not a file of a finished recording")
	ErrNotReady = errors.New("recordings are not indexed yet")
)

// mediaTypes covers what recordings are made of. The runtime image has no
// mime database, and Go's own table lacks video types.
var mediaTypes = map[string]string{
	".mp4":  "video/mp4",
	".m4v":  "video/mp4",
	".m4a":  "audio/mp4",
	".mov":  "video/quicktime",
	".webm": "video/webm",
	".mkv":  "video/x-matroska",
	".ts":   "video/mp2t",
	".flv":  "video/x-flv",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".png":  "image/png",
	".json": "application/json",
	".vtt":  "text/vtt; charset=utf-8",
	".txt":  "text/plain; charset=utf-8",
}

func contentType(name string) string {
	ext := strings.ToLower(filepath.Ext(name))
	if t, ok := mediaTypes[ext]; ok {
		return t
	}
	if t := mime.TypeByExtension(ext); t != "" && !strings.Contains(t, "html") && !strings.Contains(t, "javascript") && !strings.Contains(t, "svg") {
		return t
	}
	return "application/octet-stream"
}

// handleVODFile serves a recording's file with Range support, so a <video>
// can seek. download=1 asks the browser to save it instead.
func (m *media) handleVODFile(w http.ResponseWriter, r *http.Request) {
	if m.files == nil {
		http.NotFound(w, r)
		return
	}
	q := r.URL.Query()
	f, err := m.files.OpenVODFile(r.Context(), q.Get("dir"), q.Get("file"))
	switch {
	case errors.Is(err, ErrNotReady):
		w.Header().Set("Retry-After", "5")
		writeStatus(w, http.StatusServiceUnavailable)
		return
	case err != nil:
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		http.NotFound(w, r)
		return
	}
	name := filepath.Base(q.Get("file"))
	h := w.Header()
	h.Set("Content-Type", contentType(name))
	h.Set("Cache-Control", "private, no-cache")
	// Nothing served here is a page: never run it as one.
	h.Set("Content-Security-Policy", "default-src 'none'; sandbox")
	if q.Get("download") == "1" {
		h.Set("Content-Disposition", "attachment; filename*=UTF-8''"+url.PathEscape(name))
	}
	http.ServeContent(w, r, name, info.ModTime(), f)
}
