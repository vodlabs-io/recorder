package processing

import (
	"errors"
	"os/exec"
)

// errDiskFull is a job that stopped because the disk it writes to is full.
var errDiskFull = errors.New("the disk is full")

// ffmpegNoSpace reports whether ffmpeg exited with AVERROR(ENOSPC), its code
// for a full disk. ffmpeg exits with the negative error code, which Windows
// keeps whole (0xffffffe4) and Unix cuts to its low byte (228).
func ffmpegNoSpace(err error) bool {
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return false
	}
	code := exitErr.ExitCode()
	return code == 228 || uint32(code) == 0xffffffe4
}
