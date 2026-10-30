package preview

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"sync"
	"time"

	"golang.org/x/image/draw"
	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/gobold"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
)

// Contact sheet geometry, after vcsi's defaults: 1500 px wide (callers
// building from smaller frames pick narrower), 5 px between frames, a 10 px margin round the header, 16 px header text and
// 12 px timestamps in a translucent box 5 px from each frame's corner.
const (
	ContactWidth     = 1500
	contactSpacing   = 5
	contactMargin    = 10
	headerFontSize   = 16
	stampFontSize    = 12
	stampPadX        = 3
	stampPadY        = 1
	stampMargin      = 5
	contactMaxBytes  = 700 << 10 // fits a NATS reply once base64-encoded
	contactQuality   = 85
	contactMinQualty = 50
)

// ContactGrid is the layout for a video's shape: 4x4 for landscape, 6x3 for
// portrait so the sheet stays wider than it is tall.
func ContactGrid(info Info) (cols, rows int) {
	if info.Portrait() {
		return 6, 3
	}
	return 4, 4
}

// ContactTile is the size each frame of a sheet width px wide is drawn at.
func ContactTile(info Info, width int) Tile {
	cols, _ := ContactGrid(info)
	w := (width - (cols-1)*contactSpacing) / cols
	h := w * 9 / 16
	if info.Width > 0 && info.Height > 0 {
		h = w * info.Height / info.Width
	}
	return Tile{even(w), even(h)}
}

// ContactTargets are the times a contact sheet samples: evenly spread,
// skipping the first and last 7%, where streams sit on placeholders.
func ContactTargets(info Info) []time.Duration {
	cols, rows := ContactGrid(info)
	d := info.Duration
	return Spread(d*7/100, d*93/100, cols*rows)
}

// Shot is one decoded frame and its time.
type Shot struct {
	At  time.Duration
	Img image.Image
}

// ContactSheet renders a vcsi-style sheet width px wide: header lines (the
// first is usually the file name) over a grid of shots, each stamped with
// its time. The JPEG is re-encoded at lower quality until it fits the size
// cap.
func ContactSheet(info Info, width int, header []string, shots []Shot) ([]byte, error) {
	if len(shots) == 0 {
		return nil, errors.New("preview: no shots for a contact sheet")
	}
	cols, rows := ContactGrid(info)
	tile := ContactTile(info, width)
	hf, sf, err := faces()
	if err != nil {
		return nil, err
	}
	lineH := headerFontSize * 12 / 10
	headerH := 2*contactMargin + len(header)*lineH
	width = cols*(tile.W+contactSpacing) - contactSpacing
	usedRows := min(rows, (len(shots)+cols-1)/cols)
	height := headerH + usedRows*(tile.H+contactSpacing)

	img := image.NewRGBA(image.Rect(0, 0, width, height))
	draw.Draw(img, img.Bounds(), image.Black, image.Point{}, draw.Src)
	for i, line := range header {
		y := contactMargin + i*lineH + lineH - (lineH-headerFontSize)/2 - 2
		drawText(img, hf, ellipsize(hf, line, width-2*contactMargin), contactMargin, y, color.White)
	}
	for i, s := range shots {
		if i >= cols*rows {
			break
		}
		x := (i % cols) * (tile.W + contactSpacing)
		y := headerH + (i/cols)*(tile.H+contactSpacing)
		r := image.Rect(x, y, x+tile.W, y+tile.H)
		drawFit(img, r, s.Img)
		stamp(img, sf, r, Clock(s.At))
	}
	q := contactQuality
	for {
		var buf bytes.Buffer
		if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: q}); err != nil {
			return nil, err
		}
		if buf.Len() <= contactMaxBytes || q <= contactMinQualty {
			return buf.Bytes(), nil
		}
		q -= 10
	}
}

// stamp draws a timestamp in the bottom-right corner of r.
func stamp(img *image.RGBA, f font.Face, r image.Rectangle, text string) {
	adv := font.MeasureString(f, text).Ceil()
	m := f.Metrics()
	textH := (m.Ascent + m.Descent).Ceil()
	box := image.Rect(
		r.Max.X-stampMargin-adv-2*stampPadX,
		r.Max.Y-stampMargin-textH-2*stampPadY,
		r.Max.X-stampMargin,
		r.Max.Y-stampMargin,
	)
	draw.Draw(img, box, image.NewUniform(color.NRGBA{0, 0, 0, 0xAA}), image.Point{}, draw.Over)
	drawText(img, f, text, box.Min.X+stampPadX, box.Min.Y+stampPadY+m.Ascent.Ceil(), color.White)
}

func drawText(img *image.RGBA, f font.Face, text string, x, baseline int, c color.Color) {
	d := font.Drawer{Dst: img, Src: image.NewUniform(c), Face: f, Dot: fixed.P(x, baseline)}
	d.DrawString(text)
}

func ellipsize(f font.Face, s string, max int) string {
	if font.MeasureString(f, s).Ceil() <= max {
		return s
	}
	r := []rune(s)
	for len(r) > 0 && font.MeasureString(f, string(r)+"…").Ceil() > max {
		r = r[:len(r)-1]
	}
	return string(r) + "…"
}

var (
	faceOnce           sync.Once
	headerFace, stampF font.Face
	faceErr            error
)

// faces are Go Bold, which ships inside golang.org/x/image, at the header
// and timestamp sizes.
func faces() (font.Face, font.Face, error) {
	faceOnce.Do(func() {
		ft, err := opentype.Parse(gobold.TTF)
		if err != nil {
			faceErr = err
			return
		}
		if headerFace, faceErr = opentype.NewFace(ft, &opentype.FaceOptions{Size: headerFontSize, DPI: 72, Hinting: font.HintingFull}); faceErr != nil {
			return
		}
		stampF, faceErr = opentype.NewFace(ft, &opentype.FaceOptions{Size: stampFontSize, DPI: 72, Hinting: font.HintingFull})
	})
	return headerFace, stampF, faceErr
}

// drawFit scales src to fit r, centred, keeping its aspect ratio.
func drawFit(dst draw.Image, r image.Rectangle, src image.Image) {
	sb := src.Bounds()
	w, h := r.Dx(), r.Dy()
	if sb.Dx()*h > sb.Dy()*w {
		h = max(1, sb.Dy()*w/sb.Dx())
	} else {
		w = max(1, sb.Dx()*h/sb.Dy())
	}
	min := image.Pt(r.Min.X+(r.Dx()-w)/2, r.Min.Y+(r.Dy()-h)/2)
	at := image.Rectangle{Min: min, Max: min.Add(image.Pt(w, h))}
	if sb.Dx() == w && sb.Dy() == h {
		draw.Draw(dst, at, src, sb.Min, draw.Src)
		return
	}
	draw.CatmullRom.Scale(dst, at, src, sb, draw.Src, nil)
}

// Sharpness scores how much detail a frame holds (the variance of its
// luma's Laplacian): black, faded and blurred frames score low.
func Sharpness(img image.Image) float64 {
	y, ok := img.(*image.YCbCr)
	if !ok {
		return 0
	}
	b := y.Bounds()
	var sum, sq float64
	var n int
	for py := b.Min.Y + 1; py < b.Max.Y-1; py += 2 {
		for px := b.Min.X + 1; px < b.Max.X-1; px += 2 {
			c := int(y.Y[y.YOffset(px, py)])
			l := 4*c - int(y.Y[y.YOffset(px-1, py)]) - int(y.Y[y.YOffset(px+1, py)]) -
				int(y.Y[y.YOffset(px, py-1)]) - int(y.Y[y.YOffset(px, py+1)])
			sum += float64(l)
			sq += float64(l * l)
			n++
		}
	}
	if n == 0 {
		return 0
	}
	mean := sum / float64(n)
	return sq/float64(n) - mean*mean
}

// Triptych sizes. A portrait thumbnail shows three frames side by side in
// the same 16:9 box a landscape one fills.
const (
	thumbW       = 480
	thumbH       = 270
	triptychGap  = 3
	thumbQuality = 85
)

// TriptychTargets are the times a portrait thumbnail shows: the middle of
// each third of the video, the stretch an archive card's hover scrubs in
// that panel.
func TriptychTargets(d time.Duration) []time.Duration {
	return []time.Duration{d / 6, d / 2, d * 5 / 6}
}

// TriptychTile is the size each panel's frame is decoded at.
func TriptychTile() Tile {
	return Tile{even((thumbW - 2*triptychGap) / 3), thumbH}
}

// Triptych composes three portrait frames into one landscape thumbnail.
func Triptych(frames []image.Image) ([]byte, error) {
	if len(frames) != 3 {
		return nil, fmt.Errorf("preview: triptych needs 3 frames, got %d", len(frames))
	}
	img := image.NewRGBA(image.Rect(0, 0, thumbW, thumbH))
	draw.Draw(img, img.Bounds(), image.Black, image.Point{}, draw.Src)
	pw := (thumbW - 2*triptychGap) / 3
	for i, f := range frames {
		x := i * (pw + triptychGap)
		drawFit(img, image.Rect(x, 0, x+pw, thumbH), f)
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: thumbQuality}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Clock formats d as M:SS, or H:MM:SS from an hour.
func Clock(d time.Duration) string {
	s := int(d.Seconds())
	if s >= 3600 {
		return fmt.Sprintf("%d:%02d:%02d", s/3600, s/60%60, s%60)
	}
	return fmt.Sprintf("%d:%02d", s/60, s%60)
}
