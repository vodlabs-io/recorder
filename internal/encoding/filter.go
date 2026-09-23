package encoding

import (
	"bytes"
	"io"
)

// filterWriter wraps an io.Writer and drops lines containing noisy ffmpeg
// messages (same as processing's filterWriter — the packages deliberately
// don't import each other for a 30-line utility).
type filterWriter struct {
	w   io.Writer
	buf []byte // partial line buffer
}

func (fw *filterWriter) Write(p []byte) (int, error) {
	fw.buf = append(fw.buf, p...)
	for {
		idx := bytes.IndexByte(fw.buf, '\n')
		if idx < 0 {
			break
		}
		line := fw.buf[:idx+1]
		fw.buf = fw.buf[idx+1:]
		if bytes.Contains(line, []byte("Non-monotonic DTS")) {
			continue
		}
		// TikTok H.264 streams trip this per-frame decoder nag (plus its
		// "upload a sample" follow-up), flooding ffmpeg.log on re-encodes.
		if bytes.Contains(line, []byte("Late SEI is not implemented")) ||
			bytes.Contains(line, []byte("streams.videolan.org/upload")) {
			continue
		}
		if _, err := fw.w.Write(line); err != nil {
			return len(p), err
		}
	}
	return len(p), nil
}

// flush writes any remaining partial line in the buffer.
func (fw *filterWriter) flush() error {
	if len(fw.buf) > 0 {
		_, err := fw.w.Write(fw.buf)
		fw.buf = nil
		return err
	}
	return nil
}
