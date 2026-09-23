package processing

import (
	"bytes"
	"io"
)

// filterWriter wraps an io.Writer and drops lines containing noisy ffmpeg messages.
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
