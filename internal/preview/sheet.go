package preview

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"strings"
	"time"

	"golang.org/x/image/draw"
)

// Tile is the size of one timeline preview.
type Tile struct{ W, H int }

// tileEdge is a tile's long side: big enough for a seek-bar preview and an
// archive card's hover, small enough that a sheet of them stays a quick
// download.
const tileEdge = 256

// TileFor sizes tiles to the video's shape: 256x144 for 16:9, 144x256 for
// 9:16. Both sides are even, as 4:2:0 chroma needs.
func TileFor(info Info) Tile {
	w, h := info.Width, info.Height
	if w <= 0 || h <= 0 {
		return Tile{tileEdge, 144}
	}
	if w >= h {
		return Tile{tileEdge, even(tileEdge * h / w)}
	}
	return Tile{even(tileEdge * w / h), tileEdge}
}

func even(n int) int {
	n &^= 1
	if n < 2 {
		return 2
	}
	return n
}

// SheetCols and SheetRows lay out a finished file's sprite sheets: 64
// previews, about 150-250 KB as a JPEG. A live recording publishes smaller
// sheets, since only full ones are published.
const (
	SheetCols = 8
	SheetRows = 8
)

// Cue is one timeline preview: the picture shown from Start until End,
// found at (X, Y) in Image.
type Cue struct {
	Start, End time.Duration
	Image      string
	X, Y, W, H int
}

// SheetWriter packs timeline previews into sprite sheets as they arrive.
// It holds only the open sheet's tiles, as the small JPEGs they arrived in,
// and composes a sheet when it fills. Sheets are named prefix000.jpg,
// prefix001.jpg, … and written through write.
type SheetWriter struct {
	tile       Tile
	cols, rows int
	prefix     string
	quality    int
	write      func(name string, data []byte) error

	sheet int
	done  []Cue // cues of written sheets
	open  []Cue // cues of the sheet being filled
	tiles [][]byte
}

// NewSheetWriter starts a storyboard of tile-sized previews, cols x rows to
// a sheet.
func NewSheetWriter(tile Tile, cols, rows int, prefix string, write func(name string, data []byte) error) *SheetWriter {
	return &SheetWriter{tile: tile, cols: max(1, cols), rows: max(1, rows), prefix: prefix, quality: 70, write: write}
}

// Resume continues a storyboard whose sheets up to the one cues end in were
// already written.
func (s *SheetWriter) Resume(cues []Cue) {
	s.done = append([]Cue(nil), cues...)
	for _, c := range cues {
		var n int
		if _, err := fmt.Sscanf(strings.TrimPrefix(c.Image, s.prefix), "%03d.jpg", &n); err == nil && n >= s.sheet {
			s.sheet = n + 1
		}
	}
}

// SheetName is the file name of sheet n.
func (s *SheetWriter) SheetName(n int) string { return fmt.Sprintf("%s%03d.jpg", s.prefix, n) }

// Add places the preview shown from at onward. Previews must arrive in time
// order. A full sheet is encoded and written before Add returns.
func (s *SheetWriter) Add(at time.Duration, frame []byte) error {
	if _, err := jpeg.DecodeConfig(bytes.NewReader(frame)); err != nil {
		return fmt.Errorf("preview: decode frame: %w", err)
	}
	i := len(s.open)
	x := (i % s.cols) * s.tile.W
	y := (i / s.cols) * s.tile.H
	s.closeLast(at)
	s.open = append(s.open, Cue{Start: at, End: at, Image: s.SheetName(s.sheet), X: x, Y: y, W: s.tile.W, H: s.tile.H})
	s.tiles = append(s.tiles, frame)
	if len(s.open) == s.cols*s.rows {
		return s.Flush()
	}
	return nil
}

func (s *SheetWriter) closeLast(at time.Duration) {
	if n := len(s.open); n > 0 {
		s.open[n-1].End = at
	} else if n := len(s.done); n > 0 && s.done[n-1].End < at {
		s.done[n-1].End = at
	}
}

// Flush writes the sheet being filled, keeping only the rows it uses, and
// starts the next one.
func (s *SheetWriter) Flush() error {
	n := len(s.open)
	if n == 0 {
		return nil
	}
	rows := (n + s.cols - 1) / s.cols
	cols := min(n, s.cols)
	canvas := blackYCbCr(cols*s.tile.W, rows*s.tile.H)
	for i, t := range s.tiles {
		img, err := jpeg.Decode(bytes.NewReader(t))
		if err != nil {
			return fmt.Errorf("preview: decode frame: %w", err)
		}
		c := s.open[i]
		placeFit(canvas, image.Rect(c.X, c.Y, c.X+c.W, c.Y+c.H), img)
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, canvas, &jpeg.Options{Quality: s.quality}); err != nil {
		return err
	}
	if err := s.write(s.SheetName(s.sheet), buf.Bytes()); err != nil {
		return err
	}
	s.done = append(s.done, s.open...)
	s.open, s.tiles = nil, nil
	s.sheet++
	return nil
}

// Pending lists the previews not yet in a written sheet.
func (s *SheetWriter) Pending() int { return len(s.open) }

// Cues lists the previews in written sheets.
func (s *SheetWriter) Cues() []Cue { return s.done }

// Close writes the last, partial sheet and ends the last preview at end.
func (s *SheetWriter) Close(end time.Duration) ([]Cue, error) {
	s.closeLast(end)
	if err := s.Flush(); err != nil {
		return nil, err
	}
	return s.done, nil
}

// WriteVTT writes cues as a WebVTT thumbnails track: each cue's text is its
// image with a #xywh media fragment. Cues without a crop (W == 0) name a
// whole image.
func WriteVTT(w io.Writer, cues []Cue) error {
	var b strings.Builder
	b.WriteString("WEBVTT\n")
	for _, c := range cues {
		end := c.End
		if end <= c.Start {
			end = c.Start + time.Millisecond
		}
		fmt.Fprintf(&b, "\n%s --> %s\n%s", vttTime(c.Start), vttTime(end), c.Image)
		if c.W > 0 {
			fmt.Fprintf(&b, "#xywh=%d,%d,%d,%d", c.X, c.Y, c.W, c.H)
		}
		b.WriteByte('\n')
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// ParseVTT reads a thumbnails track back, as WriteVTT writes it.
func ParseVTT(data []byte) ([]Cue, error) {
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	if !strings.HasPrefix(text, "WEBVTT") {
		return nil, fmt.Errorf("preview: not a WebVTT track")
	}
	var cues []Cue
	for _, block := range strings.Split(text, "\n\n")[1:] {
		lines := strings.Split(strings.TrimSpace(block), "\n")
		if len(lines) < 2 {
			continue
		}
		from, to, ok := strings.Cut(lines[0], " --> ")
		if !ok {
			continue
		}
		start, err1 := parseVTTTime(from)
		end, err2 := parseVTTTime(to)
		if err1 != nil || err2 != nil {
			return nil, fmt.Errorf("preview: bad cue timing %q", lines[0])
		}
		c := Cue{Start: start, End: end, Image: lines[1]}
		if img, frag, ok := strings.Cut(lines[1], "#xywh="); ok {
			c.Image = img
			fmt.Sscanf(frag, "%d,%d,%d,%d", &c.X, &c.Y, &c.W, &c.H)
		}
		cues = append(cues, c)
	}
	return cues, nil
}

func parseVTTTime(s string) (time.Duration, error) {
	var h, m, sec, ms int
	if _, err := fmt.Sscanf(strings.TrimSpace(s), "%d:%d:%d.%d", &h, &m, &sec, &ms); err != nil {
		return 0, err
	}
	return time.Duration(h)*time.Hour + time.Duration(m)*time.Minute + time.Duration(sec)*time.Second + time.Duration(ms)*time.Millisecond, nil
}

func vttTime(d time.Duration) string {
	ms := d.Milliseconds()
	return fmt.Sprintf("%02d:%02d:%02d.%03d", ms/3600000, ms/60000%60, ms/1000%60, ms%1000)
}

// blackYCbCr is a 4:2:0 canvas in full-range black, the colour space
// image/jpeg encodes without converting.
func blackYCbCr(w, h int) *image.YCbCr {
	img := image.NewYCbCr(image.Rect(0, 0, w, h), image.YCbCrSubsampleRatio420)
	for i := range img.Cb {
		img.Cb[i] = 128
		img.Cr[i] = 128
	}
	return img
}

// placeFit draws src centred in r, scaled down to fit if it is larger, on
// whatever r already holds (black). r's corner and size must be even.
func placeFit(dst *image.YCbCr, r image.Rectangle, src image.Image) {
	sb := src.Bounds()
	w, h := sb.Dx(), sb.Dy()
	if w > r.Dx() || h > r.Dy() {
		if w*r.Dy() > h*r.Dx() {
			w, h = r.Dx(), max(2, h*r.Dx()/w)
		} else {
			w, h = max(2, w*r.Dy()/h), r.Dy()
		}
		w, h = even(w), even(h)
		scaled := image.NewRGBA(image.Rect(0, 0, w, h))
		draw.ApproxBiLinear.Scale(scaled, scaled.Bounds(), src, sb, draw.Src, nil)
		src, sb = scaled, scaled.Bounds()
	}
	ox := r.Min.X + (r.Dx()-w)/2&^1
	oy := r.Min.Y + (r.Dy()-h)/2&^1
	if y, ok := src.(*image.YCbCr); ok && y.SubsampleRatio == image.YCbCrSubsampleRatio420 && sb.Min.X%2 == 0 && sb.Min.Y%2 == 0 {
		copy420(dst, ox, oy, y, w&^1, h&^1)
		return
	}
	for py := range h {
		for px := range w {
			cr, cg, cb, _ := src.At(sb.Min.X+px, sb.Min.Y+py).RGBA()
			yy, u, v := color.RGBToYCbCr(uint8(cr>>8), uint8(cg>>8), uint8(cb>>8))
			dst.Y[dst.YOffset(ox+px, oy+py)] = yy
			if px%2 == 0 && py%2 == 0 {
				ci := dst.COffset(ox+px, oy+py)
				dst.Cb[ci], dst.Cr[ci] = u, v
			}
		}
	}
}

// copy420 copies a w x h block of 4:2:0 planes from src's origin to (x, y).
func copy420(dst *image.YCbCr, x, y int, src *image.YCbCr, w, h int) {
	sb := src.Bounds()
	for row := range h {
		so := src.YOffset(sb.Min.X, sb.Min.Y+row)
		do := dst.YOffset(x, y+row)
		copy(dst.Y[do:do+w], src.Y[so:so+w])
	}
	for row := 0; row < h/2; row++ {
		so := src.COffset(sb.Min.X, sb.Min.Y+2*row)
		do := dst.COffset(x, y+2*row)
		copy(dst.Cb[do:do+w/2], src.Cb[so:so+w/2])
		copy(dst.Cr[do:do+w/2], src.Cr[so:so+w/2])
	}
}

// Strip packs n previews spread evenly across a video into one small sprite
// for hovering an archive card. frames must be sorted by time; tiles are
// reused, never decoded again.
func Strip(tile Tile, times []time.Duration, frames [][]byte, dur time.Duration, n, quality int) ([]byte, []time.Duration, error) {
	if len(frames) == 0 || n <= 0 {
		return nil, nil, fmt.Errorf("preview: nothing to strip")
	}
	cols := min(n, SheetCols)
	rows := (n + cols - 1) / cols
	canvas := blackYCbCr(cols*tile.W, rows*tile.H)
	var at []time.Duration
	// Each slot shows the frame at the middle of its share of the video.
	for i := range n {
		t := dur * time.Duration(2*i+1) / time.Duration(2*n)
		k := PickKeys(times, []time.Duration{t})
		if len(k) == 0 {
			continue
		}
		img, err := jpeg.Decode(bytes.NewReader(frames[k[0]]))
		if err != nil {
			return nil, nil, err
		}
		x, y := (i%cols)*tile.W, (i/cols)*tile.H
		placeFit(canvas, image.Rect(x, y, x+tile.W, y+tile.H), img)
		at = append(at, t)
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, canvas, &jpeg.Options{Quality: quality}); err != nil {
		return nil, nil, err
	}
	return buf.Bytes(), at, nil
}
