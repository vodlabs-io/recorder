package webpage

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/vodlabs-io/recorder/internal/browserfetch"
	"github.com/vodlabs-io/recorder/internal/streamer"
)

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

// Slugs are identities: these vectors must never change.
func TestSlugVectors(t *testing.T) {
	for in, want := range map[string]string{
		"https://example.com/live":                                      "example.com-2369eb15fe",
		"example.com":                                                   "example.com-0f115db062",
		"https://example.com":                                           "example.com-0f115db062",
		"HTTPS://WWW.Example.COM:443/live#x":                            "example.com-cc93b0944f",
		"https://www.example.com/live":                                  "example.com-cc93b0944f",
		"https://cdn.example.com/hls/stream.m3u8?token=abc":             "cdn.example.com-9c83abc08f",
		"https://a.very.long.subdomain.chain.of.labels.example.co.uk/x": "labels.example.co.uk-b5264f7bb2",
		"https://abcdefghijklmnopqrstuvwxyz0123456789.com/":             "tuvwxyz0123456789.com-ed298ebb05",
		"https://bücher.example/live":                                   "b-cher.example-2c9441a457",
	} {
		got, err := Slug(in)
		if err != nil || got != want {
			t.Errorf("Slug(%q) = %q, %v; want %q", in, got, err, want)
		}
		if len(got) > maxSlugLength {
			t.Errorf("Slug(%q) is %d characters", in, len(got))
		}
	}
}

func TestCanonicalRefuses(t *testing.T) {
	for in, ok := range map[string]bool{
		"https://example.com/live":      true,
		"example.com/live":              true,
		"http://news.example.org:8080/": true,
		"https://example.com/a.m3u8":    true,
		"ftp://example.com/x":           false,
		"https://localhost/x":           false,
		"https://127.0.0.1/x":           false,
		"https://10.0.0.5/live.m3u8":    false,
		"https://169.254.169.254/":      false,
		"https://[::1]/":                false,
		"https://user:pw@example.com/":  false,
		"https://intranet/x":            false,
		"just words":                    false,
		"https://example.com/" + strings.Repeat("a", maxURLLength): false,
	} {
		if _, err := Canonical(in); (err == nil) != ok {
			t.Errorf("Canonical(%q) err = %v, want ok=%v", in, err, ok)
		}
	}
}

func TestCleanLabel(t *testing.T) {
	for in, want := range map[string]string{
		"  News \n\t Live  ":     "News Live",
		"a‮b​c\x07d":             "abcd",
		strings.Repeat("x", 500): strings.Repeat("x", maxLabelRunes),
	} {
		if got := cleanLabel(in); got != want {
			t.Errorf("cleanLabel(%q) = %q, want %q", in, got, want)
		}
	}
}

// allowLoopback lets the tests' own servers through the host check.
func allowLoopback(t *testing.T) {
	saved := blockedHost
	blockedHost = func(string) bool { return false }
	t.Cleanup(func() { blockedHost = saved })
}

// site serves fixed bodies by path and records the Referer each path saw.
type site struct {
	mu       sync.Mutex
	routes   map[string]func(w http.ResponseWriter)
	referers map[string]string
}

func newSite(t *testing.T) (*site, *httptest.Server) {
	allowLoopback(t)
	s := &site{routes: map[string]func(http.ResponseWriter){}, referers: map[string]string{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.referers[r.URL.Path] = r.Header.Get("Referer")
		h := s.routes[r.URL.Path]
		s.mu.Unlock()
		if h == nil {
			http.NotFound(w, r)
			return
		}
		h(w)
	}))
	t.Cleanup(srv.Close)
	return s, srv
}

func (s *site) body(path, body string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.routes[path] = func(w http.ResponseWriter) { io.WriteString(w, body) }
}

func (s *site) status(path string, code int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.routes[path] = func(w http.ResponseWriter) { w.WriteHeader(code) }
}

func (s *site) referer(path string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.referers[path]
	return r, ok
}

func fixture(t *testing.T, name, base string) string {
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	out := strings.ReplaceAll(string(b), "https://cdn.example.net", base)
	return strings.ReplaceAll(out, `https:\/\/cdn.example.net`, strings.ReplaceAll(base, "/", `\/`))
}

const (
	liveMedia  = "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:4\n#EXT-X-MEDIA-SEQUENCE:100\n#EXTINF:4.0,\nseg100.ts\n#EXTINF:4.0,\nseg101.ts\n"
	endedMedia = liveMedia + "#EXT-X-ENDLIST\n"
	vodMedia   = "#EXTM3U\n#EXT-X-PLAYLIST-TYPE:VOD\n#EXT-X-TARGETDURATION:4\n#EXTINF:4.0,\nseg0.ts\n#EXT-X-ENDLIST\n"
	master     = "#EXTM3U\n#EXT-X-I-FRAME-STREAM-INF:BANDWIDTH=90000,URI=\"iframes.m3u8\"\n#EXT-X-STREAM-INF:BANDWIDTH=800000,RESOLUTION=640x360\nlow/index.m3u8\n#EXT-X-STREAM-INF:BANDWIDTH=3000000,RESOLUTION=1280x720\nhigh/index.m3u8\n"
)

func check(t *testing.T, rawURL string) (finding, error) {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	return readStatic(context.Background(), http.DefaultClient, u, discard)
}

func TestStaticEscapedJSONMaster(t *testing.T) {
	s, srv := newSite(t)
	s.body("/watch", fixture(t, "escaped_json.html", srv.URL))
	s.body("/live/ch7/master.m3u8", master)
	s.body("/live/ch7/low/index.m3u8", liveMedia)

	f, err := check(t, srv.URL+"/watch")
	if err != nil || !f.live || f.format != formatHLS {
		t.Fatalf("static = %+v, %v; want live HLS", f, err)
	}
	if !strings.Contains(f.stream, "/live/ch7/master.m3u8") {
		t.Fatalf("stream = %s, want the master (the writer picks the variant)", f.stream)
	}
	if f.headers["Referer"] != srv.URL+"/watch" || f.headers["Origin"] != srv.URL {
		t.Fatalf("headers = %v, want the page as Referer and its origin", f.headers)
	}
	if r, _ := s.referer("/live/ch7/master.m3u8"); r != srv.URL+"/watch" {
		t.Fatalf("probe Referer = %q", r)
	}
	if _, probed := s.referer("/live/ch7/iframes.m3u8"); probed {
		t.Fatal("followed the I-frame playlist")
	}
	if f.title != "Channel 7 Live" || f.siteName != "Channel 7" {
		t.Fatalf("finding = %+v", f)
	}
}

func TestStaticRelativeVideoSource(t *testing.T) {
	s, srv := newSite(t)
	s.body("/tv/", fixture(t, "relative_video.html", srv.URL))
	s.body("/hls/live.m3u8", liveMedia)

	f, err := check(t, srv.URL+"/tv/")
	if err != nil || !f.live || f.stream != srv.URL+"/hls/live.m3u8" {
		t.Fatalf("static = %+v, %v", f, err)
	}
	if f.title != "Town & Country TV" {
		t.Fatalf("title = %q", f.title)
	}
}

func TestStaticOGVideo(t *testing.T) {
	s, srv := newSite(t)
	s.body("/p", fixture(t, "og_video.html", srv.URL))
	s.body("/og/live.m3u8", liveMedia)
	if f, err := check(t, srv.URL+"/p"); err != nil || !f.live {
		t.Fatalf("static = %+v, %v", f, err)
	}
}

func TestStaticDirectPlaylists(t *testing.T) {
	s, srv := newSite(t)
	s.body("/live.m3u8", liveMedia)
	s.body("/ended.m3u8", endedMedia)
	s.body("/vod.m3u8", vodMedia)
	s.body("/master.m3u8", master)
	s.body("/low/index.m3u8", vodMedia)
	s.body("/stream", liveMedia) // no extension, sniffed from the body
	s.body("/live.flv", "FLV\x01\x05\x00\x00\x00\x09rest-of-stream")
	s.status("/locked.m3u8", http.StatusForbidden)

	for path, live := range map[string]bool{
		"/live.m3u8": true, "/ended.m3u8": false, "/vod.m3u8": false,
		"/master.m3u8": false, "/stream": true, "/live.flv": true,
		// Refused to this machine is refused to the recording too.
		"/locked.m3u8": false,
	} {
		f, err := check(t, srv.URL+path)
		if err != nil || f.live != live {
			t.Errorf("%s: static = %+v, %v; want live=%v", path, f, err, live)
			continue
		}
		if live && f.headers != nil {
			t.Errorf("%s: a direct stream carries page headers %v", path, f.headers)
		}
	}
	if f, _ := check(t, srv.URL+"/live.flv"); f.format != formatFLV {
		t.Fatalf("FLV format = %q", f.format)
	}
}

func TestStaticOfflineAndUnknown(t *testing.T) {
	s, srv := newSite(t)
	s.body("/none", fixture(t, "no_stream.html", srv.URL))
	s.status("/gone", http.StatusGone)
	s.status("/broken", http.StatusInternalServerError)

	for _, path := range []string{"/none", "/gone", "/missing.m3u8"} {
		f, err := check(t, srv.URL+path)
		if err != nil || f.live {
			t.Errorf("%s: static = %+v, %v; want offline", path, f, err)
		}
	}
	if _, err := check(t, srv.URL+"/broken"); err == nil {
		t.Error("a 500 page read as an answer")
	}
}

func TestStaticProbesAtMostThree(t *testing.T) {
	s, srv := newSite(t)
	s.body("/page", fixture(t, "many.html", srv.URL))
	s.body("/4.m3u8", liveMedia)
	f, err := check(t, srv.URL+"/page")
	if err != nil || f.live {
		t.Fatalf("static = %+v, %v; want offline after three dead probes", f, err)
	}
	if _, probed := s.referer("/4.m3u8"); probed {
		t.Fatal("probed a fourth candidate")
	}
}

func TestFromSniffTakesTheFirstUsableCandidate(t *testing.T) {
	page, _ := url.Parse("https://www.news.example/live")
	f := fromSniff(page, browserfetch.SniffResponse{Title: "News Live", Candidates: []browserfetch.SniffCandidate{
		{URL: "https://imasdk.googleapis.com/ad.m3u8", Format: "hls", Frame: "https://www.news.example/live"},
		{URL: "blob:https://www.news.example/1", Format: "hls"},
		{URL: "https://10.0.0.1/x.m3u8", Format: "hls"},
		{URL: "http://localhost:8080/x.m3u8", Format: "hls"},
		{URL: "https://cdn.player.example/live/index.m3u8?t=1", Format: "hls", Frame: "https://geo.player.example/embed/x"},
		{URL: "https://cdn.other.example/b.flv", Format: "flv"},
	}})
	if !f.live || f.stream != "https://cdn.player.example/live/index.m3u8?t=1" || f.format != formatHLS {
		t.Fatalf("finding = %+v", f)
	}
	if f.headers["Referer"] != "https://geo.player.example/embed/x" || f.headers["Origin"] != "https://geo.player.example" {
		t.Fatalf("headers = %v, want the embedding frame's", f.headers)
	}
}

// Without a desktop app the resolver reads the HTML; the page it was given is
// what later checks read, and a removed streamer's page is forgotten.
func TestResolverLifecycle(t *testing.T) {
	s, srv := newSite(t)
	s.body("/live.m3u8", liveMedia)
	r := New(nil, discard)
	r.client = http.DefaultClient

	key, err := r.Preflight(context.Background(), srv.URL+"/live.m3u8")
	if err != nil || key.Platform != Platform {
		t.Fatalf("Preflight = %v, %v", key, err)
	}
	if k, _ := KeyFor(srv.URL + "/live.m3u8"); k != key {
		t.Fatalf("KeyFor = %v, want %v", k, key)
	}
	str, err := r.Check(context.Background(), key, false)
	if err != nil || str.Status != streamer.StatusLive || len(str.Streams) != 1 || str.Streams[0].Format != formatHLS {
		t.Fatalf("Check = %+v, %v", str, err)
	}

	s.body("/live.m3u8", endedMedia)
	if str, _ := r.Check(context.Background(), key, false); str.Status != streamer.StatusLive {
		t.Fatal("a check within the cache window read the page again")
	}
	if str, _ := r.Check(context.Background(), key, true); str.Status != streamer.StatusOffline {
		t.Fatalf("a fresh check of an ended stream = %s", str.Status)
	}

	r.Forget(key)
	if str, err := r.Check(context.Background(), key, true); err != nil || str.Status != streamer.StatusOffline {
		t.Fatalf("a forgotten page: %+v, %v", str, err)
	}
	if _, err := r.Preflight(context.Background(), srv.URL+"/ended.m3u8"); !errors.Is(err, ErrNotLive) {
		t.Fatalf("Preflight of a dead stream = %v, want ErrNotLive", err)
	}
}

// The real client refuses this machine whatever the URL checks said.
func TestGuardedClientRefusesLoopback(t *testing.T) {
	_, srv := newSite(t)
	if _, err := guardedClient().Get(srv.URL); err == nil {
		t.Fatal("the guarded client reached a loopback server")
	}
}

// A playlist under a path that names an FLV (EarthCam's layout) is one address.
func TestStaticPlaylistUnderAnFLVPath(t *testing.T) {
	s, srv := newSite(t)
	esc := strings.ReplaceAll(srv.URL, "/", `\/`)
	s.body("/cam", `<script>var cams={"a":{"livestreamingpath":"\/fecnetwork\/1.flv",`+
		`"stream":"`+esc+`\/fecnetwork\/1.flv\/playlist.m3u8?t=abc%2Bd"}};</script>`)
	s.body("/fecnetwork/1.flv/playlist.m3u8", liveMedia)
	f, err := check(t, srv.URL+"/cam")
	if err != nil || !f.live || f.stream != srv.URL+"/fecnetwork/1.flv/playlist.m3u8?t=abc%2Bd" || f.format != formatHLS {
		t.Fatalf("static = %+v, %v", f, err)
	}
}
