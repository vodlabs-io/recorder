package processing

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"

	"github.com/vodlabs-io/recorder/internal/ffmpeg"
	"github.com/vodlabs-io/recorder/internal/provenance"
)

// processFLVConvert converts a single file part (flvwriter .ts or webmwriter
// .webm) into .mp4. Conversions run eagerly, per part, during recording so a
// group's concat (which consumes the .mp4 intermediates) is ready the moment its
// split boundary is reached.
func (j *job) processFLVConvert(ctx context.Context, logger *slog.Logger) error {
	if j.inputPath == "" {
		return fmt.Errorf("missing input path for convert job")
	}

	ext := filepath.Ext(j.inputPath)
	outputPath := strings.TrimSuffix(j.inputPath, ext) + ".mp4"
	if ext != ".ts" && ext != ".webm" {
		outputPath = j.inputPath + ".mp4"
	}

	rec := j.provenanceRecord(filepath.Base(outputPath))
	args := flvConvertArgs(j.inputPath, outputPath, rec)
	if ext == ".webm" {
		args = webmConvertArgs(j.inputPath, outputPath, rec)
	}

	logger.Info("Converting part", "writer", j.vod.WriterName, "input", j.inputPath, "output", outputPath)
	j.writeLogFile(fmt.Sprintf("Converting %s to .mp4 with args: %s\n", ext, strings.Join(args, " ")))

	return j.runFFmpeg(ctx, args, outputPath, logger)
}

// flvConvertArgs is the .ts -> .mp4 remux for one flvwriter part.
func flvConvertArgs(inputPath, outputPath, rec string) []string {
	args := []string{
		// MPEG-TS carries no container-level decoder config, so the codec
		// parameters are only known once a video packet has been parsed. A
		// part that opens on audio pushes that past the default window, and
		// the copy then silently produces an audio-only .mp4 and exits 0.
		"-analyzeduration", ffmpeg.AnalyzeDuration,
		"-i", inputPath,
		"-err_detect", "ignore_err",
		"-fflags", "+genpts+discardcorrupt+igndts",
		"-avoid_negative_ts", "make_zero",
		"-loglevel", "error",
		"-c", "copy",
		"-flags", "+global_header",
		"-movflags", provenance.MovFlags("+faststart"),
	}
	args = append(args, provenance.MetadataArgs(rec)...)
	return append(args, "-y", outputPath)
}

// webmConvertArgs turns one webmwriter part (VP8 + Opus) into .mp4. MP4 here
// cannot carry VP8, so the video is encoded to H.264 with libx264, which every
// build has; a browser broadcaster's 720p VP8 encodes faster than real time at
// veryfast. Opus is copied. Frame timestamps pass through: the broadcaster's
// frame rate varies, and a constant-rate output would duplicate and drop frames.
func webmConvertArgs(inputPath, outputPath, rec string) []string {
	args := []string{
		"-i", inputPath,
		"-err_detect", "ignore_err",
		"-fflags", "+genpts+discardcorrupt",
		"-avoid_negative_ts", "make_zero",
		"-loglevel", "error",
		"-c:v", "libx264", "-preset", "veryfast", "-crf", "20", "-pix_fmt", "yuv420p",
		"-fps_mode", "passthrough",
		"-c:a", "copy",
		"-movflags", provenance.MovFlags("+faststart"),
	}
	args = append(args, provenance.MetadataArgs(rec)...)
	return append(args, "-y", outputPath)
}
