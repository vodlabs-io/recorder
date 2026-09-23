package ffmpeg

import (
	"fmt"
	"log"
)

// binaryData and probeData are provided by platform-specific embed files
// (embed_*.go) and contain the FFmpeg and FFprobe binaries for the target
// platform. probeData may be an empty placeholder (a custom-ffmpeg release
// without ffprobe); ProbePath then returns an error instead of a path.

func init() {
	// Warn at startup if an embedded binary looks missing or truncated.
	if err := verifyEmbeddedBinary("ffmpeg", binaryData); err != nil {
		log.Printf("WARNING: FFmpeg binary check failed: %v", err)
	}
	// An empty ffprobe placeholder is supported; only warn when it is present
	// but implausibly small.
	if len(probeData) > 0 {
		if err := verifyEmbeddedBinary("ffprobe", probeData); err != nil {
			log.Printf("WARNING: FFprobe binary check failed: %v", err)
		}
	}
}

// verifyEmbeddedBinary checks if the named binary is properly embedded.
func verifyEmbeddedBinary(name string, data []byte) error {
	if len(data) == 0 {
		return fmt.Errorf("%s binary data is empty (0 bytes)", name)
	}

	// Sanity check: ffmpeg/ffprobe binaries should be at least 1MB
	if len(data) < 1024*1024 {
		return fmt.Errorf("%s binary data seems too small (%d bytes), may be corrupted", name, len(data))
	}

	return nil
}
