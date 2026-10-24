package webui

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// dirFiles serves name from one folder, like the daemon's opener.
type dirFiles struct{ dir, name string }

func (d dirFiles) OpenVODFile(_ context.Context, dir, file string) (*os.File, error) {
	if dir != d.dir || file != d.name {
		return nil, ErrNotFound
	}
	return os.Open(filepath.Join(d.dir, file))
}

func TestVODFile(t *testing.T) {
	dir := t.TempDir()
	body := strings.Repeat("0123456789", 100)
	if err := os.WriteFile(filepath.Join(dir, "vod 1.mp4"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, func(c *Config) { c.Files = dirFiles{dir, "vod 1.mp4"} })
	c := h.client()
	q := "api/vods/file?dir=" + dir + "&file=vod%201.mp4"

	if code := h.do(c, "GET", q, "").StatusCode; code != 401 {
		t.Errorf("without a session = %d", code)
	}
	h.signIn(c, testToken)

	res, err := c.Get(h.base + q)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != 200 || string(b) != body || res.Header.Get("Content-Type") != "video/mp4" {
		t.Errorf("GET = %d %s, %d bytes", res.StatusCode, res.Header.Get("Content-Type"), len(b))
	}
	if res.Header.Get("Accept-Ranges") != "bytes" || res.Header.Get("Content-Disposition") != "" {
		t.Errorf("headers = %v", res.Header)
	}

	req, _ := http.NewRequest("GET", h.base+q, nil)
	req.Header.Set("Range", "bytes=10-19")
	res, err = c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ = io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusPartialContent || string(b) != "0123456789" {
		t.Errorf("Range = %d %q", res.StatusCode, b)
	}

	head := h.do(c, "HEAD", q, "")
	if head.StatusCode != 200 || head.ContentLength != int64(len(body)) {
		t.Errorf("HEAD = %d, length %d", head.StatusCode, head.ContentLength)
	}

	dl := h.do(c, "GET", q+"&download=1", "")
	if cd := dl.Header.Get("Content-Disposition"); cd != "attachment; filename*=UTF-8''vod%201.mp4" {
		t.Errorf("Content-Disposition = %q", cd)
	}

	for _, bad := range []string{
		"api/vods/file?dir=/etc&file=passwd",
		"api/vods/file?dir=" + dir + "&file=../vod%201.mp4",
	} {
		if code := h.do(c, "GET", bad, "").StatusCode; code != 404 {
			t.Errorf("%s = %d", bad, code)
		}
	}
}

func TestContentTypeNeverRunsAsPage(t *testing.T) {
	for _, name := range []string{"x.html", "x.svg", "x.js", "x.unknown"} {
		if ct := contentType(name); ct != "application/octet-stream" {
			t.Errorf("contentType(%s) = %s", name, ct)
		}
	}
}
