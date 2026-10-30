package processing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
// a slot lands in are opened. The key next to it lists the files it was
// built from, so a merge or a deleted file makes it stale.
const (
	SessionSheetName    = "contact-sheet.jpg"
	sessionSheetKeyName = "contact-sheet.json"
)

// SheetFile is one file a session sheet is built from.
type SheetFile struct {
	Name  string `json:"name"`
	Bytes int64  `json:"bytes"`
}

// SessionSheetCurrent reports whether dir holds a session sheet built from
// exactly files.
func SessionSheetCurrent(dir string, files []SheetFile) bool {
	b, err := os.ReadFile(filepath.Join(dir, sessionSheetKeyName))
	if err != nil {
		return false
	}
	var key []SheetFile
	if json.Unmarshal(b, &key) != nil || !slices.Equal(key, files) {
		return false
	}
	_, err = os.Stat(filepath.Join(dir, SessionSheetName))
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

// WriteSessionSheet builds the contact sheet of the session in videoDir from
// files, in order, and writes it and its key into outDir.
func WriteSessionSheet(ctx context.Context, logger *slog.Logger, videoDir, outDir string, files []SheetFile, hdr PreviewHeader) ([]byte, error) {
	start := time.Now()
	dec := previewDecoder
	var parts []*sheetPart
	defer func() {
		for _, p := range parts {
			if p.src != nil {
				p.src.Close()
			}
		}
	}()
	var total time.Duration
	for _, f := range files {
		p := &sheetPart{SheetFile: f, path: filepath.Join(videoDir, f.Name), offset: total}
		if m, err := ReadPreviewsManifest(filepath.Join(videoDir, PreviewsManifestName(f.Name))); err == nil && m.DurationMs > 0 {
			p.info = preview.Info{Width: m.Width, Height: m.Height, Duration: time.Duration(m.DurationMs) * time.Millisecond, Codec: m.Codec, FrameRate: m.FrameRate}
		} else {
			src, info, err := openSource(ctx, logger, p.path)
			if err != nil {
				logger.Debug("Skipping file for the session sheet", "file", f.Name, "error", err)
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
	if len(parts) == 0 {
		return nil, errors.New("session sheet: no readable files")
	}

	info := parts[0].info
	info.Duration = total
	targets := preview.ContactTargets(info)
	tile := preview.ContactTile(info, preview.ContactWidth)
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
		shots, err := contactCandidates(ctx, dec, p.src, p.path, p.info, local, tile)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			logger.Debug("Skipping file for the session sheet", "file", p.Name, "error", err)
			continue
		}
		for _, s := range shots {
			cand = append(cand, preview.Shot{At: s.At + p.offset, Img: s.Img})
		}
	}
	if len(cand) == 0 {
		return nil, errors.New("session sheet: no frames")
	}
	sheet, err := preview.ContactSheet(info, preview.ContactWidth, sessionHeader(parts, info, hdr), pickSharper(cand, targets))
	if err != nil {
		return nil, err
	}
	sheet = stamped(logger, sheet, "contact-sheet", "")
	key, err := json.Marshal(files)
	if err != nil {
		return nil, err
	}
	if err := WriteFileAtomic(filepath.Join(outDir, SessionSheetName), sheet); err != nil {
		return nil, err
	}
	if err := WriteFileAtomic(filepath.Join(outDir, sessionSheetKeyName), key); err != nil {
		return nil, err
	}
	logger.Info("Generated session contact sheet", "dir", videoDir, "files", len(files),
		"took", time.Since(start).Round(time.Millisecond))
	return sheet, nil
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
