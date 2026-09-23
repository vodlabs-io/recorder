//go:build darwin && amd64

package ffmpeg

import _ "embed"

//go:embed bin/darwin_amd64/ffmpeg
var binaryData []byte

//go:embed bin/darwin_amd64/ffprobe
var probeData []byte
