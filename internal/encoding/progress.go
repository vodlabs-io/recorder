package encoding

import (
	"bufio"
	"context"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
)

// encodeProgressFn receives encode-stage progress. percent is -1 when the
// total duration is unknown (indeterminate); speed is ffmpeg's realtime
// multiplier (0 when not yet reported).
type encodeProgressFn func(percent float64, etaSec int64, speed float64)

// parseEncodeProgress consumes ffmpeg's `-progress pipe:1` key=value stream,
// reporting at most ~1/s. The output begins offsetSec into a job of
// totalDurSec; totalDurSec 0 means the job's length is unknown.
//
// out_time_us is the reliable microsecond field; out_time_ms is parsed as a
// fallback but is ALSO microseconds (a long-standing ffmpeg quirk), and
// out_time=HH:MM:SS.micro is the last resort.
func parseEncodeProgress(r io.Reader, offsetSec, totalDurSec float64, report encodeProgressFn) {
	scanner := bufio.NewScanner(r)
	var outTimeSec, speed float64
	last := time.Time{}

	flush := func(force bool) {
		if report == nil {
			return
		}
		if !force && time.Since(last) < time.Second {
			return
		}
		last = time.Now()

		percent := -1.0
		var eta int64
		if totalDurSec > 0 {
			done := offsetSec + outTimeSec
			percent = done / totalDurSec * 100
			if percent > 100 {
				percent = 100
			}
			if speed > 0 {
				eta = int64((totalDurSec - done) / speed)
				if eta < 0 {
					eta = 0
				}
			}
		}
		report(percent, eta, speed)
	}

	for scanner.Scan() {
		line := scanner.Text()
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		switch key {
		case "out_time_us", "out_time_ms":
			if us, err := strconv.ParseInt(value, 10, 64); err == nil && us >= 0 {
				outTimeSec = float64(us) / 1e6
			}
		case "out_time":
			if sec, ok := parseClockTime(value); ok {
				outTimeSec = sec
			}
		case "speed":
			if v, err := strconv.ParseFloat(strings.TrimSuffix(value, "x"), 64); err == nil && v > 0 {
				speed = v
			}
		case "progress":
			flush(value == "end")
		}
	}
}

// parseClockTime parses ffmpeg's HH:MM:SS.micro form into seconds.
func parseClockTime(v string) (float64, bool) {
	parts := strings.Split(v, ":")
	if len(parts) != 3 {
		return 0, false
	}
	h, err1 := strconv.ParseFloat(parts[0], 64)
	m, err2 := strconv.ParseFloat(parts[1], 64)
	s, err3 := strconv.ParseFloat(parts[2], 64)
	if err1 != nil || err2 != nil || err3 != nil {
		return 0, false
	}
	return h*3600 + m*60 + s, true
}

// copyProgressFn receives copy-stage progress in bytes.
type copyProgressFn func(done, total int64)

// copyFileWithProgress copies src to dst (creating/truncating dst), fsyncs,
// and reports progress at most ~1/s. It honors ctx cancellation between
// chunks; on error or cancel the partial dst is left for the caller to
// clean up.
func copyFileWithProgress(ctx context.Context, src, dst string, report copyProgressFn) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	info, err := in.Stat()
	if err != nil {
		return err
	}
	total := info.Size()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}

	buf := make([]byte, 1<<20)
	var done int64
	last := time.Time{}
	for {
		if err := ctx.Err(); err != nil {
			out.Close()
			return err
		}
		n, rerr := in.Read(buf)
		if n > 0 {
			if _, werr := out.Write(buf[:n]); werr != nil {
				out.Close()
				return werr
			}
			done += int64(n)
			if report != nil && (time.Since(last) >= time.Second || done == total) {
				last = time.Now()
				report(done, total)
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			out.Close()
			return rerr
		}
	}

	if err := out.Sync(); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
