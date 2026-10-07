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

// TestPlaylistTerminatedOnFetchFailure pins the ordinary end of a live stream:
// the origin stops answering rather than publishing EXT-X-ENDLIST.
//
// An unterminated part playlist reads as live at assembly time — ffmpeg's HLS
// demuxer starts three segments from the end and then blocks reloading — so a
// recording of any length collapses to its last few seconds.
func TestPlaylistTerminatedOnFetchFailure(t *testing.T) {
	var polls atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/live/media.m3u8", func(w http.ResponseWriter, r *http.Request) {
		// Poll 1 is the media-playlist resolve, poll 2 the first recording
		// fetch; the origin drops out once the stream has been captured.
		if polls.Add(1) > 2 {
			http.Error(w, "gone", http.StatusNotFound)
			return
		}
		fmt.Fprint(w, "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:2\n#EXT-X-MEDIA-SEQUENCE:0\n")
		fmt.Fprint(w, "#EXTINF:2.000,\na0.ts\n#EXTINF:2.000,\na1.ts\n")
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
	}, srv.Client(), slog.New(slog.DiscardHandler), progressCh); err == nil {
		t.Fatal("Record: expected the failing poll to surface an error")
	}

	assertEndedPlaylist(t, dir, "000000.ts", "000001.ts")
}

// TestPlaylistTerminatedOnCancel covers the other common exit: the daemon
// cancels the part (stream offline, split limit, shutdown) between polls.
func TestPlaylistTerminatedOnCancel(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/live/media.m3u8", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:2\n#EXT-X-MEDIA-SEQUENCE:0\n")
		fmt.Fprint(w, "#EXTINF:2.000,\na0.ts\n#EXTINF:2.000,\na1.ts\n")
	})
	mux.HandleFunc("/live/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "TSDATA")
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	dir := t.TempDir()
	// Cancelling once the first segment's bytes are reported guarantees it is
	// already recorded, so the assertion below is not a race.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	progressCh := make(chan int64, 100)
	go func() {
		for range progressCh {
			cancel()
		}
	}()

	if err := Record(ctx, Config{
		URL:          srv.URL + "/live/media.m3u8",
		OutDir:       dir,
		PollInterval: 10 * time.Millisecond,
		Retries:      1,
		FileName:     "index.m3u8",
	}, srv.Client(), slog.New(slog.DiscardHandler), progressCh); err != nil {
		t.Fatalf("Record: %v", err)
	}

	assertEndedPlaylist(t, dir, "000000.ts")
}

func assertEndedPlaylist(t *testing.T, dir string, wantSegments ...string) {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(dir, "index.m3u8"))
	if err != nil {
		t.Fatalf("read playlist: %v", err)
	}
	playlist := string(body)
	if !strings.Contains(playlist, "#EXT-X-ENDLIST") {
		t.Errorf("playlist must be terminated:\n%s", playlist)
	}
	for _, seg := range wantSegments {
		if !strings.Contains(playlist, seg) {
			t.Errorf("playlist is missing %q:\n%s", seg, playlist)
		}
	}
}
