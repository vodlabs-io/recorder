package processing

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dustin/go-humanize"

	"github.com/vodlabs-io/recorder/internal/ffmpeg"
	"github.com/vodlabs-io/recorder/internal/preview"
	"github.com/vodlabs-io/recorder/internal/provenance"
)

// Previews are sidecars of a VOD file, next to it like its thumbnail so the
// folder sweep carries them along. For vod1.mp4:
//
//	vod1.thumbs.vtt              timeline previews (WebVTT, sprite cues)
//	vod1.thumbs.000.jpg, …       their sprite sheets
//	vod1.scrub.jpg               the archive card's hover strip
//	vod1.sheet.jpg               the contact sheet
//	vod1.previews.json           what the UI needs to lay them out
func stem(vodFile string) string {
	if i := strings.LastIndex(vodFile, "."); i > 0 {
		return vodFile[:i]
	}
	return vodFile
}

// StoryboardName is the WebVTT timeline-preview track of a VOD file.
func StoryboardName(vodFile string) string { return stem(vodFile) + ".thumbs.vtt" }

// StoryboardSheetPrefix starts the names of a VOD file's sprite sheets.
func StoryboardSheetPrefix(vodFile string) string { return stem(vodFile) + ".thumbs." }

// ContactSheetName is the contact sheet of a VOD file.
func ContactSheetName(vodFile string) string { return stem(vodFile) + ".sheet.jpg" }

// ScrubStripName is the hover strip of a VOD file.
func ScrubStripName(vodFile string) string { return stem(vodFile) + ".scrub.jpg" }

// PreviewsManifestName describes a VOD file's previews.
func PreviewsManifestName(vodFile string) string { return stem(vodFile) + ".previews.json" }

// IsPreviewSidecar reports whether name is one of a VOD file's preview
// sidecars rather than a video or its thumbnail.
func IsPreviewSidecar(name string) bool {
	for _, suffix := range []string{".thumbs.vtt", ".scrub.jpg", ".sheet.jpg", ".previews.json"} {
		if strings.HasSuffix(name, suffix) {
			return true
		}
	}
	return strings.Contains(name, ".thumbs.") && strings.HasSuffix(name, ".jpg")
}

// PreviewsVersion is bumped when the layout changes, so older previews are
// regenerated on next view.
const PreviewsVersion = 1

// scrubFrames is how many frames the hover strip holds.
const scrubFrames = 48

// PreviewsManifest is vodN.previews.json.
type PreviewsManifest struct {
	Version    int     `json:"version"`
	Width      int     `json:"width"`
	Height     int     `json:"height"`
	Portrait   bool    `json:"portrait"`
	DurationMs int64   `json:"duration_ms"`
	IntervalMs int64   `json:"interval_ms"`
	Frames     int     `json:"frames"`
	Sheets     int     `json:"sheets"`
	Tile       [2]int  `json:"tile"`
	Scrub      Scrub   `json:"scrub"`
	Codec      string  `json:"codec,omitempty"`
	FrameRate  float64 `json:"frame_rate,omitempty"`
}

// Scrub lays out the hover strip: Count tiles of Tile size, Cols per row,
// tile i showing the video at TimesMs[i].
type Scrub struct {
	Cols    int     `json:"cols"`
	Count   int     `json:"count"`
	TimesMs []int64 `json:"times_ms"`
}

// PreviewHeader is what a contact sheet's header says beyond the file
// itself.
type PreviewHeader struct {
	Platform, Username string
	Start              time.Time
}

// previewDecoder is the daemon's one decoder for preview work: one ffmpeg at
// a time, at low priority.
var previewDecoder = preview.NewDecoder(ffmpeg.Path, 1)

// PreviewDecoder is the shared decoder, for on-demand generation outside
// processing.
func PreviewDecoder() *preview.Decoder { return previewDecoder }

// GeneratePreviews builds every preview of one VOD file — timeline previews,
// hover strip, contact sheet and, for a portrait video, a three-up thumbnail
// over thumbPath — and writes them through write. Keyframes are read straight
// from an MP4's index; other containers, and short videos whose keyframes are
// too sparse for a preview a second, are decoded front to back instead.
func GeneratePreviews(ctx context.Context, logger *slog.Logger, videoPath, thumbName string, hdr PreviewHeader, write func(name string, data []byte) error) (PreviewsManifest, error) {
	start := time.Now()
	dec := previewDecoder
	file := filepath.Base(videoPath)
	src, info, err := openSource(ctx, logger, videoPath)
	if err != nil {
		return PreviewsManifest{}, err
	}
	if src != nil {
		defer src.Close()
	}
	if info.Duration <= 0 {
		return PreviewsManifest{}, errors.New("previews: unknown duration")
	}

	interval := preview.Interval(info.Duration)
	tile := preview.TileFor(info)
	box := preview.Box{W: tile.W, H: tile.H}

	var times []time.Duration
	var frames [][]byte
	sparse := src == nil || preview.MeanKeySpacing(src.KeyTimes(), info.Duration) > interval*3/2
	switch {
	case src != nil && !(sparse && info.Duration <= preview.FullDecodeLimit):
		times, frames, err = decodePicked(ctx, dec, src, preview.Ticks(info.Duration, interval), box, 5)
	default:
		frames, err = dec.DecodeFile(ctx, videoPath, preview.FileOptions{
			Interval:      interval,
			KeyframesOnly: info.Duration > preview.FullDecodeLimit,
			Box:           box,
			Quality:       5,
			Duration:      info.Duration,
		})
		times = preview.Ticks(info.Duration, interval)[:len(frames)]
	}
	if err != nil {
		return PreviewsManifest{}, fmt.Errorf("previews: decode timeline: %w", err)
	}
	if len(frames) == 0 {
		return PreviewsManifest{}, errors.New("previews: no frames decoded")
	}

	sw := preview.NewSheetWriter(tile, StoryboardSheetPrefix(file), write)
	for i, f := range frames {
		if err := sw.Add(times[i], f); err != nil {
			return PreviewsManifest{}, err
		}
	}
	cues, err := sw.Close(info.Duration)
	if err != nil {
		return PreviewsManifest{}, err
	}
	var vtt bytes.Buffer
	if err := preview.WriteVTT(&vtt, cues); err != nil {
		return PreviewsManifest{}, err
	}
	if err := write(StoryboardName(file), vtt.Bytes()); err != nil {
		return PreviewsManifest{}, err
	}

	strip, stripTimes, err := preview.Strip(tile, times, frames, info.Duration, scrubFrames, 70)
	if err != nil {
		return PreviewsManifest{}, err
	}
	if err := write(ScrubStripName(file), stamped(logger, strip, "scrub-strip", file)); err != nil {
		return PreviewsManifest{}, err
	}

	shots, err := contactShots(ctx, dec, src, videoPath, info)
	if err != nil {
		return PreviewsManifest{}, fmt.Errorf("previews: contact sheet: %w", err)
	}
	stat, _ := os.Stat(videoPath)
	sheet, err := preview.ContactSheet(info, contactHeader(file, stat, info, hdr), shots)
	if err != nil {
		return PreviewsManifest{}, err
	}
	if err := write(ContactSheetName(file), stamped(logger, sheet, "contact-sheet", file)); err != nil {
		return PreviewsManifest{}, err
	}

	if info.Portrait() && thumbName != "" {
		var picks []image.Image
		for _, t := range preview.TriptychTargets(info.Duration) {
			picks = append(picks, nearestShot(shots, t).Img)
		}
		thumb, err := preview.Triptych(picks)
		if err != nil {
			return PreviewsManifest{}, err
		}
		if err := write(thumbName, stamped(logger, thumb, "thumbnail", file)); err != nil {
			return PreviewsManifest{}, err
		}
	}

	m := PreviewsManifest{
		Version:    PreviewsVersion,
		Width:      info.Width,
		Height:     info.Height,
		Portrait:   info.Portrait(),
		DurationMs: info.Duration.Milliseconds(),
		IntervalMs: interval.Milliseconds(),
		Frames:     len(cues),
		Sheets:     (len(cues) + preview.SheetCols*preview.SheetRows - 1) / (preview.SheetCols * preview.SheetRows),
		Tile:       [2]int{tile.W, tile.H},
		Scrub:      Scrub{Cols: min(scrubFrames, preview.SheetCols), Count: len(stripTimes)},
		Codec:      info.Codec,
		FrameRate:  info.FrameRate,
	}
	for _, t := range stripTimes {
		m.Scrub.TimesMs = append(m.Scrub.TimesMs, t.Milliseconds())
	}
	js, err := json.Marshal(m)
	if err != nil {
		return PreviewsManifest{}, err
	}
	// The manifest goes last: its presence says the set is complete.
	if err := write(PreviewsManifestName(file), js); err != nil {
		return PreviewsManifest{}, err
	}
	logger.Info("Generated previews", "file", file, "frames", m.Frames, "sheets", m.Sheets,
		"interval", interval, "took", time.Since(start).Round(time.Millisecond))
	return m, nil
}

// openSource indexes an MP4, or probes anything else. src is nil when the
// file has to be decoded front to back.
func openSource(ctx context.Context, logger *slog.Logger, path string) (*preview.MP4, preview.Info, error) {
	if src, err := preview.OpenMP4(path); err == nil {
		return src, src.Info(), nil
	} else if !errors.Is(err, preview.ErrUnsupported) {
		return nil, preview.Info{}, err
	}
	p, err := ffmpeg.Probe(ctx, logger, path)
	if err != nil {
		return nil, preview.Info{}, err
	}
	if p.VideoCodec == "" {
		return nil, preview.Info{}, errors.New("previews: no video stream")
	}
	return nil, preview.Info{
		Width:    p.Width,
		Height:   p.Height,
		Duration: time.Duration(p.Duration * float64(time.Second)),
		Codec:    codecName(p.VideoCodec),
	}, nil
}

func codecName(c string) string {
	switch c {
	case "h264":
		return "H.264"
	case "hevc":
		return "HEVC"
	case "vp8":
		return "VP8"
	}
	return strings.ToUpper(c)
}

// decodeBatch bounds how many keyframes one ffmpeg run holds in memory.
const decodeBatch = 64

// decodePicked decodes the keyframe shown at each target time, reading only
// those keyframes from the file.
func decodePicked(ctx context.Context, dec *preview.Decoder, src *preview.MP4, targets []time.Duration, box preview.Box, quality int) ([]time.Duration, [][]byte, error) {
	keys := src.KeyTimes()
	picks := preview.PickKeys(keys, targets)
	var times []time.Duration
	var out [][]byte
	for len(picks) > 0 {
		n := min(len(picks), decodeBatch)
		batch := make([]preview.Keyframe, 0, n)
		for _, i := range picks[:n] {
			kf, err := src.Keyframe(i)
			if err != nil {
				return nil, nil, err
			}
			batch = append(batch, kf)
		}
		jpegs, err := dec.DecodeKeyframes(ctx, src.Config(), batch, box, quality)
		if err != nil {
			return nil, nil, err
		}
		for i, kf := range batch {
			times = append(times, kf.PTS)
			out = append(out, jpegs[i])
		}
		picks = picks[n:]
	}
	return times, out, nil
}

// contactShots decodes two candidates per contact sheet slot and keeps the
// sharper, so a fade or placeholder doesn't take a slot.
func contactShots(ctx context.Context, dec *preview.Decoder, src *preview.MP4, path string, info preview.Info) ([]preview.Shot, error) {
	targets := preview.ContactTargets(info)
	tile := preview.ContactTile(info)
	box := preview.Box{W: tile.W, H: tile.H}
	var cand []preview.Shot
	if src != nil {
		keys := src.KeyTimes()
		var batch []preview.Keyframe
		seen := map[int]bool{}
		for _, t := range targets {
			i := preview.NearestKey(keys, t)
			for _, k := range []int{i, i + 1} {
				if k < len(keys) && !seen[k] {
					seen[k] = true
					kf, err := src.Keyframe(k)
					if err != nil {
						return nil, err
					}
					batch = append(batch, kf)
				}
			}
		}
		jpegs, err := dec.DecodeKeyframes(ctx, src.Config(), batch, box, 3)
		if err != nil {
			return nil, err
		}
		for i, j := range jpegs {
			img, err := jpeg.Decode(bytes.NewReader(j))
			if err != nil {
				return nil, err
			}
			cand = append(cand, preview.Shot{At: batch[i].PTS, Img: img})
		}
	} else {
		step := info.Duration / time.Duration(2*len(targets)+2)
		jpegs, err := dec.DecodeFile(ctx, path, preview.FileOptions{Interval: step, KeyframesOnly: true, Box: box, Quality: 3, Duration: info.Duration})
		if err != nil {
			return nil, err
		}
		for i, j := range jpegs {
			img, err := jpeg.Decode(bytes.NewReader(j))
			if err != nil {
				return nil, err
			}
			cand = append(cand, preview.Shot{At: time.Duration(i) * step, Img: img})
		}
	}
	if len(cand) == 0 {
		return nil, errors.New("no frames")
	}
	shots := make([]preview.Shot, 0, len(targets))
	for _, t := range targets {
		a, b := nearestTwo(cand, t)
		best := a
		if b != nil && preview.Sharpness(b.Img) > preview.Sharpness(a.Img)*1.15 {
			best = b
		}
		shots = append(shots, preview.Shot{At: best.At, Img: best.Img})
	}
	return shots, nil
}

// nearestTwo returns the candidate closest to t and the one after it.
func nearestTwo(cand []preview.Shot, t time.Duration) (*preview.Shot, *preview.Shot) {
	best := 0
	for i := range cand {
		if absDur(cand[i].At-t) < absDur(cand[best].At-t) {
			best = i
		}
	}
	if best+1 < len(cand) {
		return &cand[best], &cand[best+1]
	}
	return &cand[best], nil
}

func nearestShot(shots []preview.Shot, t time.Duration) preview.Shot {
	s, _ := nearestTwo(shots, t)
	return *s
}

func absDur(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

// contactHeader is the sheet's header, vcsi's lines first.
func contactHeader(file string, stat os.FileInfo, info preview.Info, hdr PreviewHeader) []string {
	lines := []string{file}
	size := "File size: unknown"
	if stat != nil {
		size = "File size: " + humanize.IBytes(uint64(stat.Size()))
	}
	lines = append(lines, size, "Duration: "+preview.Clock(info.Duration))
	dims := fmt.Sprintf("Dimensions: %dx%d", info.Width, info.Height)
	if info.Codec != "" {
		dims += "  ·  " + info.Codec
	}
	if info.FrameRate > 0 {
		dims += fmt.Sprintf("  ·  %.4g fps", info.FrameRate)
	}
	lines = append(lines, dims)
	var who []string
	if hdr.Username != "" {
		who = append(who, hdr.Username)
	}
	if hdr.Platform != "" {
		who = append(who, hdr.Platform)
	}
	if !hdr.Start.IsZero() {
		who = append(who, hdr.Start.Format("2006-01-02 15:04 MST"))
	}
	if len(who) > 0 {
		lines = append(lines, strings.Join(who, "  ·  "))
	}
	return lines
}

// stamped records what produced an image, as thumbnails do. A failure keeps
// the image unstamped.
func stamped(logger *slog.Logger, data []byte, kind, file string) []byte {
	out, err := provenance.StampJPEG(data, provenance.Record(kind, "", file))
	if err != nil {
		logger.Debug("Could not stamp preview", "file", file, "error", err)
		return data
	}
	return out
}

// HasPreviews reports whether dir holds a complete, current set of previews
// for vodFile.
func HasPreviews(dir, vodFile string) bool {
	m, err := ReadPreviewsManifest(filepath.Join(dir, PreviewsManifestName(vodFile)))
	return err == nil && m.Version == PreviewsVersion
}

// ReadPreviewsManifest reads a vodN.previews.json.
func ReadPreviewsManifest(path string) (PreviewsManifest, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return PreviewsManifest{}, err
	}
	var m PreviewsManifest
	return m, json.Unmarshal(b, &m)
}

// WriteFileAtomic writes a sidecar via a temp file and rename, so a reader
// never sees half of one.
func WriteFileAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// WritePreviews regenerates every preview sidecar of videoDir/vodFile into
// outDir (usually the same folder), replacing any older set. The thumbnail is
// rewritten only for a portrait video.
func WritePreviews(ctx context.Context, logger *slog.Logger, videoDir, outDir, vodFile string, hdr PreviewHeader) (PreviewsManifest, error) {
	RemovePreviews(outDir, vodFile)
	return GeneratePreviews(ctx, logger, filepath.Join(videoDir, vodFile), ThumbnailName(vodFile), hdr,
		func(name string, data []byte) error { return WriteFileAtomic(filepath.Join(outDir, name), data) })
}

// RemovePreviews deletes vodFile's preview sidecars from dir (not its
// thumbnail).
func RemovePreviews(dir, vodFile string) {
	names := []string{StoryboardName(vodFile), ContactSheetName(vodFile), ScrubStripName(vodFile), PreviewsManifestName(vodFile)}
	sheets, _ := filepath.Glob(filepath.Join(dir, globEscape(StoryboardSheetPrefix(vodFile))+"[0-9][0-9][0-9].jpg"))
	for _, n := range names {
		os.Remove(filepath.Join(dir, n))
	}
	for _, s := range sheets {
		os.Remove(s)
	}
}

func globEscape(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `*`, `\*`, `?`, `\?`, `[`, `\[`)
	return r.Replace(s)
}
