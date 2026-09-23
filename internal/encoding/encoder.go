package encoding

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"runtime"
	"strconv"
	"strings"

	"github.com/vodlabs-io/recorder/internal/ffmpeg"
)

// EncoderStatus describes one H.264 encoder candidate on this machine, for
// the Settings dropdown.
type EncoderStatus struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Available: the embedded ffmpeg build includes the encoder.
	Available bool `json:"available"`
	// Working: the encoder passed a real self-test encode (drivers loaded).
	Working bool `json:"working"`
	// Hardware distinguishes HW encoders from the libx264 fallback.
	Hardware bool `json:"hardware"`
}

// softwareEncoder is the universal fallback, always compiled into the
// embedded build.
const softwareEncoder = "libx264"

var encoderNames = map[string]string{
	"h264_videotoolbox": "Apple VideoToolbox",
	"h264_nvenc":        "NVIDIA NVENC",
	"h264_qsv":          "Intel Quick Sync",
	"h264_amf":          "AMD AMF",
	"h264_mf":           "Windows Media Foundation",
	softwareEncoder:     "Software (libx264)",
}

// encoderName is an encoder id's display name, falling back to the id so a
// warning never shows an empty string.
func encoderName(id string) string {
	if n := encoderNames[id]; n != "" {
		return n
	}
	return id
}

// hwCandidates lists this platform's hardware H.264 encoders in preference
// order (mirrors scripts/concat.sh, extended for Windows). All accept
// ordinary software frames as input; VAAPI is deliberately excluded (needs
// GPU-surface uploads that don't fit the filter graph).
func hwCandidates() []string {
	switch runtime.GOOS {
	case "darwin":
		return []string{"h264_videotoolbox"}
	case "windows":
		return []string{"h264_nvenc", "h264_qsv", "h264_amf", "h264_mf"}
	case "linux":
		return []string{"h264_nvenc", "h264_qsv", "h264_amf"}
	default:
		return nil
	}
}

// KnownEncoderIDs returns the encoder ids valid as a ProcessingConfig.Encoder
// value on this platform (the software fallback included).
func KnownEncoderIDs() []string {
	return append(hwCandidates(), softwareEncoder)
}

// pickBitrate scales the hardware-encoder target bitrate (bits/s) to the
// output height (libx264 uses CRF instead and ignores this).
func pickBitrate(h int) int64 {
	switch {
	case h >= 2160:
		return 16_000_000
	case h >= 1440:
		return 10_000_000
	case h >= 1080:
		return 6_000_000
	case h >= 720:
		return 4_000_000
	default:
		return 2_000_000
	}
}

// sourceHeadroom is the multiplier applied to the source's video bitrate when
// deriving the re-encode ceiling: a generational re-encode at exactly the
// source rate visibly degrades, so allow 1.5× (expressed as num/den for
// integer math).
const (
	sourceHeadroomNum = 3
	sourceHeadroomDen = 2
)

// bitratePlan carries the video-rate decision for one re-encode.
type bitratePlan struct {
	// target is the hardware encoders' -b:v: the height ladder, capped by the
	// source-derived ceiling. Low-rate sources (e.g. ~600kbps TikTok streams)
	// would otherwise be inflated ~5× by the flat ladder.
	target int64
	// cap is the source-derived ceiling itself (0 = source rate unknown; no
	// cap). Also bounds libx264's CRF output via -maxrate.
	cap int64
}

// planBitrate combines the height ladder with the source's highest part
// video bitrate (0 = unknown).
func planBitrate(h int, srcMaxBps int64) bitratePlan {
	p := bitratePlan{target: pickBitrate(h)}
	if srcMaxBps > 0 {
		p.cap = srcMaxBps * sourceHeadroomNum / sourceHeadroomDen
		if p.cap < p.target {
			p.target = p.cap
		}
	}
	return p
}

// Audio re-encode rate bounds (bits/s): the AAC target defaults to 192k and
// is capped at 1.5× the source's own audio rate (same headroom as video),
// with a floor so AAC is never starved by a tiny or misreported source rate.
const (
	audioBitrateDefault = 192_000
	audioBitrateFloor   = 64_000
)

// planAudioBitrate derives the AAC -b:a from the source's highest part audio
// bitrate (0 = unknown ⇒ default).
func planAudioBitrate(srcMaxBps int64) int64 {
	if srcMaxBps <= 0 {
		return audioBitrateDefault
	}
	t := srcMaxBps * sourceHeadroomNum / sourceHeadroomDen
	if t < audioBitrateFloor {
		return audioBitrateFloor
	}
	if t > audioBitrateDefault {
		return audioBitrateDefault
	}
	return t
}

// encoderArgs returns the codec + quality options for the given encoder.
// Options differ per encoder, so each has its own recipe; qsv and
// MediaFoundation want nv12 input.
func encoderArgs(enc string, bp bitratePlan) []string {
	target := strconv.FormatInt(bp.target, 10)
	switch enc {
	case softwareEncoder:
		args := []string{"-c:v", "libx264", "-preset", "medium", "-crf", "20", "-pix_fmt", "yuv420p"}
		if bp.cap > 0 {
			// Capped CRF: quality-driven, but never past the source ceiling.
			args = append(args,
				"-maxrate", strconv.FormatInt(bp.cap, 10),
				"-bufsize", strconv.FormatInt(2*bp.cap, 10))
		}
		return args
	case "h264_videotoolbox", "h264_nvenc", "h264_amf":
		return []string{"-c:v", enc, "-b:v", target, "-pix_fmt", "yuv420p"}
	case "h264_qsv", "h264_mf":
		return []string{"-c:v", enc, "-b:v", target, "-pix_fmt", "nv12"}
	default:
		return nil
	}
}

// availableEncoders parses `ffmpeg -encoders` output into the set of encoder
// names compiled into the embedded build.
func availableEncoders(ctx context.Context, logger *slog.Logger) (map[string]bool, error) {
	cmd, err := ffmpeg.Command(ctx, logger, "-hide_banner", "-encoders")
	if err != nil {
		return nil, err
	}
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("ffmpeg -encoders: %w", err)
	}
	set := make(map[string]bool)
	for _, line := range strings.Split(string(out), "\n") {
		// Lines look like " V....D h264_videotoolbox   VideoToolbox H.264 Encoder".
		fields := strings.Fields(line)
		if len(fields) >= 2 && strings.HasPrefix(fields[0], "V") {
			set[fields[1]] = true
		}
	}
	return set, nil
}

// selfTestEncoder encodes a 1s synthetic clip with the given encoder args to
// the null muxer. Succeeds only if the encoder is present AND actually works
// (drivers loaded, options accepted). The clip uses the real target
// resolution and bitrate, not a tiny stand-in: hardware encoders pick an
// H.264 level from the resolution and refuse to create a session if the
// bitrate exceeds that level's ceiling — a small test frame at a high -b:v
// would fail spuriously even when the real encode is fine.
func selfTestEncoder(ctx context.Context, logger *slog.Logger, args []string, w, h int, logSink io.Writer) bool {
	testArgs := []string{
		"-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", fmt.Sprintf("testsrc=size=%dx%d:rate=30:duration=1", w, h),
	}
	testArgs = append(testArgs, args...)
	testArgs = append(testArgs, "-f", "null", "-")

	cmd, err := ffmpeg.Command(ctx, logger, testArgs...)
	if err != nil {
		return false
	}
	if logSink != nil {
		fmt.Fprintf(logSink, "--- encoder self-test: %s (%dx%d) ---\n", strings.Join(args, " "), w, h)
		cmd.Stderr = logSink
	}
	return cmd.Run() == nil
}

// hwFallbackWarning reports an auto-mode fallback to software after hardware
// encoders that this build has failed their self-test. A machine with no
// hardware candidate at all is not a fallback and gets no warning: software
// is simply the expected encoder there.
func hwFallbackWarning(failed []string, w, h int) string {
	if len(failed) == 0 {
		return ""
	}
	return fmt.Sprintf("%s failed a self-test at %dx%d; used the software encoder instead",
		strings.Join(failed, " and "), w, h)
}

// selectEncoder picks the encoder for a re-encode targeting a w×h canvas,
// honoring pref: "" (auto — first working hardware candidate, then
// software), "software"/"libx264", or a specific encoder id. When a forced
// encoder is unavailable or fails its self-test, it falls back to software
// and reports why via warning. srcMaxBps (0 = unknown) is the source parts'
// highest video bitrate, capping the target rate via planBitrate.
func (s *Service) selectEncoder(ctx context.Context, logger *slog.Logger, pref string, w, h int, srcMaxBps int64, logSink io.Writer) (args []string, chosen, warning string, err error) {
	bitrate := planBitrate(h, srcMaxBps)
	logger.Info("Video bitrate plan",
		"target_bps", bitrate.target, "source_cap_bps", bitrate.cap, "source_max_bps", srcMaxBps)

	avail, aerr := availableEncoders(ctx, logger)
	if aerr != nil {
		return nil, "", "", fmt.Errorf("detect encoders: %w", aerr)
	}

	// software is the terminal fallback. libx264 is compiled into every
	// embedded build, so a failed SELF-TEST here means the test harness is
	// broken (e.g. an ffmpeg build missing a lavfi piece), not the encoder —
	// proceed with libx264 anyway and surface a warning; if libx264 is truly
	// broken the real encode fails with the actual error retained on the job.
	software := func() (a []string, id, warn string, err error) {
		sw := encoderArgs(softwareEncoder, bitrate)
		if !avail[softwareEncoder] {
			return nil, "", "", fmt.Errorf("no H.264 encoder found (%s missing from this build)", softwareEncoder)
		}
		if !selfTestEncoder(ctx, logger, sw, w, h, logSink) {
			logger.Warn("Software encoder failed its self-test; proceeding unverified", "encoder", softwareEncoder)
			return sw, softwareEncoder, "encoder self-test could not run; used the software encoder unverified", nil
		}
		return sw, softwareEncoder, "", nil
	}

	switch pref {
	case "", "auto":
		var failed []string
		for _, cand := range hwCandidates() {
			if !avail[cand] {
				continue
			}
			a := encoderArgs(cand, bitrate)
			if selfTestEncoder(ctx, logger, a, w, h, logSink) {
				return a, cand, "", nil
			}
			logger.Info("Hardware encoder failed self-test; skipping", "encoder", cand)
			failed = append(failed, encoderName(cand))
		}
		a, id, swWarn, serr := software()
		// Auto otherwise swallows this: the merge silently runs many times
		// slower, and Settings still lists the encoder as working because it
		// self-tests at 1080p, where the same encoder usually is.
		warning = hwFallbackWarning(failed, w, h)
		if swWarn != "" {
			if warning != "" {
				warning += "; "
			}
			warning += swWarn
		}
		return a, id, warning, serr

	case "software", softwareEncoder:
		return software()

	default:
		if avail[pref] {
			a := encoderArgs(pref, bitrate)
			if a == nil {
				warning = fmt.Sprintf("unknown encoder %q; used software instead", pref)
			} else if selfTestEncoder(ctx, logger, a, w, h, logSink) {
				return a, pref, "", nil
			} else {
				warning = fmt.Sprintf("configured encoder %s failed its self-test at %dx%d; used software instead", encoderName(pref), w, h)
			}
		} else {
			warning = fmt.Sprintf("configured encoder %s is not available in this build; used software instead", encoderName(pref))
		}
		a, id, swWarn, serr := software()
		if swWarn != "" {
			warning += "; " + swWarn
		}
		return a, id, warning, serr
	}
}

// DetectEncoders reports every candidate encoder's availability and
// self-test result for the Settings UI. Results are cached (self-tests cost
// ~a second each); refresh forces a re-test.
func (s *Service) DetectEncoders(ctx context.Context, refresh bool) ([]EncoderStatus, error) {
	s.detectMu.Lock()
	defer s.detectMu.Unlock()
	if s.detected != nil && !refresh {
		return s.detected, nil
	}

	logger := s.logger.With("op", "detect_encoders")
	avail, err := availableEncoders(ctx, logger)
	if err != nil {
		return nil, err
	}

	// Self-test at a representative 1080p canvas (no source ⇒ plain ladder).
	const w, h = 1920, 1080
	bitrate := planBitrate(h, 0)

	var out []EncoderStatus
	for _, id := range KnownEncoderIDs() {
		st := EncoderStatus{
			ID:        id,
			Name:      encoderNames[id],
			Available: avail[id],
			Hardware:  id != softwareEncoder,
		}
		if st.Available {
			st.Working = selfTestEncoder(ctx, logger, encoderArgs(id, bitrate), w, h, io.Discard)
		}
		out = append(out, st)
	}
	s.detected = out
	return out, nil
}
