package webui

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
)

// PreviewOpener opens the timeline previews of a finished recording's file.
// A FileOpener that also implements it gets the preview route.
type PreviewOpener interface {
	// OpenVODPreview opens file's WebVTT track (kind "vtt") or sprite sheet
	// n (kind "sheet"), generating them on first request.
	OpenVODPreview(ctx context.Context, dir, file, kind string, n int) (*os.File, error)
}

// sheetRef matches a cue's sprite sheet, as previews name them: thumbs.000.jpg,
// or vod1.thumbs.000.jpg from older recorders.
var sheetRef = regexp.MustCompile(`(?m)^(?:[^\s#/]+\.)?thumbs\.(\d{3})\.jpg(#xywh=)`)

// handleVODPreview serves a recording's timeline previews for the player's
// seek bar. The track names its sheets relative to itself on disk; here each
// becomes a request to this route, which a browser resolves against the
// track's own URL.
func (m *media) handleVODPreview(w http.ResponseWriter, r *http.Request) {
	po, ok := m.files.(PreviewOpener)
	if !ok {
		http.NotFound(w, r)
		return
	}
	q := r.URL.Query()
	dir, file, kind := q.Get("dir"), q.Get("file"), q.Get("kind")
	n, _ := strconv.Atoi(q.Get("n"))
	f, err := po.OpenVODPreview(r.Context(), dir, file, kind, n)
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
	h := w.Header()
	h.Set("Cache-Control", "private, no-cache")
	h.Set("Content-Security-Policy", "default-src 'none'; sandbox")
	if kind != "vtt" {
		h.Set("Content-Type", "image/jpeg")
		http.ServeContent(w, r, "sheet.jpg", info.ModTime(), f)
		return
	}
	body, err := io.ReadAll(io.LimitReader(f, 8<<20))
	if err != nil {
		http.Error(w, "read failed", http.StatusInternalServerError)
		return
	}
	base := "preview?dir=" + url.QueryEscape(dir) + "&file=" + url.QueryEscape(file) + "&kind=sheet&n="
	body = sheetRef.ReplaceAllFunc(body, func(m []byte) []byte {
		sub := sheetRef.FindSubmatch(m)
		n, _ := strconv.Atoi(string(sub[1]))
		return append([]byte(base+strconv.Itoa(n)), sub[2]...)
	})
	h.Set("Content-Type", "text/vtt; charset=utf-8")
	http.ServeContent(w, r, "thumbs.vtt", info.ModTime(), bytes.NewReader(body))
}
