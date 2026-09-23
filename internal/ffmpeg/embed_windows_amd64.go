//go:build windows && amd64

package ffmpeg

import _ "embed"

//go:embed bin/windows_amd64/ffmpeg.exe
var binaryData []byte

//go:embed bin/windows_amd64/ffprobe.exe
var probeData []byte
