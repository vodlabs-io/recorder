package webui

import (
	"context"
	"io"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/vodlabs-io/recorder/internal/ffmpeg"
)

// sessionFiles serves a recording's files from one folder, in order.
type sessionFiles struct {
	dir   string
	names []string
}

func (s sessionFiles) OpenVODFile(_ context.Context, dir, file string) (*os.File, error) {
	for _, n := range s.names {
		if dir == s.dir && file == n {
			return os.Open(filepath.Join(s.dir, file))
		}
	}
	return nil, ErrNotFound
}

func (s sessionFiles) VODFiles(_ context.Context, dir string) ([]string, error) {
	if dir != s.dir {
		return nil, ErrNotFound
	}
	return s.names, nil
}

func recordPart(t *testing.T, dir, name string, seconds int) {
	t.Helper()
	cmd, err := ffmpeg.Command(context.Background(), slog.Default(),
		"-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc=size=160x120:rate=30:duration="+strconv.Itoa(seconds),
		"-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p", "-g", "60", filepath.Join(dir, name))
	if err != nil {
		t.Skip(err)
	}
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("no usable ffmpeg: %v %s", err, b)
	}
}

func TestHLSPlaysEveryFile(t *testing.T) {
	dir := t.TempDir()
	recordPart(t, dir, "vod1.mp4", 8)
	recordPart(t, dir, "vod2.mp4", 4)
	files := sessionFiles{dir, []string{"vod1.mp4", "vod2.mp4"}}
	h := newHarness(t, func(c *Config) { c.Files = files })
	c := h.client()
	h.signIn(c, testToken)
	get := func(path string) (int, string) {
		t.Helper()
		res, err := c.Get(h.base + path)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return res.StatusCode, string(b)
	}
	d := url.QueryEscape(dir)

	code, m3u8 := get("api/vods/hls/index.m3u8?dir=" + d + "&start=vod2.mp4")
	if code != 200 || strings.Count(m3u8, "#EXT-X-DISCONTINUITY") != 1 || strings.Count(m3u8, "#EXT-X-MAP") != 2 ||
		!strings.Contains(m3u8, "#EXT-X-START:TIME-OFFSET=8.000") || !strings.HasSuffix(m3u8, "#EXT-X-ENDLIST\n") {
		t.Fatalf("playlist = %d\n%s", code, m3u8)
	}
	for _, line := range strings.Split(m3u8, "\n") {
		uri := line
		if after, ok := strings.CutPrefix(line, `#EXT-X-MAP:URI="`); ok {
			uri = strings.TrimSuffix(after, `"`)
		} else if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if code, body := get("api/vods/hls/" + uri); code != 200 || len(body) == 0 {
			t.Fatalf("%s = %d", uri, code)
		}
	}

	if code, vtt := get("api/vods/hls/chapters.vtt?dir=" + d); code != 200 ||
		!strings.Contains(vtt, "00:00:08.000 --> 00:00:12.000\nPart 2 of 2") {
		t.Fatalf("chapters = %d\n%s", code, vtt)
	}
	if code, _ := get("api/vods/hls/index.m3u8?dir=elsewhere"); code != 404 {
		t.Fatalf("unknown recording = %d", code)
	}
}

func TestHLSFallsBackForNonMP4(t *testing.T) {
	dir := t.TempDir()
	recordPart(t, dir, "vod1.mp4", 2)
	if err := os.WriteFile(filepath.Join(dir, "vod2.webm"), []byte("\x1aE\xdf\xa3"), 0o644); err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, func(c *Config) { c.Files = sessionFiles{dir, []string{"vod1.mp4", "vod2.webm"}} })
	c := h.client()
	h.signIn(c, testToken)
	if code := h.do(c, "GET", "api/vods/hls/index.m3u8?dir="+url.QueryEscape(dir), "").StatusCode; code != 404 {
		t.Fatalf("playlist with a WebM file = %d", code)
	}
}
