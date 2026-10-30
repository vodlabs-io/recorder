package preview

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Box is the size frames are scaled to fit inside, keeping their display
// aspect ratio. A zero dimension is unbounded.
type Box struct{ W, H int }

// Decoder turns pictures into JPEGs with ffmpeg, one process per batch, and
// bounds how many processes run at once across the whole daemon. Each
// process gets one decode thread and a lowered OS priority, so preview work
// never competes with recording.
type Decoder struct {
	path func() (string, error)
	sem  chan struct{}
}

// NewDecoder returns a Decoder running at most workers ffmpeg processes at
// once (at least one). path locates the ffmpeg binary.
func NewDecoder(path func() (string, error), workers int) *Decoder {
	if workers < 1 {
		workers = 1
	}
	return &Decoder{path: path, sem: make(chan struct{}, workers)}
}

// DecodeKeyframes decodes each keyframe into one JPEG scaled into box, in
// order. All keyframes must share cfg.
func (d *Decoder) DecodeKeyframes(ctx context.Context, cfg Config, frames []Keyframe, box Box, quality int) ([][]byte, error) {
	if len(frames) == 0 {
		return nil, nil
	}
	in, err := wrapMP4(cfg, frames)
	if err != nil {
		return nil, fmt.Errorf("preview: wrap keyframes: %w", err)
	}
	args := []string{
		"-hide_banner", "-loglevel", "error", "-nostdin",
		"-threads", "1",
		"-f", "mp4", "-i", "pipe:0",
	}
	args = append(args, outputArgs(box, quality, "passthrough", "")...)
	jpegs, err := d.run(ctx, args, bytes.NewReader(in))
	if err != nil {
		return nil, err
	}
	if len(jpegs) != len(frames) {
		return nil, fmt.Errorf("preview: %d keyframes decoded into %d frames", len(frames), len(jpegs))
	}
	return jpegs, nil
}

// FileOptions selects frames from a whole-file decode.
type FileOptions struct {
	// Interval is the spacing of output frames: frame n shows the picture
	// at n*Interval.
	Interval time.Duration
	// KeyframesOnly decodes nothing but keyframes; each output frame is then
	// the latest keyframe at or before its time.
	KeyframesOnly bool
	Box           Box
	Quality       int
	// Duration, when known, fixes the frame count at one per interval: ffmpeg
	// pads past the end of a full decode, and a keyframe-only decode stops
	// at the last keyframe, whose picture then fills the tail.
	Duration time.Duration
}

// DecodeFile reads a media file front to back and returns one JPEG per
// opts.Interval. It is the path for containers whose index Go doesn't read,
// and for short files whose keyframes are too sparse for the interval.
func (d *Decoder) DecodeFile(ctx context.Context, path string, opts FileOptions) ([][]byte, error) {
	if opts.Interval <= 0 {
		return nil, errors.New("preview: no interval")
	}
	args := []string{"-hide_banner", "-loglevel", "error", "-nostdin", "-threads", "1"}
	if opts.KeyframesOnly {
		args = append(args, "-skip_frame", "nokey")
	}
	args = append(args, "-i", path)
	rate := strconv.FormatFloat(1/opts.Interval.Seconds(), 'f', -1, 64)
	args = append(args, outputArgs(opts.Box, opts.Quality, "cfr", rate)...)
	jpegs, err := d.run(ctx, args, nil)
	if err != nil {
		return nil, err
	}
	if opts.Duration > 0 {
		n := len(Ticks(opts.Duration, opts.Interval))
		if len(jpegs) > n {
			jpegs = jpegs[:n]
		}
		for len(jpegs) > 0 && len(jpegs) < n {
			jpegs = append(jpegs, jpegs[len(jpegs)-1])
		}
	}
	return jpegs, nil
}

// outputArgs maps the decoded video to a stream of JPEGs on stdout. The first
// scale squares the pixels so the box fits the display aspect ratio.
func outputArgs(box Box, quality int, fpsMode, rate string) []string {
	w, h := box.W, box.H
	if w <= 0 {
		w = 16384
	}
	if h <= 0 {
		h = 16384
	}
	if quality <= 0 {
		quality = 5
	}
	vf := "scale=trunc(iw*sar/2)*2:ih,setsar=1," +
		"scale=" + strconv.Itoa(w) + ":" + strconv.Itoa(h) +
		":force_original_aspect_ratio=decrease:force_divisible_by=2"
	args := []string{"-an", "-sn", "-dn", "-map", "0:v:0", "-vf", vf, "-fps_mode", fpsMode}
	if rate != "" {
		args = append(args, "-r", rate)
	}
	return append(args, "-c:v", "mjpeg", "-q:v", strconv.Itoa(quality), "-f", "image2", "-update", "1", "pipe:1")
}

func (d *Decoder) run(ctx context.Context, args []string, stdin io.Reader) ([][]byte, error) {
	select {
	case d.sem <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-d.sem }()

	path, err := d.path()
	if err != nil {
		return nil, fmt.Errorf("preview: locate ffmpeg: %w", err)
	}
	cmd := exec.CommandContext(ctx, path, args...)
	lowerPriority(cmd)
	cmd.Stdin = stdin
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("preview: start ffmpeg: %w", err)
	}
	afterStart(cmd)
	jpegs, splitErr := splitJPEGs(bufio.NewReaderSize(out, 64<<10))
	waitErr := cmd.Wait()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if waitErr != nil {
		return nil, fmt.Errorf("preview: ffmpeg: %w (%s)", waitErr, strings.TrimSpace(stderr.String()))
	}
	if splitErr != nil {
		return nil, splitErr
	}
	return jpegs, nil
}

// splitJPEGs cuts ffmpeg's back-to-back mjpeg output into single images. It
// walks the marker segments rather than scanning for an end marker, so it
// never mistakes entropy-coded data for a boundary.
func splitJPEGs(r *bufio.Reader) ([][]byte, error) {
	var out [][]byte
	for {
		var img bytes.Buffer
		err := readJPEG(r, &img)
		if err == io.EOF && img.Len() == 0 {
			return out, nil
		}
		if err != nil {
			return out, fmt.Errorf("preview: split JPEG stream: %w", err)
		}
		out = append(out, bytes.Clone(img.Bytes()))
	}
}

func readJPEG(r *bufio.Reader, w *bytes.Buffer) error {
	var soi [2]byte
	if _, err := io.ReadFull(r, soi[:]); err != nil {
		return err
	}
	if soi[0] != 0xFF || soi[1] != 0xD8 {
		return errors.New("missing start of image")
	}
	w.Write(soi[:])
	pending := -1 // a marker already consumed by the scan copier
	for {
		var m byte
		if pending >= 0 {
			m = byte(pending)
			pending = -1
		} else {
			b, err := r.ReadByte()
			if err != nil {
				return io.ErrUnexpectedEOF
			}
			if b != 0xFF {
				return errors.New("expected marker")
			}
			if m, err = r.ReadByte(); err != nil {
				return io.ErrUnexpectedEOF
			}
		}
		w.WriteByte(0xFF)
		w.WriteByte(m)
		switch {
		case m == 0xD9: // end of image
			return nil
		case m == 0xFF || m == 0x01 || (m >= 0xD0 && m <= 0xD7):
			continue
		}
		var lb [2]byte
		if _, err := io.ReadFull(r, lb[:]); err != nil {
			return io.ErrUnexpectedEOF
		}
		w.Write(lb[:])
		n := int(lb[0])<<8 | int(lb[1])
		if n < 2 {
			return errors.New("bad segment length")
		}
		if _, err := io.CopyN(w, r, int64(n-2)); err != nil {
			return io.ErrUnexpectedEOF
		}
		if m == 0xDA { // start of scan: entropy-coded data up to the next real marker
			next, err := copyScan(r, w)
			if err != nil {
				return err
			}
			pending = int(next)
		}
	}
}

// copyScan copies entropy-coded bytes and returns the marker that ends them,
// already consumed. 0xFF00 is a stuffed byte and RSTn markers belong to the
// scan.
func copyScan(r *bufio.Reader, w *bytes.Buffer) (byte, error) {
	for {
		chunk, err := r.ReadSlice(0xFF)
		if err != nil && err != bufio.ErrBufferFull {
			return 0, io.ErrUnexpectedEOF
		}
		if err == bufio.ErrBufferFull {
			w.Write(chunk)
			continue
		}
		w.Write(chunk[:len(chunk)-1])
		n, err := r.ReadByte()
		if err != nil {
			return 0, io.ErrUnexpectedEOF
		}
		if n == 0x00 || (n >= 0xD0 && n <= 0xD7) {
			w.WriteByte(0xFF)
			w.WriteByte(n)
			continue
		}
		return n, nil
	}
}
