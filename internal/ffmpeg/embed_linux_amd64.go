//go:build linux && amd64

package ffmpeg

import _ "embed"

//go:embed bin/linux_amd64/ffmpeg
var binaryData []byte

//go:embed bin/linux_amd64/ffprobe
var probeData []byte
