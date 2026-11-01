package encoding

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/vodlabs-io/recorder/internal/dither"
	"github.com/vodlabs-io/recorder/internal/ffmpeg"
	"github.com/vodlabs-io/recorder/internal/processing"
	"github.com/vodlabs-io/recorder/internal/provenance"
	"github.com/vodlabs-io/recorder/internal/recording"
	"github.com/vodlabs-io/recorder/internal/vlstate"
	"github.com/vodlabs-io/recorder/internal/webm"
)

// mergedOutputName is the merge output file. Multi-part sessions only ever
// hold vodN.mp4 or vodN.webm, so the bare names never collide.
const mergedOutputName = "vod.mp4"

// mergedWebMName is the output of a merge whose parts are all WebM: a stream
// copy when they join as they are, otherwise mergedOutputName (see
// switchToMP4).
const mergedWebMName = "vod.webm"

// mergeOutputName is the output a merge of inputs starts out with.
func mergeOutputName(inputs []string) string {
	for _, in := range inputs {
		if filepath.Ext(in) != ".webm" {
			return mergedOutputName
		}
	}
	return mergedWebMName
}

func isWebM(name string) bool { return filepath.Ext(name) == ".webm" }

// mergingSuffix marks the in-flight output in the session dir; the scanner's
// ^vod(\d*)\.(mp4|webm)$ regex never matches it, so a partial copy is invisible.
const mergingSuffix = ".merging"

// bakSuffix hides kept originals from the scanner (and players).
const bakSuffix = ".bak"

// encodeLogName is the ffmpeg log in the session folder — same file the
// processing pipeline appends to.
const encodeLogName = "ffmpeg.log"

// partProbe is one input's probed facts plus the derived copy-compat
// signature.
type partProbe struct {
	name      string
	path      string
	res       ffmpeg.ProbeResult
	signature string
}

// mergePlan is everything the merge engine derives from probing the parts.
type mergePlan struct {
	parts    []partProbe
	targetW  int
	targetH  int
	uniform  bool
	allAudio bool
	// sumDur is the summed probed duration in seconds (0 when any part's
	// duration was unknown — progress then reports indeterminate, and the
	// duration verification and the dither are skipped).
	sumDur float64
	// srcMaxBps is the highest part video bitrate (bits/s, 0 when no part
	// reported one) — caps the re-encode target so low-rate sources aren't
	// inflated to the flat height ladder.
	srcMaxBps int64
	// srcMaxAudioBps is the audio-stream counterpart, capping the AAC rate.
	srcMaxAudioBps int64
	// hasVP8 says a part's video is VP8, which only a WebM output can copy.
	hasVP8 bool
	// vp8FrameRate is the highest frame rate measured over the VP8 parts, 0
	// when none could be. A re-encode runs at it as a constant rate: ffmpeg
	// cannot guess one from a browser broadcaster's millisecond clock and
	// falls back to 1000 fps, which starves the hardware encoders' per-frame
	// bit budget.
	vp8FrameRate float64
	// frameRate is the highest frame rate measured over the other parts, 0
	// when none has a plausible one.
	frameRate float64
}

// reencodeBatchParts is the most parts one re-encode opens at once. ffmpeg
// opens every input up front and keeps a decoder per part, so start-up time
// and memory grow with the count: a few hundred parts take minutes to start
// and gigabytes to run. Longer merges re-encode in batches that are then
// joined with a stream copy. A var so tests can batch a few parts.
var reencodeBatchParts = 40

// maxFrameRate bounds the probed frame rates a re-encode trusts: a
// stream without a real rate can report its clock rate instead.
const maxFrameRate = 120

// defaultFrameRate is the rate h264_mf runs at when no part names one.
const defaultFrameRate = 30

// stagingDir is the job's local scratch space. VOD folders often live on slow
// external storage, so ffmpeg writes the encode output locally and only the
// verified result is copied back. Kept across a daemon restart so an
// interrupted job resumes at the copy stage.
func stagingDir(jobID int64) (string, error) {
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("resolve user cache dir: %w", err)
	}
	return filepath.Join(cacheDir, "vodlabs", "recorder", "encode", fmt.Sprint(jobID)), nil
}

func (s *Service) cleanupStaging(jobID int64) {
	dir, err := stagingDir(jobID)
	if err != nil {
		return
	}
	if err := os.RemoveAll(dir); err != nil {
		s.logger.Warn("Failed to remove staging dir", "job_id", jobID, "error", err)
	}
}

func removePartial(path string) {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		// Best-effort; a stray partial is re-deleted on the next attempt.
		_ = err
	}
}

// runMergeJob executes one merge end to end:
// probe → encode to local staging (stream-copy fast path when the parts are
// uniform) → verify → copy into the session folder → finalize (thumbnail,
// originals, manifest). Returns a non-fatal warning (e.g. forced encoder
// fell back to software) alongside any error.
func (s *Service) runMergeJob(ctx context.Context, logger *slog.Logger, job *Job) (warning string, err error) {
	if err := checkOutputFree(job); err != nil {
		return "", err
	}

	logF, err := os.OpenFile(filepath.Join(job.Dir, encodeLogName), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return "", fmt.Errorf("open ffmpeg log: %w", err)
	}
	defer logF.Close()
	fmt.Fprintf(logF, "=== %s job %d started ===\n", jobTitle(job), job.ID)

	staging, err := stagingDir(job.ID)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(staging, 0755); err != nil {
		return "", fmt.Errorf("create staging dir: %w", err)
	}
	// Probe every part once (they must all still exist).
	plan, err := s.buildMergePlan(ctx, logger, job)
	if err != nil {
		return "", err
	}
	if job.Type == jobTypeConvert {
		plan.uniform = false
	} else if plan.hasVP8 && !isWebM(job.Output) && plan.uniform {
		logger.Info("Parts are VP8, which MP4 cannot carry; merge will re-encode")
		plan.uniform = false
	}
	logger.Info("Merge plan",
		"parts", len(plan.parts), "canvas", fmt.Sprintf("%dx%d", plan.targetW, plan.targetH),
		"uniform", plan.uniform, "all_audio", plan.allAudio, "sum_duration_sec", plan.sumDur,
		"source_max_bps", plan.srcMaxBps, "source_max_audio_bps", plan.srcMaxAudioBps)
	if isWebM(job.Output) && !plan.uniform {
		if err := s.switchToMP4(ctx, logger, job); err != nil {
			return "", err
		}
	}

	// Resume: a staged output that already verifies means a prior run was
	// killed between encode and copy — skip straight to the copy stage.
	stagedOut := filepath.Join(staging, job.Output)
	if verr := s.verifyStaged(ctx, logger, stagedOut, plan); verr == nil {
		logger.Info("Resuming from verified staged output", "staged", stagedOut)
	} else {
		removePartial(stagedOut)
		stagedOut, warning, err = s.encodeToStaging(ctx, logger, job, plan, staging, logF)
		if err != nil {
			return warning, err
		}
		if verr := s.verifyStaged(ctx, logger, stagedOut, plan); verr != nil {
			return warning, fmt.Errorf("merged output failed verification: %w", verr)
		}
	}
	outFinal := filepath.Join(job.Dir, job.Output)
	outMerging := outFinal + mergingSuffix
	removePartial(outMerging) // stale partial from a crash mid-copy

	// Copy stage: deliver the verified result into the session folder. The
	// VOD Archive shows this as "moving" (the folder may be on slow storage).
	s.setStage(ctx, job.ID, stageCopy)
	if err := s.deliverOutput(ctx, job, stagedOut, outMerging, outFinal); err != nil {
		removePartial(outMerging)
		return warning, fmt.Errorf("deliver merged output: %w", err)
	}

	// Finalize stage: thumbnail, originals, manifest, staging cleanup.
	s.setStage(ctx, job.ID, stageFinalize)
	s.finalizeMerge(ctx, logger, job, plan, outFinal)
	s.cleanupStaging(job.ID)

	fmt.Fprintf(logF, "=== %s job %d finished ===\n", jobTitle(job), job.ID)
	return warning, nil
}

// jobTitle names the job type in the session's ffmpeg.log.
func jobTitle(job *Job) string {
	if job.Type == jobTypeConvert {
		return "Convert"
	}
	return "Merge"
}

// reencodeWriter is the provenance writer name of a re-encoded output.
func reencodeWriter(job *Job) string {
	if job.Type == jobTypeConvert {
		return "convert+reencode"
	}
	return "merge+reencode"
}

// checkOutputFree fails a job whose output is already in the session folder.
func checkOutputFree(job *Job) error {
	if _, err := os.Stat(filepath.Join(job.Dir, job.Output)); err == nil {
		return fmt.Errorf("output %s already exists in the session folder", job.Output)
	}
	return nil
}

// switchToMP4 turns a WebM merge into a re-encode to mergedOutputName: the
// encoders write H.264, which WebM cannot carry. The new output is stored on
// the job row so a requeued job resumes with it.
func (s *Service) switchToMP4(ctx context.Context, logger *slog.Logger, job *Job) error {
	logger.Info("WebM parts cannot be joined as they are; merge will re-encode to MP4",
		"from", job.Output, "to", mergedOutputName)
	job.Output = mergedOutputName
	if err := checkOutputFree(job); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE encode_jobs SET output = ? WHERE id = ?`, job.Output, job.ID); err != nil {
		return fmt.Errorf("record merge output: %w", err)
	}
	return nil
}

// buildMergePlan probes every input and derives the target canvas and
// copy-compatibility.
func (s *Service) buildMergePlan(ctx context.Context, logger *slog.Logger, job *Job) (*mergePlan, error) {
	plan := &mergePlan{allAudio: true, uniform: true}
	sumKnown := true
	for _, name := range job.Inputs {
		path := filepath.Join(job.Dir, name)
		if _, err := os.Stat(path); err != nil {
			return nil, fmt.Errorf("part %s is missing: %w", name, err)
		}
		res, err := ffmpeg.Probe(ctx, logger, path)
		if err != nil {
			return nil, fmt.Errorf("probe %s: %w", name, err)
		}
		if res.VideoCodec == "vp8" {
			plan.hasVP8 = true
			// A VP8 stream changes size at key frames while its header keeps
			// the first size, so the canvas takes the largest frame.
			if v, err := webm.Scan(path); err != nil {
				logger.Warn("Could not scan VP8 part", "part", name, "error", err)
			} else {
				if v.Width*v.Height > res.Width*res.Height {
					logger.Info("Part has larger frames than its header names", "part", name,
						"header", fmt.Sprintf("%dx%d", res.Width, res.Height), "max", fmt.Sprintf("%dx%d", v.Width, v.Height))
					res.Width, res.Height = v.Width, v.Height
				}
				plan.vp8FrameRate = max(plan.vp8FrameRate, v.FrameRate)
			}
		} else if res.VideoFrameRate <= maxFrameRate {
			plan.frameRate = max(plan.frameRate, res.VideoFrameRate)
		}
		p := partProbe{
			name:      name,
			path:      path,
			res:       res,
			signature: copySignature(res),
		}
		if res.Width > plan.targetW {
			plan.targetW = res.Width
		}
		if res.Height > plan.targetH {
			plan.targetH = res.Height
		}
		if res.AudioCodec == "" {
			plan.allAudio = false
		}
		if len(plan.parts) > 0 && p.signature != plan.parts[0].signature {
			// Logged per part: this is the only record of why a merge that
			// looks uniform to the eye still has to re-encode.
			logger.Info("Part differs from the first; merge will re-encode",
				"part", name, "signature", p.signature, "first", plan.parts[0].signature)
			plan.uniform = false
		}
		if res.Duration <= 0 {
			sumKnown = false
		}
		plan.sumDur += res.Duration
		if res.VideoBitRate > plan.srcMaxBps {
			plan.srcMaxBps = res.VideoBitRate
		}
		if res.AudioBitRate > plan.srcMaxAudioBps {
			plan.srcMaxAudioBps = res.AudioBitRate
		}
		plan.parts = append(plan.parts, p)
	}
	if plan.targetW <= 0 || plan.targetH <= 0 {
		return nil, fmt.Errorf("could not determine target resolution")
	}
	// H.264 requires even dimensions.
	plan.targetW -= plan.targetW % 2
	plan.targetH -= plan.targetH % 2
	if !sumKnown {
		plan.sumDur = 0
	}
	return plan, nil
}

// copySignature fingerprints everything the mp4 muxer freezes from the first
// input: it writes one sample entry per track, so a part whose decoder config
// differs would be played back with the wrong one. Parts may only be
// stream-copied together when these match. VP8 leaves out the frame size:
// every key frame carries its own, so parts of different sizes join as one
// WebM stream.
func copySignature(res ffmpeg.ProbeResult) string {
	size := fmt.Sprintf("%dx%d", res.Width, res.Height)
	if res.VideoCodec == "vp8" {
		size = "any"
	}
	return fmt.Sprintf("v=%s/%s/%s/%s/%s a=%s/%dHz/%dch/%s/%s",
		res.VideoCodec, size, res.PixFmt, res.VideoProfile, res.VideoExtradataFingerprint,
		res.AudioCodec, res.AudioSampleRate, res.AudioChannels, res.AudioProfile, res.AudioExtradataFingerprint)
}

// encodeToStaging produces the merged file in the staging dir and returns its
// path: a lossless stream copy when the parts are uniform, otherwise scale/pad
// to the common canvas and re-encode. Staging is local on purpose:
// `+faststart` rewrites the whole output once the mux finishes, which is far
// slower on a network drive.
func (s *Service) encodeToStaging(ctx context.Context, logger *slog.Logger, job *Job, plan *mergePlan, staging string, logF *os.File) (stagedOut, warning string, err error) {
	stagedOut = filepath.Join(staging, job.Output)
	if plan.uniform {
		logger.Info("Parts match; using stream-copy fast path", "signature", plan.parts[0].signature)
		if err := s.runCopyConcat(ctx, logger, job, plan, stagedOut, logF); err != nil {
			if ctx.Err() != nil {
				return "", "", err
			}
			logger.Warn("Stream copy failed; falling back to re-encode", "error", err)
			removePartial(stagedOut)
		} else if verr := s.verifyStaged(ctx, logger, stagedOut, plan); verr != nil {
			// The concat demuxer splices mismatched decoder configs without
			// complaining and still exits 0, keeping only the first part's:
			// the signature catches what a probe can see, this catches the
			// rest.
			logger.Warn("Stream copy failed verification; falling back to re-encode", "error", verr)
			removePartial(stagedOut)
		} else {
			return stagedOut, "", nil
		}
		if isWebM(job.Output) {
			if err := s.switchToMP4(ctx, logger, job); err != nil {
				return "", "", err
			}
			stagedOut = filepath.Join(staging, job.Output)
		}
	}
	warning, err = s.runReencodeConcat(ctx, logger, job, plan, stagedOut, logF)
	return stagedOut, warning, err
}

// concatListContent builds the concat demuxer's list file: one
// `file '<path>'` line per part, with single quotes escaped for the demuxer's
// quoting rules (' → '\”).
func concatListContent(parts []partProbe) string {
	var sb strings.Builder
	for _, p := range parts {
		escaped := strings.ReplaceAll(p.path, "'", `'\''`)
		fmt.Fprintf(&sb, "file '%s'\n", escaped)
	}
	return sb.String()
}

// runCopyConcat joins uniform parts with the concat demuxer (no re-encode).
func (s *Service) runCopyConcat(ctx context.Context, logger *slog.Logger, job *Job, plan *mergePlan, out string, logF *os.File) error {
	listPath := filepath.Join(filepath.Dir(out), "concat_list.txt")
	if err := os.WriteFile(listPath, []byte(concatListContent(plan.parts)), 0o644); err != nil {
		return fmt.Errorf("write concat list: %w", err)
	}
	defer os.Remove(listPath)

	rec := provenance.Record("merge", job.SessionID, filepath.Base(out))
	return s.runFFmpegWithProgress(ctx, logger, job.ID, copyConcatArgs(listPath, out, rec, isWebM(job.Output)), out, plan.sumDur, logF)
}

// copyConcatArgs is the concat-demuxer stream copy, into MP4 or, for WebM
// parts, WebM. The output muxer is named rather than inferred from the
// filename, so the caller stays free to choose any output path.
func copyConcatArgs(listPath, out, rec string, toWebM bool) []string {
	format := []string{"-f", "mp4", "-movflags", provenance.MovFlags("+faststart")}
	if toWebM {
		format = []string{"-f", "webm"}
	}
	args := []string{
		"-hide_banner", "-loglevel", "warning", "-nostats",
		"-analyzeduration", ffmpeg.AnalyzeDuration,
		"-f", "concat", "-safe", "0", "-i", listPath,
		"-c", "copy",
	}
	args = append(args, format...)
	args = append(args, provenance.MetadataArgs(rec)...)
	return append(args, "-progress", "pipe:1", "-y", out)
}

// buildReencodeFilter builds the filter_complex graph that normalizes n parts
// onto a targetW×targetH canvas and concatenates them: each input is scaled
// (aspect-preserving), padded/centered on a black canvas, and pixel-format
// normalized; when every part has audio it is resampled to a common format so
// the streams are concatenable, otherwise the graph is video-only. Mirrors
// scripts/concat.sh.
func buildReencodeFilter(n, targetW, targetH int, allAudio bool, ditherFilter string) string {
	var filter, concatLabels strings.Builder
	for i := 0; i < n; i++ {
		fmt.Fprintf(&filter,
			"[%d:v]scale=%d:%d:force_original_aspect_ratio=decrease,pad=%d:%d:(ow-iw)/2:(oh-ih)/2:color=black,setsar=1,format=yuv420p[v%d];",
			i, targetW, targetH, targetW, targetH, i)
		if allAudio {
			fmt.Fprintf(&filter, "[%d:a]aresample=48000,aformat=sample_fmts=fltp:channel_layouts=stereo[a%d];", i, i)
			fmt.Fprintf(&concatLabels, "[v%d][a%d]", i, i)
		} else {
			fmt.Fprintf(&concatLabels, "[v%d]", i)
		}
	}
	// The offsets apply to the joined video, whose timeline starts at zero and
	// is what the script is written against.
	vout := "[outv]"
	if ditherFilter != "" {
		vout = "[cv]"
	}
	if allAudio {
		fmt.Fprintf(&filter, "%sconcat=n=%d:v=1:a=1%s[outa]", concatLabels.String(), n, vout)
	} else {
		fmt.Fprintf(&filter, "%sconcat=n=%d:v=1:a=0%s", concatLabels.String(), n, vout)
	}
	if ditherFilter != "" {
		fmt.Fprintf(&filter, ";[cv]%s[outv]", ditherFilter)
	}
	return filter.String()
}

// partsDir is the directory every part lives in, or "" when they span more
// than one.
func partsDir(parts []partProbe) string {
	if len(parts) == 0 {
		return ""
	}
	dir := filepath.Dir(parts[0].path)
	for _, p := range parts[1:] {
		if filepath.Dir(p.path) != dir {
			return ""
		}
	}
	return dir
}

// reencodeInputArgs is the input-option block for the re-encode concat: one
// `-i` per part, each opened with the options the graph needs. A non-empty
// dir is ffmpeg's working directory, and parts are named relative to it to
// keep the command line short.
func reencodeInputArgs(parts []partProbe, dir string) []string {
	args := make([]string, 0, len(parts)*6)
	for _, p := range parts {
		input := p.path
		if dir != "" {
			// file: keeps a name containing ':' from being read as a protocol.
			input = "file:" + filepath.Base(p.path)
		}
		args = append(args,
			// Every part is a filter_complex input, so ffmpeg must know its
			// pixel format before the graph can be configured; the default
			// analysis window is too short for a part that opens on audio.
			"-analyzeduration", ffmpeg.AnalyzeDuration,
			// A part can switch resolution mid-file when the stream
			// reconnects at a different rendition. Rebuilding the graph on
			// that change (the default) would reset the concat filter's
			// running timeline offset, dropping every later part back to the
			// start of the output; scale absorbs the change on its own.
			"-reinit_filter", "0",
			"-i", input)
	}
	return args
}

// runReencodeConcat normalizes mismatched parts onto a common canvas and
// joins them with the concat filter, in batches of at most
// reencodeBatchParts parts that are then stream-copied together.
func (s *Service) runReencodeConcat(ctx context.Context, logger *slog.Logger, job *Job, plan *mergePlan, stagedOut string, logF *os.File) (string, error) {
	encArgs, chosen, warning, err := s.selectEncoder(ctx, logger, job.Encoder, plan.targetW, plan.targetH, plan.srcMaxBps, &filterWriter{w: logF})
	if err != nil {
		return warning, err
	}
	frameRate := outputFrameRate(plan, chosen)
	logger.Info("Re-encoding to common canvas",
		"encoder", chosen, "canvas", fmt.Sprintf("%dx%d", plan.targetW, plan.targetH), "frame_rate", frameRate)
	if !plan.allAudio {
		logger.Info("At least one part has no audio; output will be video-only")
	}
	if plan.sumDur <= 0 {
		logger.Info("Merged duration unknown; skipping dither")
	}
	if plan.allAudio {
		logger.Info("Audio bitrate plan",
			"target_bps", planAudioBitrate(plan.srcMaxAudioBps), "source_max_bps", plan.srcMaxAudioBps)
	}

	batches := batchParts(plan.parts, reencodeBatchParts)
	if len(batches) == 1 {
		return warning, s.reencodeBatch(ctx, logger, job, plan, encArgs, frameRate, 0, plan.sumDur, stagedOut, logF)
	}
	logger.Info("Re-encoding in batches", "batches", len(batches), "max_parts", reencodeBatchParts)

	staging := filepath.Dir(stagedOut)
	encoded := make([]partProbe, 0, len(batches))
	var start float64
	for i, parts := range batches {
		bp := *plan
		bp.parts = parts
		bp.sumDur = partsDuration(plan, parts)
		out := filepath.Join(staging, fmt.Sprintf("batch%03d.mp4", i+1))
		// A batch finished before the daemon was stopped is kept.
		if s.verifyStaged(ctx, logger, out, &bp) == nil {
			logger.Info("Reusing encoded batch", "batch", i+1)
		} else {
			removePartial(out)
			if err := s.reencodeBatch(ctx, logger, job, &bp, encArgs, frameRate, start, plan.sumDur, out, logF); err != nil {
				return warning, fmt.Errorf("batch %d of %d: %w", i+1, len(batches), err)
			}
		}
		res, err := ffmpeg.Probe(ctx, logger, out)
		if err != nil {
			return warning, fmt.Errorf("probe batch %d: %w", i+1, err)
		}
		b := partProbe{name: filepath.Base(out), path: out, res: res, signature: copySignature(res)}
		if i > 0 && b.signature != encoded[0].signature {
			return warning, fmt.Errorf("batch %d was encoded differently from batch 1 and cannot be joined to it: %s, want %s",
				i+1, b.signature, encoded[0].signature)
		}
		encoded = append(encoded, b)
		// The next batch starts where this one ends once they are joined.
		start += res.Duration
	}

	logger.Info("Joining batches", "batches", len(encoded))
	listPath := filepath.Join(staging, "concat_list.txt")
	if err := os.WriteFile(listPath, []byte(concatListContent(encoded)), 0o644); err != nil {
		return warning, fmt.Errorf("write concat list: %w", err)
	}
	defer os.Remove(listPath)
	rec := provenance.Record(reencodeWriter(job), job.SessionID, filepath.Base(job.Output))
	// The join is a fast copy; its progress is shown as indeterminate.
	if err := s.runFFmpegWithProgress(ctx, logger, job.ID, copyConcatArgs(listPath, stagedOut, rec, false), stagedOut, 0, logF); err != nil {
		return warning, fmt.Errorf("join batches: %w", err)
	}
	for _, b := range encoded {
		removePartial(b.path)
	}
	return warning, nil
}

// batchParts splits parts into consecutive runs of at most n.
func batchParts(parts []partProbe, n int) [][]partProbe {
	var batches [][]partProbe
	for len(parts) > n {
		batches = append(batches, parts[:n])
		parts = parts[n:]
	}
	return append(batches, parts)
}

// partsDuration is the probed duration of parts, 0 when the plan's total is
// unknown.
func partsDuration(plan *mergePlan, parts []partProbe) float64 {
	if plan.sumDur <= 0 {
		return 0
	}
	var d float64
	for _, p := range parts {
		d += p.res.Duration
	}
	return d
}

// outputFrameRate is the constant frame rate a re-encode with encoder runs
// at, 0 to keep the parts' own timing. VP8 parts always need one (see
// vp8FrameRate). Media Foundation will not start without a frame rate, and
// the concat filter's output carries none.
func outputFrameRate(plan *mergePlan, encoder string) float64 {
	switch {
	case plan.vp8FrameRate > 0:
		return plan.vp8FrameRate
	case encoder != "h264_mf":
		return 0
	case plan.frameRate > 0:
		return plan.frameRate
	default:
		return defaultFrameRate
	}
}

// reencodeBatch re-encodes plan's parts into out in one ffmpeg run. out
// begins startSec into a merge of totalSec, which places its dither and its
// progress; plan.sumDur is the batch's own duration.
func (s *Service) reencodeBatch(ctx context.Context, logger *slog.Logger, job *Job, plan *mergePlan, encArgs []string, frameRate, startSec, totalSec float64, out string, logF *os.File) error {
	dir := filepath.Dir(out)
	ditherFilter, ditherPath, err := writeDither(dir, startSec, plan.sumDur)
	if err != nil {
		return err
	}
	if ditherPath != "" {
		defer os.Remove(ditherPath)
	}

	// The graph grows with every part, so it goes in a file rather than on
	// the command line, which Windows caps at 32K characters.
	filter := buildReencodeFilter(len(plan.parts), plan.targetW, plan.targetH, plan.allAudio, ditherFilter)
	filterPath := filepath.Join(dir, "filter_complex.txt")
	if err := os.WriteFile(filterPath, []byte(filter), 0o644); err != nil {
		return fmt.Errorf("write filter graph: %w", err)
	}
	defer os.Remove(filterPath)

	inputDir := partsDir(plan.parts)
	args := reencodeArgs(job, plan, encArgs, frameRate, filterPath, inputDir, out)
	return s.runFFmpegInDir(ctx, logger, job.ID, args, inputDir, out, startSec, totalSec, logF)
}

// writeDither writes the dither script for a clip of durationSec that starts
// startSec into the merged timeline into dir, and returns the filter chain
// that reads it and its path. Both are empty when the duration is unknown:
// sendcmd rejects an empty script.
func writeDither(dir string, startSec, durationSec float64) (filter, path string, err error) {
	script := dither.VideoScriptAt(startSec, durationSec)
	if script == "" {
		return "", "", nil
	}
	path = filepath.Join(dir, "dither.txt")
	if err := os.WriteFile(path, []byte(script), 0o644); err != nil {
		return "", "", fmt.Errorf("write dither script: %w", err)
	}
	return dither.VideoFilter(path), path, nil
}

// reencodeArgs builds the re-encode concat command line, which reads the
// filter graph from filterPath. Parts are named relative to inputDir when it
// is non-empty (see reencodeInputArgs). A frameRate above 0 makes the output
// constant-rate at it.
func reencodeArgs(job *Job, plan *mergePlan, encArgs []string, frameRate float64, filterPath, inputDir, stagedOut string) []string {
	args := []string{"-hide_banner", "-loglevel", "warning", "-nostats"}
	args = append(args, reencodeInputArgs(plan.parts, inputDir)...)

	if plan.allAudio {
		args = append(args, "-/filter_complex", filterPath, "-map", "[outv]", "-map", "[outa]")
		args = append(args, encArgs...)
		args = append(args, "-c:a", "aac", "-b:a", strconv.FormatInt(planAudioBitrate(plan.srcMaxAudioBps), 10))
	} else {
		args = append(args, "-/filter_complex", filterPath, "-map", "[outv]")
		args = append(args, encArgs...)
		args = append(args, "-an")
	}
	if frameRate > 0 {
		args = append(args, "-fps_mode", "cfr", "-r", strconv.FormatFloat(frameRate, 'f', -1, 64))
	}
	args = append(args, "-movflags", provenance.MovFlags("+faststart"))
	args = append(args, provenance.MetadataArgs(
		provenance.Record(reencodeWriter(job), job.SessionID, filepath.Base(job.Output)))...)
	return append(args, "-progress", "pipe:1", "-y", stagedOut)
}

// runFFmpegWithProgress executes ffmpeg, streaming -progress output into the
// KV progress key and stderr into the session's ffmpeg.log. On failure or
// cancel the partial output is removed.
func (s *Service) runFFmpegWithProgress(ctx context.Context, logger *slog.Logger, jobID int64, args []string, outputPath string, totalDurSec float64, logF *os.File) error {
	return s.runFFmpegInDir(ctx, logger, jobID, args, "", outputPath, 0, totalDurSec, logF)
}

// runFFmpegInDir is runFFmpegWithProgress with ffmpeg started in dir (an
// empty dir keeps the daemon's working directory), for an output that
// begins offsetSec into a job of totalDurSec.
func (s *Service) runFFmpegInDir(ctx context.Context, logger *slog.Logger, jobID int64, args []string, dir, outputPath string, offsetSec, totalDurSec float64, logF *os.File) error {
	cmd, err := ffmpeg.Command(ctx, logger, args...)
	if err != nil {
		return fmt.Errorf("create ffmpeg command: %w", err)
	}
	cmd.Dir = dir

	fw := &filterWriter{w: logF}
	cmd.Stderr = fw

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("open ffmpeg progress pipe: %w", err)
	}

	logger.Info("Starting ffmpeg", "ffmpeg_args", strings.Join(args, " "))
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start ffmpeg: %w", err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		parseEncodeProgress(stdout, offsetSec, totalDurSec, func(percent float64, etaSec int64, speed float64) {
			s.store.PutEncodeProgress(ctx, vlstate.EncodeProgress{
				JobID:   jobID,
				Stage:   stageEncode,
				Percent: percent,
				EtaSec:  etaSec,
				Speed:   speed,
			})
		})
	}()

	runErr := cmd.Wait()
	<-done
	fw.flush()

	if ctx.Err() != nil {
		removePartial(outputPath)
		return fmt.Errorf("ffmpeg cancelled: %w", ctx.Err())
	}
	if runErr != nil {
		removePartial(outputPath)
		return fmt.Errorf("ffmpeg failed: %w", runErr)
	}
	return nil
}

// verifyStaged checks the staged output is a plausible merge of the parts:
// nonzero size and (when the parts' total duration is known) a duration
// within max(5s, 2%) of the sum.
func (s *Service) verifyStaged(ctx context.Context, logger *slog.Logger, stagedOut string, plan *mergePlan) error {
	info, err := os.Stat(stagedOut)
	if err != nil {
		return err
	}
	if info.Size() == 0 {
		return fmt.Errorf("output is empty")
	}
	res, err := ffmpeg.Probe(ctx, logger, stagedOut)
	if err != nil {
		return fmt.Errorf("probe output: %w", err)
	}
	if plan.sumDur > 0 {
		tol := plan.sumDur * 0.02
		if tol < 5 {
			tol = 5
		}
		diff := res.Duration - plan.sumDur
		if diff < 0 {
			diff = -diff
		}
		if diff > tol {
			return fmt.Errorf("duration %.1fs deviates from expected %.1fs by more than %.1fs",
				res.Duration, plan.sumDur, tol)
		}
	}
	return nil
}

// deliverOutput moves the staged result into the session folder: a rename
// when staging and the session share a volume, otherwise a chunked copy with
// progress, then an atomic rename onto the final name.
func (s *Service) deliverOutput(ctx context.Context, job *Job, stagedOut, outMerging, outFinal string) error {
	if err := os.Rename(stagedOut, outMerging); err != nil {
		// Cross-device (or any rename failure): stream the bytes over.
		if copyErr := copyFileWithProgress(ctx, stagedOut, outMerging, func(done, total int64) {
			percent := -1.0
			if total > 0 {
				percent = float64(done) / float64(total) * 100
			}
			s.store.PutEncodeProgress(ctx, vlstate.EncodeProgress{
				JobID:   job.ID,
				Stage:   stageCopy,
				Percent: percent,
			})
		}); copyErr != nil {
			return copyErr
		}
	}
	return os.Rename(outMerging, outFinal)
}

// finalizeMerge performs the post-copy bookkeeping. Everything here is
// best-effort: the merged file is already in place, so failures only degrade
// metadata (logged, never fatal).
func (s *Service) finalizeMerge(ctx context.Context, logger *slog.Logger, job *Job, plan *mergePlan, outFinal string) {
	// Thumbnail and previews for the merged file (best-effort, like the
	// group job's).
	thumbPath := filepath.Join(job.Dir, processing.ThumbnailName(job.Output))
	thumbErr := processing.GenerateThumbnail(ctx, logger, outFinal, thumbPath)
	if _, err := processing.WritePreviews(ctx, logger, job.Dir, job.Dir, job.Output,
		processing.PreviewHeader{Platform: job.Platform, Username: job.Username}); err != nil {
		logger.Warn("Failed to generate merged previews", "error", err)
	}
	if thumbErr != nil {
		logger.Warn("Failed to generate merged thumbnail", "error", thumbErr)
	} else if s.cacheThumbnail != nil {
		// job.Dir may be on slow archive storage, so cache the screenshot while
		// it is known to be readable.
		s.cacheThumbnail(job.SessionID, job.Output, thumbPath)
	}

	// Originals: delete, or hide as .bak. Their sidecar thumbnails and
	// previews are orphaned either way.
	for _, p := range plan.parts {
		if job.DeleteParts {
			if err := os.Remove(p.path); err != nil && !os.IsNotExist(err) {
				logger.Warn("Failed to remove original part", "part", p.name, "error", err)
			}
		} else {
			bak := p.path + bakSuffix
			removePartial(bak) // overwrite a stale backup from a prior merge
			if err := os.Rename(p.path, bak); err != nil {
				logger.Warn("Failed to rename original part", "part", p.name, "error", err)
			}
		}
		thumb := processing.SidecarPath(job.Dir, processing.ThumbnailName(p.name))
		if err := os.Remove(thumb); err != nil && !os.IsNotExist(err) {
			logger.Warn("Failed to remove part thumbnail", "part", p.name, "error", err)
		}
		processing.RemovePreviews(job.Dir, p.name)
		if s.forgetThumbnail != nil {
			s.forgetThumbnail(job.SessionID, p.name)
		}
	}

	// Rewrite the manifest: the session now has one file with a measured
	// (media-accurate) duration.
	s.rewriteManifest(ctx, logger, job, outFinal)
}

// rewriteManifest replaces the session manifest's file list with the merged
// output, preferring probed durations over the recorder's wall-clock
// estimates.
func (s *Service) rewriteManifest(ctx context.Context, logger *slog.Logger, job *Job, outFinal string) {
	info, err := os.Stat(outFinal)
	if err != nil {
		logger.Warn("Cannot stat merged output for manifest", "error", err)
		return
	}
	measured := 0.0
	if res, err := ffmpeg.Probe(ctx, logger, outFinal); err == nil {
		measured = res.Duration
	} else {
		logger.Warn("Cannot probe merged output for manifest", "error", err)
	}

	m, err := recording.ReadManifest(job.Dir)
	if err != nil {
		// A folder without a manifest: reconstruct identity from the job.
		m = recording.Manifest{
			Platform:  job.Platform,
			Username:  job.Username,
			SessionID: job.SessionID,
		}
	}

	// The merged file spans the originals' recorded time range.
	merged := recording.ManifestFile{
		Name:        job.Output,
		Bytes:       info.Size(),
		DurationSec: measured,
	}
	inputSet := make(map[string]bool, len(job.Inputs))
	for _, in := range job.Inputs {
		inputSet[in] = true
	}
	var kept []recording.ManifestFile
	for _, f := range m.Files {
		if !inputSet[f.Name] {
			kept = append(kept, f)
			continue
		}
		if merged.Start.IsZero() || (!f.Start.IsZero() && f.Start.Before(merged.Start)) {
			merged.Start = f.Start
		}
		if !f.End.IsZero() && f.End.After(merged.End) {
			merged.End = f.End
		}
	}
	if merged.Start.IsZero() {
		merged.Start = m.Start
	}
	if merged.End.IsZero() {
		merged.End = m.End
	}
	if measured == 0 {
		// Probe failed: fall back to the wall-clock sum the inputs carried.
		for _, f := range m.Files {
			if inputSet[f.Name] {
				merged.DurationSec += f.DurationSec
			}
		}
	}

	m.Files = append([]recording.ManifestFile{merged}, kept...)
	m.TotalBytes = merged.Bytes
	m.DurationSec = merged.DurationSec
	for _, f := range kept {
		m.TotalBytes += f.Bytes
		m.DurationSec += f.DurationSec
	}

	if err := recording.WriteManifest(job.Dir, m); err != nil {
		logger.Warn("Failed to rewrite session manifest", "error", err)
	}
}
