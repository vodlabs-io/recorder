//go:build windows && arm64

package ffmpeg

import _ "embed"

//go:embed bin/windows_arm64/ffmpeg.exe
var binaryData []byte

//go:embed bin/windows_arm64/ffprobe.exe
var probeData []byte
