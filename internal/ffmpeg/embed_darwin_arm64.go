//go:build darwin && arm64

package ffmpeg

import _ "embed"

//go:embed bin/darwin_arm64/ffmpeg
var binaryData []byte

//go:embed bin/darwin_arm64/ffprobe
var probeData []byte
