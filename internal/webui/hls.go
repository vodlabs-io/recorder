package webui

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/vodlabs-io/recorder/internal/preview"
	"github.com/vodlabs-io/recorder/internal/vodhls"
)

// SessionLister lists a finished recording's files in play order. A
// FileOpener that also implements it gets the HLS routes, which play every
// file of a recording as one stream.
type SessionLister interface {
	VODFiles(ctx context.Context, dir string) ([]string, error)
}

// hlsCacheSamples bounds the moovs kept for segmenting: a few long files'
// worth, tens of MB.
const hlsCacheSamples = 2_000_000

// hlsPart is one file of a recording on the stream's timeline.
type hlsPart struct {
	name   string
	offset float64
	plan   *vodhls.Plan
}

// hlsParts plans every file of the recording in dir. Any file that can't be
// segmented fails the lot, so the player falls back to one file at a time.
func (m *media) hlsParts(ctx context.Context, dir string) ([]hlsPart, error) {
	sl, ok := m.files.(SessionLister)
	if !ok {
		return nil, ErrNotFound
	}
	names, err := sl.VODFiles(ctx, dir)
	if err != nil {
		return nil, err
	}
	if len(names) == 0 {
		return nil, ErrNotFound
	}
	parts := make([]hlsPart, 0, len(names))
	var offset float64
	for _, name := range names {
		f, p, err := m.openPlan(ctx, dir, name)
		if err != nil {
			return nil, err
		}
		f.Close()
		p.offset = offset
		offset += p.plan.Duration()
		parts = append(parts, p)
	}
	return parts, nil
}

// openPlan opens one file of a recording and plans it. The caller closes
// the file.
func (m *media) openPlan(ctx context.Context, dir, name string) (*os.File, hlsPart, error) {
	f, err := m.files.OpenVODFile(ctx, dir, name)
	if err != nil {
		return nil, hlsPart{}, err
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		f.Close()
		return nil, hlsPart{}, ErrNotFound
	}
	key := dir + "\x00" + name + "\x00" + strconv.FormatInt(info.Size(), 10) + "\x00" + strconv.FormatInt(info.ModTime().UnixNano(), 10)
	plan, err := m.hls.Plan(key, f)
	if err != nil {
		f.Close()
		return nil, hlsPart{}, err
	}
	return f, hlsPart{name: name, plan: plan}, nil
}

// hlsError answers a request the HLS routes can't serve.
func hlsError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, ErrNotReady) {
		w.Header().Set("Retry-After", "5")
		writeStatus(w, http.StatusServiceUnavailable)
		return
	}
	http.NotFound(w, r)
}

func hlsHeaders(w http.ResponseWriter, contentType string) {
	h := w.Header()
	h.Set("Content-Type", contentType)
	h.Set("Cache-Control", "private, no-cache")
	h.Set("Content-Security-Policy", "default-src 'none'; sandbox")
}

// handleHLSPlaylist serves a recording's files as one VOD playlist, a
// discontinuity at each file boundary. start names the file to start at.
func (m *media) handleHLSPlaylist(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	dir := q.Get("dir")
	parts, err := m.hlsParts(r.Context(), dir)
	if err != nil {
		hlsError(w, r, err)
		return
	}
	target := 1.0
	for _, p := range parts {
		for _, d := range p.plan.Durations() {
			target = max(target, math.Ceil(d))
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-TARGETDURATION:%d\n#EXT-X-PLAYLIST-TYPE:VOD\n#EXT-X-INDEPENDENT-SEGMENTS\n", int(target))
	for _, p := range parts {
		if p.name == q.Get("start") && p.offset > 0 {
			fmt.Fprintf(&b, "#EXT-X-START:TIME-OFFSET=%.3f,PRECISE=YES\n", p.offset)
		}
	}
	ref := "dir=" + url.QueryEscape(dir) + "&file="
	for i, p := range parts {
		if i > 0 {
			b.WriteString("#EXT-X-DISCONTINUITY\n")
		}
		file := ref + url.QueryEscape(p.name)
		fmt.Fprintf(&b, "#EXT-X-MAP:URI=\"init.mp4?%s\"\n", file)
		for n, d := range p.plan.Durations() {
			fmt.Fprintf(&b, "#EXTINF:%.3f,\nseg.m4s?%s&n=%d\n", d, file, n)
		}
	}
	b.WriteString("#EXT-X-ENDLIST\n")
	hlsHeaders(w, "application/vnd.apple.mpegurl")
	io.WriteString(w, b.String())
}

// handleHLSInit serves one file's initialization segment.
func (m *media) handleHLSInit(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f, p, err := m.openPlan(r.Context(), q.Get("dir"), q.Get("file"))
	if err != nil {
		hlsError(w, r, err)
		return
	}
	f.Close()
	hlsHeaders(w, "video/mp4")
	http.ServeContent(w, r, "init.mp4", time.Time{}, bytes.NewReader(p.plan.Init()))
}

// handleHLSSegment cuts one segment out of a file.
func (m *media) handleHLSSegment(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	n, err := strconv.Atoi(q.Get("n"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	f, p, err := m.openPlan(r.Context(), q.Get("dir"), q.Get("file"))
	if err != nil {
		hlsError(w, r, err)
		return
	}
	defer f.Close()
	var buf bytes.Buffer
	if err := p.plan.WriteSegment(&buf, f, n); err != nil {
		m.log.Debug("HLS segment failed", "file", q.Get("file"), "n", n, "error", err)
		http.NotFound(w, r)
		return
	}
	hlsHeaders(w, "video/mp4")
	http.ServeContent(w, r, "seg.m4s", time.Time{}, bytes.NewReader(buf.Bytes()))
}

// handleHLSChapters serves a chapter per file, so the seek bar shows where
// each one starts.
func (m *media) handleHLSChapters(w http.ResponseWriter, r *http.Request) {
	parts, err := m.hlsParts(r.Context(), r.URL.Query().Get("dir"))
	if err != nil {
		hlsError(w, r, err)
		return
	}
	var b strings.Builder
	b.WriteString("WEBVTT\n")
	if len(parts) > 1 {
		for i, p := range parts {
			start := time.Duration(p.offset * float64(time.Second))
			end := time.Duration((p.offset + p.plan.Duration()) * float64(time.Second))
			fmt.Fprintf(&b, "\n%s --> %s\nPart %d of %d\n", vttClock(start), vttClock(end), i+1, len(parts))
		}
	}
	hlsHeaders(w, "text/vtt; charset=utf-8")
	io.WriteString(w, b.String())
}

// handleHLSThumbnails joins the files' timeline-preview tracks onto the
// stream's timeline. Cue images point at the preview route, relative to
// this one.
func (m *media) handleHLSThumbnails(w http.ResponseWriter, r *http.Request) {
	dir := r.URL.Query().Get("dir")
	parts, err := m.hlsParts(r.Context(), dir)
	if err != nil {
		hlsError(w, r, err)
		return
	}
	po, _ := m.files.(PreviewOpener)
	var cues []preview.Cue
	for _, p := range parts {
		if po == nil {
			break
		}
		f, err := po.OpenVODPreview(r.Context(), dir, p.name, "vtt", 0)
		if err != nil {
			continue
		}
		data, err := io.ReadAll(io.LimitReader(f, 8<<20))
		f.Close()
		if err != nil {
			continue
		}
		own, err := preview.ParseVTT(data)
		if err != nil {
			continue
		}
		offset := time.Duration(p.offset * float64(time.Second))
		base := "../preview?dir=" + url.QueryEscape(dir) + "&file=" + url.QueryEscape(p.name) + "&kind=sheet&n="
		for _, c := range own {
			m := sheetName.FindStringSubmatch(c.Image)
			if m == nil {
				continue
			}
			n, _ := strconv.Atoi(m[1])
			c.Start += offset
			c.End += offset
			c.Image = base + strconv.Itoa(n)
			cues = append(cues, c)
		}
	}
	var b bytes.Buffer
	if err := preview.WriteVTT(&b, cues); err != nil {
		http.Error(w, "write failed", http.StatusInternalServerError)
		return
	}
	hlsHeaders(w, "text/vtt; charset=utf-8")
	w.Write(b.Bytes())
}

// sheetName matches a cue's sprite sheet, as previews name them: thumbs.000.jpg,
// or vod1.thumbs.000.jpg from older recorders.
var sheetName = regexp.MustCompile(`(?:^|\.)thumbs\.(\d{3})\.jpg$`)

func vttClock(d time.Duration) string {
	ms := d.Milliseconds()
	return fmt.Sprintf("%02d:%02d:%02d.%03d", ms/3_600_000, ms/60_000%60, ms/1000%60, ms%1000)
}
