package main

import (
	"bytes"
	"fmt"
	"image"
	"image/jpeg"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"time"

	"golang.org/x/image/draw"

	"github.com/vodlabs-io/recorder/internal/preview"
)

// media serves what the app's player plays, for a Scene that opens it: each
// VOD file as a still video of its thumbnail, as long as the file, and a
// timeline-preview track whose tiles are the Streamer's thumbnails. Both are
// made on first request, the video with ffmpeg, and kept in dir.
type media struct {
	a   *archive
	dir string
	mu  sync.Mutex
}

// Timeline-preview tiles, the size the real daemon cuts for 16:9 video.
const (
	tileW    = 256
	tileH    = 144
	maxTiles = 12
)

func (m *media) routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /media", m.serveVideo)
	mux.HandleFunc("GET /thumbs.vtt", m.serveTrack)
	mux.HandleFunc("GET /sheet.jpg", m.serveSheet)
}

// lookup finds the VOD at ?dir= and the index of its ?file=.
func (m *media) lookup(r *http.Request) (VOD, int, bool) {
	v, ok := m.a.find(r.URL.Query().Get("dir"))
	if !ok {
		return v, 0, false
	}
	file := r.URL.Query().Get("file")
	for i := range v.Files {
		if fileName(v, i) == file {
			return v, i, true
		}
	}
	return v, 0, false
}

func (m *media) serveVideo(w http.ResponseWriter, r *http.Request) {
	v, i, ok := m.lookup(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	path := filepath.Join(m.dir, fmt.Sprintf("%s-%d.mp4", v.ID, i))
	m.mu.Lock()
	err := m.stillVideo(m.a.thumbPath(v, i), path, v.Duration/time.Duration(v.Files))
	m.mu.Unlock()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.ServeFile(w, r, path)
}

// stillVideo writes img as an H.264 video lasting dur, one frame a minute.
func (m *media) stillVideo(img, out string, dur time.Duration) error {
	if _, err := os.Stat(out); err == nil {
		return nil
	}
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		return fmt.Errorf("playing a VOD needs ffmpeg on PATH")
	}
	tmp := out + ".tmp.mp4"
	cmd := exec.Command(ffmpeg, "-v", "error", "-y",
		"-loop", "1", "-framerate", "1/60", "-i", img,
		"-t", strconv.Itoa(int(dur.Seconds())),
		"-vf", "scale=1280:-2,format=yuv420p",
		"-c:v", "libx264", "-tune", "stillimage", "-movflags", "+faststart", tmp)
	if b, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("ffmpeg: %v: %s", err, b)
	}
	return os.Rename(tmp, out)
}

// tiles are the thumbnails of every VOD of v's Streamer, v's own first.
func (m *media) tiles(v VOD) []string {
	var own, rest []string
	for _, o := range m.a.ds.VODs {
		if o.Key != v.Key {
			continue
		}
		for i := range o.Files {
			p := m.a.thumbPath(o, i)
			if o.ID == v.ID {
				own = append(own, p)
			} else {
				rest = append(rest, p)
			}
		}
	}
	var out []string
	for _, p := range append(own, rest...) {
		if _, err := os.Stat(p); err == nil && !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	return out[:min(len(out), maxTiles)]
}

// serveTrack is the file's timeline-preview track: the file's span divided
// evenly among the tiles, in order.
func (m *media) serveTrack(w http.ResponseWriter, r *http.Request) {
	v, _, ok := m.lookup(r)
	tiles := m.tiles(v)
	if !ok || len(tiles) == 0 {
		http.NotFound(w, r)
		return
	}
	span := v.Duration / time.Duration(v.Files) / time.Duration(len(tiles))
	sheet := "sheet.jpg?dir=" + r.URL.Query().Get("dir")
	cues := make([]preview.Cue, len(tiles))
	for j := range tiles {
		cues[j] = preview.Cue{
			Start: span * time.Duration(j), End: span * time.Duration(j+1),
			Image: sheet, X: j * tileW, W: tileW, H: tileH,
		}
	}
	w.Header().Set("Content-Type", "text/vtt")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	if err := preview.WriteVTT(w, cues); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// serveSheet lays the tiles of ?dir='s VOD out in one row.
func (m *media) serveSheet(w http.ResponseWriter, r *http.Request) {
	v, ok := m.a.find(r.URL.Query().Get("dir"))
	tiles := m.tiles(v)
	if !ok || len(tiles) == 0 {
		http.NotFound(w, r)
		return
	}
	sheet := image.NewRGBA(image.Rect(0, 0, tileW*len(tiles), tileH))
	for j, p := range tiles {
		f, err := os.Open(p)
		if err != nil {
			continue
		}
		src, err := jpeg.Decode(f)
		f.Close()
		if err != nil {
			continue
		}
		draw.CatmullRom.Scale(sheet, image.Rect(j*tileW, 0, (j+1)*tileW, tileH), src, src.Bounds(), draw.Src, nil)
	}
	var b bytes.Buffer
	if err := jpeg.Encode(&b, sheet, &jpeg.Options{Quality: 85}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Write(b.Bytes())
}
