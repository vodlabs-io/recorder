package processing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/dustin/go-humanize"

	"github.com/vodlabs-io/recorder/internal/preview"
)

// A session's contact sheet covers all its files laid end to end, sampled
// at the same 16 or 18 slots however many files there are; only the files
// a slot lands in are opened. Its key lists the files it was built from, so
// a merge or a deleted file makes it stale. A portrait session of several
// files has a three-up thumbnail across them too, keyed the same way.
const (
	SessionSheetName        = "contact-sheet.jpg"
	sessionSheetKeyBase     = "contact-sheet.json"
	sessionSheetKeyName     = PreviewsDir + "/" + sessionSheetKeyBase
	SessionThumbnailName    = PreviewsDir + "/thumbnail.jpg"
	sessionThumbnailKeyName = PreviewsDir + "/thumbnail.json"
)

// SheetFile is one file a session sheet is built from.
type SheetFile struct {
	Name  string `json:"name"`
	Bytes int64  `json:"bytes"`
}

// SessionSheetCurrent reports whether dir holds a session sheet built from
// exactly files.
func SessionSheetCurrent(dir string, files []SheetFile) bool {
	return keyCurrent(dir, sessionSheetKeyName, SessionSheetName, files)
}

// SessionThumbnailCurrent reports whether dir holds a session thumbnail
// built from exactly files.
func SessionThumbnailCurrent(dir string, files []SheetFile) bool {
	return keyCurrent(dir, sessionThumbnailKeyName, SessionThumbnailName, files)
}

func keyCurrent(dir, keyName, imageName string, files []SheetFile) bool {
	b, err := ReadSidecar(dir, keyName)
	if err != nil {
		return false
	}
	var key []SheetFile
	if json.Unmarshal(b, &key) != nil || !slices.Equal(key, files) {
		return false
	}
	_, err = os.Stat(filepath.Join(dir, imageName))
	return err == nil
}

// sheetPart is one file placed on the session's timeline.
type sheetPart struct {
	SheetFile
	path   string
	offset time.Duration
	info   preview.Info
	src    *preview.MP4 // nil until a slot needs it, or for a non-MP4
}

// sessionParts places files of the session in videoDir end to end, skipping
// any it can't read. The caller closes the parts' sources.
func sessionParts(ctx context.Context, logger *slog.Logger, videoDir string, files []SheetFile) ([]*sheetPart, time.Duration) {
	var parts []*sheetPart
	var total time.Duration
	for _, f := range files {
		p := &sheetPart{SheetFile: f, path: filepath.Join(videoDir, f.Name), offset: total}
		if m, err := ReadPreviewsManifest(videoDir, f.Name); err == nil && m.DurationMs > 0 {
			p.info = preview.Info{Width: m.Width, Height: m.Height, Duration: time.Duration(m.DurationMs) * time.Millisecond, Codec: m.Codec, FrameRate: m.FrameRate}
		} else {
			src, info, err := openSource(ctx, logger, p.path)
			if err != nil {
				logger.Debug("Skipping file for the session previews", "file", f.Name, "error", err)
				continue
			}
			p.src, p.info = src, info
		}
		if p.info.Duration <= 0 {
			continue
		}
		parts = append(parts, p)
		total += p.info.Duration
	}
	return parts, total
}

func closeParts(parts []*sheetPart) {
	for _, p := range parts {
		if p.src != nil {
			p.src.Close()
		}
	}
}

// sessionShots decodes candidate frames near targets, times on the session's
// timeline, from whichever parts they land in.
func sessionShots(ctx context.Context, logger *slog.Logger, parts []*sheetPart, targets []time.Duration, tile preview.Tile) ([]preview.Shot, error) {
	var cand []preview.Shot
	for i, p := range parts {
		var local []time.Duration
		for _, t := range targets {
			if t >= p.offset && (t < p.offset+p.info.Duration || i == len(parts)-1) {
				local = append(local, t-p.offset)
			}
		}
		if len(local) == 0 {
			continue
		}
		if p.src == nil {
			if src, err := preview.OpenMP4(p.path); err == nil {
				p.src = src
			} else if !errors.Is(err, preview.ErrUnsupported) {
				return nil, err
			}
		}
		shots, err := contactCandidates(ctx, previewDecoder, p.src, p.path, p.info, local, tile)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			logger.Debug("Skipping file for the session previews", "file", p.Name, "error", err)
			continue
		}
		for _, s := range shots {
			cand = append(cand, preview.Shot{At: s.At + p.offset, Img: s.Img})
		}
	}
	if len(cand) == 0 {
		return nil, errors.New("no frames")
	}
	return cand, nil
}

// WriteSessionSheet builds the contact sheet of the session in videoDir from
// files, in order, and writes it and its key into outDir.
func WriteSessionSheet(ctx context.Context, logger *slog.Logger, videoDir, outDir string, files []SheetFile, hdr PreviewHeader) ([]byte, error) {
	start := time.Now()
	parts, total := sessionParts(ctx, logger, videoDir, files)
	defer closeParts(parts)
	if len(parts) == 0 {
		return nil, errors.New("session sheet: no readable files")
	}

	info := parts[0].info
	info.Duration = total
	targets := preview.ContactTargets(info)
	cand, err := sessionShots(ctx, logger, parts, targets, preview.ContactTile(info, preview.ContactWidth))
	if err != nil {
		return nil, fmt.Errorf("session sheet: %w", err)
	}
	sheet, err := preview.ContactSheet(info, preview.ContactWidth, sessionHeader(parts, info, hdr), pickSharper(cand, targets))
	if err != nil {
		return nil, err
	}
	sheet = stamped(logger, sheet, "contact-sheet", "")
	if err := writeKeyed(outDir, SessionSheetName, sessionSheetKeyName, sheet, files); err != nil {
		return nil, err
	}
	logger.Info("Generated session contact sheet", "dir", videoDir, "files", len(files),
		"took", time.Since(start).Round(time.Millisecond))
	return sheet, nil
}

// ErrNotPortrait reports a session whose thumbnail is its first file's.
var ErrNotPortrait = errors.New("session is not portrait")

// WriteSessionThumbnail builds the three-up thumbnail of a portrait session
// in videoDir across files, in order: each panel shows the middle of its
// third of the whole session. It writes it and its key into outDir.
func WriteSessionThumbnail(ctx context.Context, logger *slog.Logger, videoDir, outDir string, files []SheetFile) ([]byte, error) {
	parts, total := sessionParts(ctx, logger, videoDir, files)
	defer closeParts(parts)
	if len(parts) == 0 {
		return nil, errors.New("session thumbnail: no readable files")
	}
	if !parts[0].info.Portrait() {
		return nil, ErrNotPortrait
	}
	targets := preview.TriptychTargets(total)
	cand, err := sessionShots(ctx, logger, parts, targets, preview.TriptychTile())
	if err != nil {
		return nil, fmt.Errorf("session thumbnail: %w", err)
	}
	var picks []image.Image
	for _, s := range pickSharper(cand, targets) {
		picks = append(picks, s.Img)
	}
	thumb, err := preview.Triptych(picks)
	if err != nil {
		return nil, err
	}
	thumb = stamped(logger, thumb, "thumbnail", "")
	if err := writeKeyed(outDir, SessionThumbnailName, sessionThumbnailKeyName, thumb, files); err != nil {
		return nil, err
	}
	return thumb, nil
}

// writeKeyed writes a session-wide image and the key of files it was built
// from.
func writeKeyed(outDir, imageName, keyName string, img []byte, files []SheetFile) error {
	key, err := json.Marshal(files)
	if err != nil {
		return err
	}
	if err := WriteFileAtomic(filepath.Join(outDir, imageName), img); err != nil {
		return err
	}
	return WriteFileAtomic(filepath.Join(outDir, keyName), key)
}

// sessionHeader is the sheet's header: who and when, then vcsi's lines for
// the whole session.
func sessionHeader(parts []*sheetPart, info preview.Info, hdr PreviewHeader) []string {
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
	var lines []string
	if len(who) > 0 {
		lines = append(lines, strings.Join(who, "  ·  "))
	}
	var size int64
	for _, p := range parts {
		size += p.Bytes
	}
	files := "1 file"
	if len(parts) > 1 {
		files = fmt.Sprintf("%d files", len(parts))
	}
	lines = append(lines, files+"  ·  "+humanize.IBytes(uint64(size)), "Duration: "+preview.Clock(info.Duration))
	dims := fmt.Sprintf("Dimensions: %dx%d", info.Width, info.Height)
	if info.Codec != "" {
		dims += "  ·  " + info.Codec
	}
	if info.FrameRate > 0 {
		dims += fmt.Sprintf("  ·  %.4g fps", info.FrameRate)
	}
	return append(lines, dims)
}
