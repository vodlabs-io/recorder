package webui

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/vodlabs-io/recorder/internal/vodhls"
)

// media serves finished recordings for in-page players: their files, their
// timeline previews, and every file of one as a single HLS stream.
type media struct {
	files FileOpener
	hls   *vodhls.Cache
	log   *slog.Logger
}

func newMedia(files FileOpener, log *slog.Logger) *media {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &media{files: files, hls: vodhls.NewCache(hlsCacheSamples), log: log}
}

// routes adds the media routes to mux, each behind gate.
func (m *media) routes(mux *http.ServeMux, gate func(http.HandlerFunc) http.HandlerFunc) {
	mux.HandleFunc("GET /api/vods/file", gate(m.handleVODFile))
	mux.HandleFunc("HEAD /api/vods/file", gate(m.handleVODFile))
	mux.HandleFunc("GET /api/vods/preview", gate(m.handleVODPreview))
	mux.HandleFunc("GET /api/vods/hls/index.m3u8", gate(m.handleHLSPlaylist))
	mux.HandleFunc("GET /api/vods/hls/init.mp4", gate(m.handleHLSInit))
	mux.HandleFunc("GET /api/vods/hls/seg.m4s", gate(m.handleHLSSegment))
	mux.HandleFunc("GET /api/vods/hls/chapters.vtt", gate(m.handleHLSChapters))
	mux.HandleFunc("GET /api/vods/hls/thumbs.vtt", gate(m.handleHLSThumbnails))
}

// LocalMedia serves the same media routes to the desktop app on a loopback
// port. A random path prefix stands in for the Web UI's session cookie: a
// media element can't send a header, and the app's page is another origin,
// so responses allow any origin and the prefix is the only key.
type LocalMedia struct {
	srv  *http.Server
	base string
}

// StartLocalMedia listens on a free loopback port.
func StartLocalMedia(files FileOpener, log *slog.Logger) (*LocalMedia, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	key := make([]byte, 24)
	if _, err := rand.Read(key); err != nil {
		ln.Close()
		return nil, err
	}
	prefix := "/" + hex.EncodeToString(key)
	m := newMedia(files, log)
	mux := http.NewServeMux()
	m.routes(mux, func(h http.HandlerFunc) http.HandlerFunc { return h })
	l := &LocalMedia{base: "http://" + ln.Addr().String() + prefix + "/"}
	l.srv = &http.Server{
		Handler:           localGate(prefix, mux),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    16 << 10,
		ErrorLog:          slog.NewLogLogger(m.log.Handler(), slog.LevelDebug),
	}
	go l.srv.Serve(ln)
	return l, nil
}

// localGate passes on requests under prefix, with it stripped, and refuses
// the rest.
func localGate(prefix string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Access-Control-Allow-Origin", "*")
		p := r.URL.Path
		if len(p) <= len(prefix) || p[len(prefix)] != '/' || subtle.ConstantTimeCompare([]byte(p[:len(prefix)]), []byte(prefix)) != 1 {
			http.NotFound(w, r)
			return
		}
		if r.Method == http.MethodOptions {
			h.Set("Access-Control-Allow-Methods", "GET, HEAD")
			h.Set("Access-Control-Allow-Headers", "Range")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		r2 := r.Clone(r.Context())
		r2.URL.Path = p[len(prefix):]
		r2.URL.RawPath = ""
		next.ServeHTTP(w, r2)
	})
}

// Base is the URL the media routes sit under, ending in a slash.
func (l *LocalMedia) Base() string { return l.base }

// Close stops the listener.
func (l *LocalMedia) Close() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = l.srv.Shutdown(ctx)
}
