package webui

import (
	"bytes"
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"time"
)

//go:embed signin
var signinFS embed.FS

func (s *Server) serveSignin(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	serveEmbedded(w, r, signinFS, "signin/signin.html")
}

func (s *Server) serveSigninAsset(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-cache")
	serveEmbedded(w, r, signinFS, "signin/"+path.Base(r.URL.Path))
}

func serveEmbedded(w http.ResponseWriter, r *http.Request, fsys fs.FS, name string) {
	b, err := fs.ReadFile(fsys, name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	http.ServeContent(w, r, path.Base(name), time.Time{}, bytes.NewReader(b))
}

// noBuild is served in place of the app by a daemon built without it.
const noBuild = `<!doctype html><meta charset="utf-8"><title>VodLabs Recorder</title>
<p>This daemon was built without the Web UI. Build it with <code>bun run web:build</code>
before <code>go build</code>, or use the Docker image.</p>`

func (s *Server) serveIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if s.cfg.Dist == nil {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(noBuild))
		return
	}
	serveEmbedded(w, r, s.cfg.Dist, "index.html")
}

// serveAsset serves the build's hashed assets, which never change under a
// name, so browsers may keep them for good.
func (s *Server) serveAsset(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
	if s.cfg.Dist == nil || !strings.HasPrefix(name, "assets/") {
		http.NotFound(w, r)
		return
	}
	if _, err := fs.Stat(s.cfg.Dist, name); err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	serveEmbedded(w, r, s.cfg.Dist, name)
}
