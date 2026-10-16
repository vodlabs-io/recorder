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
	"github.com/vodlabs-io/recorder/internal/recording"
	"github.com/vodlabs-io/recorder/internal/stream"
)

// processGroupJob concatenates a worker-defined group of parts into a single
// vodN.mp4, queues that file for move (when a destination is set), and frees the
// group's local inputs. The worker is authoritative for grouping and numbering,
// so the job is self-contained: its inputs and output name are frozen on the row.
//
// It is idempotent on retry: a stale partial output is discarded and re-produced
// while the inputs are still present, and a group whose inputs are already gone
// but whose output exists (a crash between concat and cleanup) is treated as
// produced. The per-file move row is enqueued idempotently, so a re-run never
// queues a duplicate move.
func (j *job) processGroupJob(ctx context.Context, logger *slog.Logger, s *Service) error {
	if j.groupOutput == "" || len(j.groupInputs) == 0 {
		return fmt.Errorf("group job missing inputs or output")
	}

	outPath := filepath.Join(j.vod.BaseOutDir, j.groupOutput)
	inputs := j.groupInputPaths()
	inputsPresent := allPathsExist(inputs)
	_, statErr := os.Stat(outPath)
	outExists := statErr == nil

	switch {
	case inputsPresent:
		_ = os.Remove(outPath) // discard a stale partial from a hard crash
		if err := j.concatGroup(ctx, logger, inputs, outPath); err != nil {
			return err
		}
	case outExists:
		logger.Warn("Resuming group from existing output", "output", j.groupOutput)
	default:
		return fmt.Errorf("group %s: inputs missing and no output present", j.groupOutput)
	}

	// Grab the screenshot while the produced file is guaranteed local — the
	// move enqueued below may relocate it to a slow network destination. The
	// sidecar .jpg stays in the folder for the end-of-session sweep to carry
	// along. Best-effort: an ffmpeg without image support (older embedded
	// builds) must never fail the group.
	thumbPath := filepath.Join(j.vod.BaseOutDir, ThumbnailName(j.groupOutput))
	if _, err := os.Stat(thumbPath); os.IsNotExist(err) {
		if err := GenerateThumbnail(ctx, logger, outPath, thumbPath); err != nil {
			logger.Warn("Failed to generate thumbnail", "output", j.groupOutput, "error", err)
		}
	}
	// Copy it somewhere local before the move puts it out of easy reach.
	if _, err := os.Stat(thumbPath); err == nil {
		s.thumbnailCached(j.vod.SessionID, j.groupOutput, thumbPath)
	}

	// Queue the produced file for move before freeing inputs: the move targets the
	// output, so deleting the (now redundant) inputs is independent and safe.
	if err := s.enqueueFileMove(ctx, j, j.groupOutput); err != nil {
		return err
	}

	deletePartFiles := s.resolveDelete != nil && s.resolveDelete(ctx, j.strKey())
	j.deleteGroupInputs(logger, deletePartFiles)
	logger.Info("Produced split group", "output", j.groupOutput, "part_count", len(j.groupInputs))
	return nil
}

// fileParts reports whether the session's Parts are single files (FLV or
// WebM), which are converted to .mp4 intermediates, rather than HLS
// directories.
func (j *job) fileParts() bool {
	return recording.PartFileExt(j.vod.WriterName) != ""
}

// groupInputPaths maps the group's part stems to concat input paths: file parts
// are the converted .mp4 intermediates, HLS parts are the raw segment directories.
func (j *job) groupInputPaths() []string {
	paths := make([]string, 0, len(j.groupInputs))
	for _, stem := range j.groupInputs {
		if j.fileParts() {
			paths = append(paths, filepath.Join(j.vod.BaseOutDir, stem+".mp4"))
		} else {
			paths = append(paths, filepath.Join(j.vod.BaseOutDir, stem))
		}
	}
	return paths
}

// concatGroup assembles the group's inputs into outPath. A lone file part's
// intermediate is renamed rather than remuxed.
func (j *job) concatGroup(ctx context.Context, logger *slog.Logger, inputs []string, outPath string) error {
	if j.fileParts() && len(inputs) == 1 {
		if err := os.Rename(inputs[0], outPath); err != nil {
			return fmt.Errorf("rename single part to %s: %w", filepath.Base(outPath), err)
		}
		logger.Info("Single part renamed", "to", filepath.Base(outPath))
		return nil
	}

	concatFilePath := filepath.Join(j.vod.BaseOutDir, "concat_list.txt")
	defer os.Remove(concatFilePath)

	var sb strings.Builder
	for _, in := range inputs {
		// HLS parts are directories; concat their playlists. File parts are
		// listed as their .mp4 intermediates.
		listed := in
		if !j.fileParts() {
			listed = filepath.Join(in, "index.m3u8")
			if _, err := os.Stat(listed); err != nil {
				logger.Warn("Skipping part with missing playlist", "part", filepath.Base(in), "error", err)
				continue
			}
			if err := endPlaylist(listed); err != nil {
				logger.Warn("Failed to terminate part playlist", "part", filepath.Base(in), "error", err)
			}
		}
		fmt.Fprintf(&sb, "file '%s'\n", listed)
	}
	if err := os.WriteFile(concatFilePath, []byte(sb.String()), 0o644); err != nil {
		return fmt.Errorf("failed to write concat_list.txt: %w", err)
	}
	j.writeLogFile(fmt.Sprintf("Concatenating group %s: %d parts\n", filepath.Base(outPath), len(inputs)))

	if err := j.runFFmpeg(ctx, j.concatArgs(concatFilePath, outPath), outPath, logger); err != nil {
		return err
	}
	return j.verifyGroupVideo(ctx, logger, outPath)
}

// verifyGroupVideo fails a group that came out with no video track. ffmpeg
// drops an input stream whose codec it cannot determine and still exits 0, so
// without this the parts would be deleted behind an audio-only VOD. Failing
// here returns before deleteGroupInputs, keeping the parts for a retry. A
// session that asked for an audio-only variant is exempt.
func (j *job) verifyGroupVideo(ctx context.Context, logger *slog.Logger, outPath string) error {
	if stream.IsAudioOnlyVariant(j.vod.Variant) {
		return nil
	}

	probe, err := ffmpeg.Probe(ctx, logger, outPath)
	if err != nil {
		// Older embedded builds ship no ffprobe. Not being able to run the check
		// is no reason to fail a group that is probably fine.
		logger.Warn("Could not verify the group's video track",
			"output", filepath.Base(outPath), "error", err)
		return nil
	}
	if !groupLostVideo(j.vod.Variant, probe) {
		return nil
	}

	logger.Error("Group has no video track",
		"output", filepath.Base(outPath),
		"audio_codec", probe.AudioCodec,
		"duration_sec", probe.Duration,
		"parts", len(j.groupInputs))
	return fmt.Errorf("group %s has no video track (audio %q, %.0fs): "+
		"the source's decoder configuration was unreadable; its parts are kept",
		filepath.Base(outPath), probe.AudioCodec, probe.Duration)
}

// groupLostVideo reports whether a probed group is missing video it should have
// had. Split from the probing so the decision can be tested without media.
func groupLostVideo(variant string, probe ffmpeg.ProbeResult) bool {
	if stream.IsAudioOnlyVariant(variant) {
		return false
	}
	return probe.VideoCodec == ""
}

// endPlaylist appends EXT-X-ENDLIST to a part playlist that lacks one, which is
// what a part killed mid-write leaves behind. ffmpeg reads an unterminated
// playlist as live: it opens only the last three segments, then blocks reloading
// until its retry budget runs out, reducing the whole part to a few seconds.
func endPlaylist(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if bytes.Contains(data, []byte("#EXT-X-ENDLIST")) {
		return nil
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	tail := "#EXT-X-ENDLIST\n"
	if len(data) > 0 && data[len(data)-1] != '\n' {
		tail = "\n" + tail
	}
	_, err = f.WriteString(tail)
	return err
}

// concatArgs returns the ffmpeg arguments for concatenating a group. File-part
// output gets faststart and timestamp-repair flags; HLS just stream-copies the
// joined playlists.
func (j *job) concatArgs(concatFilePath, outPath string) []string {
	rec := j.provenanceRecord(filepath.Base(outPath))
	var args []string
	if j.fileParts() {
		args = []string{
			"-hide_banner",
			"-loglevel", "warning",
			"-stats",
			"-analyzeduration", ffmpeg.AnalyzeDuration,
			"-f", "concat",
			"-safe", "0",
			"-i", concatFilePath,
			"-c", "copy",
			"-fflags", "+genpts",
			"-avoid_negative_ts", "make_zero",
			"-movflags", provenance.MovFlags("+faststart"),
		}
	} else {
		args = []string{
			// The listed HLS playlists resolve to MPEG-TS segments, which carry no
			// container-level decoder config: a part that opens on audio is joined
			// into an audio-only group, with a zero exit status.
			"-analyzeduration", ffmpeg.AnalyzeDuration,
			"-f", "concat",
			"-safe", "0",
			"-i", concatFilePath,
			"-c", "copy",
		}
	}
	args = append(args, provenance.MetadataArgs(rec)...)
	return append(args, "-y", outPath)
}

// deleteGroupInputs frees a produced group's local inputs. FLV .mp4 intermediates
// are always removed; the raw part files and raw HLS segment directories are
// removed only when part-file deletion is enabled (otherwise they are retained
// for the end-of-session sweep, matching the global delete_part_files policy).
func (j *job) deleteGroupInputs(logger *slog.Logger, deletePartFiles bool) {
	for _, stem := range j.groupInputs {
		if j.fileParts() {
			mp4 := filepath.Join(j.vod.BaseOutDir, stem+".mp4")
			if err := os.Remove(mp4); err != nil && !os.IsNotExist(err) {
				logger.Warn("Failed to remove intermediate part", "path", mp4, "error", err)
			}
			if deletePartFiles {
				raw := filepath.Join(j.vod.BaseOutDir, stem+recording.PartFileExt(j.vod.WriterName))
				if err := os.Remove(raw); err != nil && !os.IsNotExist(err) {
					logger.Warn("Failed to remove raw part", "path", raw, "error", err)
				}
			}
			continue
		}
		if deletePartFiles {
			dir := filepath.Join(j.vod.BaseOutDir, stem)
			if err := os.RemoveAll(dir); err != nil && !os.IsNotExist(err) {
				logger.Warn("Failed to remove raw part dir", "path", dir, "error", err)
			}
		}
	}
}

// allPathsExist reports whether every path (file or directory) exists.
func allPathsExist(paths []string) bool {
	for _, p := range paths {
		if _, err := os.Stat(p); err != nil {
			return false
		}
	}
	return true
}
