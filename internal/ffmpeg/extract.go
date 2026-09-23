package ffmpeg

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// ensureExtracted extracts an embedded binary to the cache directory if
// needed and returns its absolute path. baseName is the binary name without
// the Windows ".exe" suffix (e.g. "ffmpeg", "ffprobe").
func ensureExtracted(baseName string, data []byte) (string, error) {
	if len(data) == 0 {
		return "", fmt.Errorf("%s is not bundled in this build", baseName)
	}

	cacheDir, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("failed to get user cache directory: %w", err)
	}

	platform := fmt.Sprintf("%s_%s", runtime.GOOS, runtime.GOARCH)
	extractDir := filepath.Join(cacheDir, "vodlabs", "ffmpeg", platform)

	binaryName := baseName
	if runtime.GOOS == "windows" {
		binaryName += ".exe"
	}

	extractedPath := filepath.Join(extractDir, binaryName)

	// Reuse an existing extraction only when its hash matches the embedded
	// build: consecutive ffmpeg releases can be within bytes of each other in
	// size, and a truncated copy must be replaced too. (~20ms once per process,
	// under sync.Once.)
	if existing, err := os.ReadFile(extractedPath); err == nil {
		want := sha256.Sum256(data)
		got := sha256.Sum256(existing)
		if bytes.Equal(want[:], got[:]) {
			// Binary already matches, verify it's executable
			if runtime.GOOS != "windows" {
				if err := os.Chmod(extractedPath, 0755); err != nil {
					return "", fmt.Errorf("failed to set executable permission: %w", err)
				}
			}
			return extractedPath, nil
		}
	}

	if err := os.MkdirAll(extractDir, 0755); err != nil {
		return "", fmt.Errorf("failed to create extraction directory: %w", err)
	}

	if err := os.WriteFile(extractedPath, data, 0755); err != nil {
		return "", fmt.Errorf("failed to write binary to cache: %w", err)
	}

	return extractedPath, nil
}
