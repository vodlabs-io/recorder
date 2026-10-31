package processing

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/vodlabs-io/recorder/internal/ffmpeg"
	"github.com/vodlabs-io/recorder/internal/provenance"
)

// ThumbnailName maps a produced VOD file name to its screenshot, relative to
// the session folder (vod1.mp4 → previews/vod1/thumbnail.jpg).
func ThumbnailName(vodFile string) string {
	return filepath.Join(PartPreviewsDir(vodFile), "thumbnail.jpg")
}

// thumbnailSeekOffsets are the seek positions tried in order when grabbing the
// screenshot frame. 30s skips typical stream-start placeholder frames; the 0s
// fallback covers VODs shorter than that.
var thumbnailSeekOffsets = []string{"30", "0"}

// GenerateThumbnail extracts a single downscaled JPEG frame from videoPath
// into outPath using the embedded ffmpeg. Exported because the VOD Archive
// API's on-demand path (VODs without a sidecar screenshot) shares it with
// processing.
//
// Requires an ffmpeg with the mjpeg encoder, image2 muxer, and scale filter;
// embedded builds without them fail here, which callers must treat as
// non-fatal (the VOD Archive shows placeholder art instead).
func GenerateThumbnail(ctx context.Context, logger *slog.Logger, videoPath, outPath string) error {
	if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
		return err
	}
	var lastErr error
	for _, seek := range thumbnailSeekOffsets {
		cmd, err := ffmpeg.Command(ctx, logger, thumbnailArgs(seek, videoPath, outPath)...)
		if err != nil {
			return fmt.Errorf("create ffmpeg command: %w", err)
		}
		var stderr bytes.Buffer
		cmd.Stdout = &stderr
		cmd.Stderr = &stderr

		runErr := cmd.Run()
		if ctx.Err() != nil {
			os.Remove(outPath)
			return ctx.Err()
		}
		if runErr != nil {
			lastErr = fmt.Errorf("ffmpeg thumbnail failed: %w (%s)", runErr, strings.TrimSpace(stderr.String()))
		} else if info, err := os.Stat(outPath); err != nil || info.Size() == 0 {
			// Seeking past EOF "succeeds" without writing a frame; retry closer
			// to the start.
			lastErr = fmt.Errorf("ffmpeg produced no frame at -ss %s", seek)
		} else {
			stampThumbnail(logger, outPath)
			return nil
		}
		os.Remove(outPath)
	}
	return lastErr
}

// stampThumbnail records what produced the image, in a JPEG comment segment.
// A thumbnail is usually published on its own, away from the video it came
// from, so it has to carry its own description.
//
// Failure is not worth losing a good thumbnail over: the image is left as it
// is and the reason is logged.
func stampThumbnail(logger *slog.Logger, path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		logger.Warn("Could not read thumbnail to stamp it", "path", path, "error", err)
		return
	}
	stamped, err := provenance.StampJPEG(data, provenance.Record("thumbnail", "", filepath.Base(path)))
	if err != nil {
		logger.Warn("Could not stamp thumbnail", "path", path, "error", err)
		return
	}
	if err := os.WriteFile(path, stamped, 0o644); err != nil {
		logger.Warn("Could not write stamped thumbnail", "path", path, "error", err)
	}
}

// thumbnailArgs is the single-frame grab at one seek offset.
func thumbnailArgs(seek, videoPath, outPath string) []string {
	return []string{
		"-hide_banner",
		"-loglevel", "error",
		// The frame is decoded through a filter chain, which cannot be
		// configured until the input's pixel format is known.
		"-analyzeduration", ffmpeg.AnalyzeDuration,
		"-ss", seek,
		"-i", videoPath,
		"-frames:v", "1",
		"-vf", "scale=480:-2",
		"-q:v", "4",
		"-y",
		outPath,
	}
}
