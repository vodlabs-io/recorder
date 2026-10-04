package hlsingest

import (
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	m3u8 "github.com/Eyevinn/hls-m3u8/m3u8"
)

func testIngest(t *testing.T, srv *httptest.Server, cfg Config) *ingest {
	t.Helper()
	return &ingest{cfg: cfg, client: srv.Client(), logger: slog.New(slog.DiscardHandler)}
}

// TestMasterPlaylistIsFollowed covers an Origin that answers a per-quality pull
// URL with a one-variant master playlist redirecting to a session-tokenised
// media playlist elsewhere, as Douyin rooms fronted by Wangsu do.
func TestMasterPlaylistIsFollowed(t *testing.T) {
	var polls atomic.Int32
	mux := http.NewServeMux()

	// The pull URL: a master, not the media playlist the caller was promised.
	mux.HandleFunc("/live/pull.m3u8", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "#EXTM3U\n#EXT-X-STREAM-INF:PROGRAM-ID=1, BANDWIDTH=1235000\n")
		fmt.Fprint(w, "/edge/media.m3u8?wsSession=abc\n")
	})
	// The real media playlist, reachable only through that redirect.
	mux.HandleFunc("/edge/media.m3u8", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("wsSession") != "abc" {
			// A session token dropped on the way through is the failure this
			// indirection exists to cause, so make it loud.
			http.Error(w, "missing session", http.StatusForbidden)
			return
		}
		n := polls.Add(1)
		fmt.Fprint(w, "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:2\n#EXT-X-MEDIA-SEQUENCE:0\n")
		fmt.Fprint(w, "#EXTINF:2.000,\ns0.ts?wsSession=abc\n#EXTINF:2.000,\ns1.ts?wsSession=abc\n")
		if n > 1 {
			fmt.Fprint(w, "#EXT-X-ENDLIST\n")
		}
	})
	mux.HandleFunc("/edge/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "TSDATA:%s", r.URL.Path)
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	sink := &capture{}
	if err := ingestOriginAt(t, srv, "/live/pull.m3u8", sink, 1); err != nil {
		t.Fatalf("Run: a master playlist must be followed, not refused: %v", err)
	}
	assertBodies(t, sink.bodies(), "TSDATA:/edge/s0.ts", "TSDATA:/edge/s1.ts")
}

// TestMasterPlaylistHonoursVariant pins that the session's quality preference
// decides at this hop too, so "worst" stays "worst" wherever the choice is made.
func TestMasterPlaylistHonoursVariant(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/live/pull.m3u8", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "#EXTM3U\n")
		// Listed best-first, which a master playlist is free to do.
		fmt.Fprint(w, "#EXT-X-STREAM-INF:BANDWIDTH=5000000,RESOLUTION=1920x1080\nhigh.m3u8\n")
		fmt.Fprint(w, "#EXT-X-STREAM-INF:BANDWIDTH=800000,RESOLUTION=640x360\nlow.m3u8\n")
	})
	for _, name := range []string{"high", "low"} {
		mux.HandleFunc("/live/"+name+".m3u8", func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:2\n#EXT-X-MEDIA-SEQUENCE:0\n")
			fmt.Fprintf(w, "#EXTINF:2.000,\n%s0.ts\n#EXT-X-ENDLIST\n", name)
		})
	}
	srv := httptest.NewServer(mux)
	defer srv.Close()

	for _, tc := range []struct{ variant, wantVariant string }{
		{"worst", "low"},
		{"best", "high"},
		{"1920x1080", "high"},
	} {
		t.Run(tc.variant, func(t *testing.T) {
			o := testIngest(t, srv, Config{URL: srv.URL + "/live/pull.m3u8", Variant: tc.variant})
			got, err := o.resolveMediaPlaylist(o.cfg.URL)
			if err != nil {
				t.Fatalf("resolveMediaPlaylist: %v", err)
			}
			want := tc.wantVariant + ".m3u8"
			if !strings.HasSuffix(got, want) {
				t.Errorf("variant %q resolved to %s, want the %s variant", tc.variant, got, want)
			}
		})
	}
}

// TestMediaPlaylistIsNotRedirected keeps the overwhelmingly common case free of
// the extra indirection: a media playlist is read as-is.
func TestMediaPlaylistIsNotRedirected(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/live/media.m3u8", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:2\n#EXT-X-MEDIA-SEQUENCE:0\n")
		fmt.Fprint(w, "#EXTINF:2.000,\na0.ts\n#EXT-X-ENDLIST\n")
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	o := testIngest(t, srv, Config{URL: srv.URL + "/live/media.m3u8"})
	got, err := o.resolveMediaPlaylist(o.cfg.URL)
	if err != nil {
		t.Fatalf("resolveMediaPlaylist: %v", err)
	}
	if got != o.cfg.URL {
		t.Errorf("resolved to %s, want the URL unchanged", got)
	}
}

// TestMasterLoopIsBounded pins the guard against a misconfigured Origin whose
// master playlist points at itself.
func TestMasterLoopIsBounded(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/live/loop.m3u8", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1000000\nloop.m3u8\n")
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	o := testIngest(t, srv, Config{URL: srv.URL + "/live/loop.m3u8"})
	if _, err := o.resolveMediaPlaylist(o.cfg.URL); err == nil {
		t.Fatal("resolveMediaPlaylist: a self-referential master must be bounded, not followed forever")
	}
}

// An I-frame playlist matches its media variant's resolution, so "best" could
// land on keyframes only.
func TestSelectVariantSkipsIFramePlaylists(t *testing.T) {
	body := "#EXTM3U\n" +
		"#EXT-X-STREAM-INF:BANDWIDTH=5000000,RESOLUTION=1920x1080\nindex-1080.m3u8\n" +
		"#EXT-X-I-FRAME-STREAM-INF:BANDWIDTH=500000,RESOLUTION=1920x1080,URI=\"keyframes/index-1080.m3u8\"\n"
	pl, _, err := m3u8.DecodeFrom(strings.NewReader(body), false)
	if err != nil {
		t.Fatal(err)
	}
	got, err := selectVariant(pl.(*m3u8.MasterPlaylist), "https://cdn.example/live/master.m3u8", "best")
	if err != nil || got != "https://cdn.example/live/index-1080.m3u8" {
		t.Fatalf("selectVariant = %q, %v; want the media variant", got, err)
	}
}
