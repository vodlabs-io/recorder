package hls

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestFMP4SegmentNaming pins how a fragmented-MP4 origin lands on disk.
//
// CHZZK serves fMP4 fragments under a ".m4v" extension, which ffmpeg's HLS
// demuxer rejects as a mismatch for the container it sniffs, so the segments
// must be renamed on disk.
func TestFMP4SegmentNaming(t *testing.T) {
	var polls atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/live/media.m3u8", func(w http.ResponseWriter, r *http.Request) {
		n := polls.Add(1)
		fmt.Fprint(w, "#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-TARGETDURATION:2\n#EXT-X-MEDIA-SEQUENCE:0\n")
		fmt.Fprint(w, "#EXT-X-MAP:URI=\"init.m4s?type=hls&filetype=.m4s\"\n")
		fmt.Fprint(w, "#EXTINF:2.000,\na0.m4v\n#EXTINF:2.000,\na1.m4v\n")
		if n > 1 {
			fmt.Fprint(w, "#EXT-X-ENDLIST\n")
		}
	})
	mux.HandleFunc("/live/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "MP4DATA:%s", r.URL.Path)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	dir := t.TempDir()
	progressCh := make(chan int64, 100)
	go func() {
		for range progressCh {
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := Record(ctx, Config{
		URL:          srv.URL + "/live/media.m3u8",
		OutDir:       dir,
		PollInterval: 10 * time.Millisecond,
		Retries:      1,
		FileName:     "index.m3u8",
	}, srv.Client(), slog.New(slog.DiscardHandler), progressCh); err != nil {
		t.Fatalf("Record: %v", err)
	}

	body, err := os.ReadFile(filepath.Join(dir, "index.m3u8"))
	if err != nil {
		t.Fatalf("read playlist: %v", err)
	}
	playlist := string(body)

	for _, want := range []string{"000000.m4s", "000001.m4s", `#EXT-X-MAP:URI="init_00.mp4"`} {
		if !strings.Contains(playlist, want) {
			t.Errorf("playlist is missing %q:\n%s", want, playlist)
		}
	}
	if strings.Contains(playlist, ".m4v") {
		t.Errorf("the origin's .m4v extension survived into the playlist:\n%s", playlist)
	}
	// EXT-X-MAP on media segments requires version 6 (RFC 8216 §7).
	if !strings.Contains(playlist, "#EXT-X-VERSION:6") {
		t.Errorf("fMP4 playlist must declare version 6:\n%s", playlist)
	}
	for _, name := range []string{"000000.m4s", "init_00.mp4"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("expected %s on disk: %v", name, err)
		}
	}
}

// TestTSSegmentNamingUnchanged guards the far more common path: a transport
// stream origin must keep the naming and version it has always had.
func TestTSSegmentNamingUnchanged(t *testing.T) {
	var polls atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/live/media.m3u8", func(w http.ResponseWriter, r *http.Request) {
		n := polls.Add(1)
		fmt.Fprint(w, "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:2\n#EXT-X-MEDIA-SEQUENCE:0\n")
		fmt.Fprint(w, "#EXTINF:2.000,\na0.ts\n#EXTINF:2.000,\na1.ts\n")
		if n > 1 {
			fmt.Fprint(w, "#EXT-X-ENDLIST\n")
		}
	})
	mux.HandleFunc("/live/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "TSDATA")
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	dir := t.TempDir()
	progressCh := make(chan int64, 100)
	go func() {
		for range progressCh {
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := Record(ctx, Config{
		URL:          srv.URL + "/live/media.m3u8",
		OutDir:       dir,
		PollInterval: 10 * time.Millisecond,
		Retries:      1,
		FileName:     "index.m3u8",
	}, srv.Client(), slog.New(slog.DiscardHandler), progressCh); err != nil {
		t.Fatalf("Record: %v", err)
	}

	body, err := os.ReadFile(filepath.Join(dir, "index.m3u8"))
	if err != nil {
		t.Fatalf("read playlist: %v", err)
	}
	playlist := string(body)
	if !strings.Contains(playlist, "000000.ts") {
		t.Errorf("transport stream segments must keep .ts:\n%s", playlist)
	}
	if !strings.Contains(playlist, "#EXT-X-VERSION:3") {
		t.Errorf("non-fMP4 output must stay at version 3:\n%s", playlist)
	}
}

// TestUnknownSegmentExtensionNamedTS: a transport stream segment whose Origin
// extension ffmpeg's HLS demuxer refuses (MyFreeCams' ".pts") is named ".ts".
func TestUnknownSegmentExtensionNamedTS(t *testing.T) {
	var polls atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/live/media.m3u8", func(w http.ResponseWriter, r *http.Request) {
		n := polls.Add(1)
		fmt.Fprint(w, "#EXTM3U\n#EXT-X-VERSION:5\n#EXT-X-TARGETDURATION:2\n#EXT-X-MEDIA-SEQUENCE:0\n")
		fmt.Fprint(w, "#EXTINF:1.000,\nstream_b1.msn0.pts\n#EXTINF:1.000,\nstream_b1.msn1.PTS\n#EXTINF:1.000,\nseg2.aac\n")
		if n > 1 {
			fmt.Fprint(w, "#EXT-X-ENDLIST\n")
		}
	})
	mux.HandleFunc("/live/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "TSDATA")
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := Record(ctx, Config{
		URL:          srv.URL + "/live/media.m3u8",
		OutDir:       dir,
		PollInterval: 10 * time.Millisecond,
		Retries:      1,
		FileName:     "index.m3u8",
	}, srv.Client(), slog.New(slog.DiscardHandler), nil); err != nil {
		t.Fatalf("Record: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(dir, "index.m3u8"))
	if err != nil {
		t.Fatalf("read playlist: %v", err)
	}
	for _, want := range []string{"000000.ts", "000001.ts", "000002.aac"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("playlist lacks %s:\n%s", want, body)
		}
		if _, err := os.Stat(filepath.Join(dir, want)); err != nil {
			t.Errorf("segment file: %v", err)
		}
	}
}
