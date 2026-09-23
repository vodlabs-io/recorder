package ffmpeg

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"
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

	cmd := exec.CommandContext(ctx, path, args...)
	return cmd, nil
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
