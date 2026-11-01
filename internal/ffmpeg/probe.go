package ffmpeg

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"strconv"
	"strings"
)

// ProbeResult holds the stream/container facts the recorder cares about for
// a single media file.
type ProbeResult struct {
	VideoCodec string
	Width      int
	Height     int
	// VideoProfile is the codec profile name ("High", "Main"), empty when
	// ffprobe cannot name it.
	VideoProfile string
	// PixFmt is the video pixel format ("yuv420p"), empty when unknown.
	PixFmt string
	// VideoFrameRate is the video stream's average frame rate over the file
	// (avg_frame_rate), else its nominal one (r_frame_rate); 0 when unknown.
	// The nominal rate of a live recording is often its clock rate instead.
	VideoFrameRate float64
	// VideoExtradataFingerprint identifies the video stream's decoder
	// configuration (avcC/hvcC), empty when the stream carries none. Two
	// streams sharing it can be spliced without re-encoding.
	VideoExtradataFingerprint string
	// AudioCodec is empty when the file has no audio stream.
	AudioCodec string
	// AudioProfile is the audio codec profile ("LC", "HE-AACv2"), empty when
	// unknown or when there is no audio.
	AudioProfile string
	// AudioSampleRate is the audio sample rate in Hz (0 when unknown).
	AudioSampleRate int
	// AudioChannels is the audio channel count (0 when unknown).
	AudioChannels int
	// AudioExtradataFingerprint is the audio counterpart, over the
	// AudioSpecificConfig. Empty when there is none.
	AudioExtradataFingerprint string
	// Duration is the container duration in seconds (0 when unknown).
	Duration float64
	// VideoBitRate is the video stream's bit rate in bits/s, falling back to
	// the container's overall rate (a slight overestimate) when the stream
	// doesn't report one. 0 when unknown.
	VideoBitRate int64
	// AudioBitRate is the audio stream's bit rate in bits/s. No container
	// fallback (that rate includes video). 0 when unknown or no audio.
	AudioBitRate int64
}

// flexString decodes a field ffprobe emits as a string when it has a name for
// the value and as a bare number when it doesn't (profile does both).
type flexString string

func (f *flexString) UnmarshalJSON(b []byte) error {
	s := string(b)
	if s == "null" {
		*f = ""
		return nil
	}
	if strings.HasPrefix(s, `"`) {
		var unquoted string
		if err := json.Unmarshal(b, &unquoted); err != nil {
			return err
		}
		*f = flexString(unquoted)
		return nil
	}
	*f = flexString(s)
	return nil
}

// probeJSON mirrors the subset of `ffprobe -print_format json` output we
// consume.
type probeJSON struct {
	Streams []struct {
		CodecType     string     `json:"codec_type"`
		CodecName     string     `json:"codec_name"`
		Profile       flexString `json:"profile"`
		Width         int        `json:"width"`
		Height        int        `json:"height"`
		PixFmt        string     `json:"pix_fmt"`
		AvgFrameRate  string     `json:"avg_frame_rate"`
		RFrameRate    string     `json:"r_frame_rate"`
		SampleRate    string     `json:"sample_rate"`
		Channels      int        `json:"channels"`
		Extradata     string     `json:"extradata"`
		ExtradataHash string     `json:"extradata_hash"`
		BitRate       string     `json:"bit_rate"`
	} `json:"streams"`
	Format struct {
		Duration string `json:"duration"`
		BitRate  string `json:"bit_rate"`
	} `json:"format"`
}

// AnalyzeDuration is the -analyzeduration every input is opened with. A part
// can carry several seconds of audio before its first video packet, and
// ffmpeg's 5s default gives up before reaching it — the video stream is then
// left with no pixel format, which a filter_complex input cannot be
// configured from. Only the time bound is raised, never probesize: a
// re-encode opens a whole batch of parts at once, so a larger probe buffer
// multiplies by the part count.
const AnalyzeDuration = "60M"

// Probe inspects a media file with the bundled ffprobe. It fails when the
// build embeds no ffprobe (see ProbePath).
func Probe(ctx context.Context, logger *slog.Logger, path string) (ProbeResult, error) {
	cmd, err := probeCommand(ctx, logger, probeArgs(path)...)
	if err != nil {
		return ProbeResult{}, err
	}

	out, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			if stderr := strings.TrimSpace(string(exitErr.Stderr)); stderr != "" {
				return ProbeResult{}, fmt.Errorf("ffprobe %s: %w: %s", path, err, stderr)
			}
		}
		return ProbeResult{}, fmt.Errorf("ffprobe %s: %w", path, err)
	}

	return parseProbe(out, path)
}

// probeArgs is the ffprobe invocation for one file.
func probeArgs(path string) []string {
	return []string{
		"-v", "error",
		"-analyzeduration", AnalyzeDuration,
		"-print_format", "json",
		"-show_format",
		"-show_streams",
		// Both describe each stream's extradata (not its payload): the dump
		// so identical decoder configs can be recognized through layout
		// differences, the hash as the fallback when it can't be parsed.
		"-show_data",
		"-show_data_hash", "md5",
		path,
	}
}

// parseProbe turns one ffprobe JSON document into a ProbeResult. Split out of
// Probe so the field extraction is testable without a binary.
func parseProbe(out []byte, path string) (ProbeResult, error) {
	var parsed probeJSON
	if err := json.Unmarshal(out, &parsed); err != nil {
		return ProbeResult{}, fmt.Errorf("parse ffprobe output for %s: %w", path, err)
	}

	var res ProbeResult
	for _, s := range parsed.Streams {
		switch s.CodecType {
		case "video":
			if res.VideoCodec == "" {
				res.VideoCodec = s.CodecName
				res.Width = s.Width
				res.Height = s.Height
				res.VideoProfile = string(s.Profile)
				res.PixFmt = s.PixFmt
				res.VideoFrameRate = parseRate(s.AvgFrameRate)
				if res.VideoFrameRate == 0 {
					res.VideoFrameRate = parseRate(s.RFrameRate)
				}
				res.VideoExtradataFingerprint = extradataFingerprint(s.Extradata, s.ExtradataHash)
				if v, err := strconv.ParseInt(s.BitRate, 10, 64); err == nil && v > 0 {
					res.VideoBitRate = v
				}
			}
		case "audio":
			if res.AudioCodec == "" {
				res.AudioCodec = s.CodecName
				res.AudioProfile = string(s.Profile)
				res.AudioChannels = s.Channels
				res.AudioExtradataFingerprint = extradataFingerprint(s.Extradata, s.ExtradataHash)
				if v, err := strconv.Atoi(s.SampleRate); err == nil && v > 0 {
					res.AudioSampleRate = v
				}
				if v, err := strconv.ParseInt(s.BitRate, 10, 64); err == nil && v > 0 {
					res.AudioBitRate = v
				}
			}
		}
	}
	if d := parsed.Format.Duration; d != "" {
		if v, err := strconv.ParseFloat(d, 64); err == nil && v > 0 {
			res.Duration = v
		}
	}
	if res.VideoBitRate == 0 {
		if v, err := strconv.ParseInt(parsed.Format.BitRate, 10, 64); err == nil && v > 0 {
			res.VideoBitRate = v
		}
	}

	if res.VideoCodec == "" {
		return res, fmt.Errorf("ffprobe %s: no video stream found", path)
	}
	return res, nil
}

// parseRate parses ffprobe's "num/den" rate, 0 when it is missing or 0/0.
func parseRate(s string) float64 {
	num, den, ok := strings.Cut(s, "/")
	if !ok {
		return 0
	}
	n, err1 := strconv.ParseFloat(num, 64)
	d, err2 := strconv.ParseFloat(den, 64)
	if err1 != nil || err2 != nil || n <= 0 || d <= 0 {
		return 0
	}
	return n / d
}
