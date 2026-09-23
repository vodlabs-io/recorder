//go:build linux && arm64

package ffmpeg

import _ "embed"

//go:embed bin/linux_arm64/ffmpeg
var binaryData []byte

//go:embed bin/linux_arm64/ffprobe
var probeData []byte
