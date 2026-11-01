package ffmpeg

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"runtime"
	"sync"
)

var (
	initOnce   sync.Once
	initErr    error
	binaryPath string

	probeOnce sync.Once
	probeErr  error
	probePath string
)

// Path returns the absolute path to the extracted ffmpeg binary.
// It ensures the binary is extracted on first call.
func Path() (string, error) {
	initOnce.Do(func() {
		binaryPath, initErr = ensureExtracted("ffmpeg", binaryData)
	})
	return binaryPath, initErr
}

// ProbePath returns the absolute path to the extracted ffprobe binary.
// It fails when the build embeds no ffprobe (an empty placeholder).
func ProbePath() (string, error) {
	probeOnce.Do(func() {
		probePath, probeErr = ensureExtracted("ffprobe", probeData)
	})
	return probePath, probeErr
}

// Command creates an exec.Cmd for running ffmpeg with the given arguments.
// The context is used for cancellation.
func Command(ctx context.Context, logger *slog.Logger, args ...string) (*exec.Cmd, error) {
	path, err := Path()
	if err != nil {
		return nil, fmt.Errorf("failed to get ffmpeg path: %w", err)
	}
	logger.Info("Using ffmpeg binary", "path", path)
	if runtime.GOOS == "windows" {
		if n := commandLineLen(path, args); n > maxWindowsCommandLine {
			return nil, fmt.Errorf("ffmpeg command line is about %d characters, over the Windows limit of %d", n, maxWindowsCommandLine)
		}
	}

	cmd := exec.CommandContext(ctx, path, args...)
	return cmd, nil
}

// maxWindowsCommandLine is CreateProcess's limit, less the terminating NUL.
const maxWindowsCommandLine = 32766

// commandLineLen estimates the length of the Windows command line for path
// and args: each argument plus a separator and a pair of quotes.
func commandLineLen(path string, args []string) int {
	n := len(path) + 2
	for _, a := range args {
		n += len(a) + 3
	}
	return n
}

// probeCommand creates an exec.Cmd for running ffprobe with the given
// arguments. The context is used for cancellation.
func probeCommand(ctx context.Context, logger *slog.Logger, args ...string) (*exec.Cmd, error) {
	path, err := ProbePath()
	if err != nil {
		return nil, fmt.Errorf("failed to get ffprobe path: %w", err)
	}
	logger.Debug("Using ffprobe binary", "path", path)

	cmd := exec.CommandContext(ctx, path, args...)
	return cmd, nil
}
