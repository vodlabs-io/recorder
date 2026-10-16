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

// processFLVConvert converts a single flvwriter .ts part into .mp4. Conversions
// run eagerly, per part, during recording so a group's concat (which consumes the
// .mp4 intermediates) is ready the moment its split boundary is reached.
func (j *job) processFLVConvert(ctx context.Context, logger *slog.Logger) error {
	if j.inputPath == "" {
		return fmt.Errorf("missing input path for convert job")
	}

	outputPath := j.inputPath
	if before, ok := strings.CutSuffix(outputPath, ".ts"); ok {
		outputPath = before + ".mp4"
	} else if !strings.HasSuffix(outputPath, ".mp4") {
		outputPath = outputPath + ".mp4"
	}

	args := flvConvertArgs(j.inputPath, outputPath, j.provenanceRecord(filepath.Base(outputPath)))

	logger.Info("Converting flvwriter part", "input", j.inputPath, "output", outputPath)
	j.writeLogFile(fmt.Sprintf("Converting .ts to .mp4 with args: %s\n", strings.Join(args, " ")))

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
