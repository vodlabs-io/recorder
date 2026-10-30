# FFmpeg Package

This package provides embedded minimal static FFmpeg binaries for multiple platforms, optimized for processing recorded live streams.

## Overview

The ffmpeg package allows the application to use FFmpeg for post-processing without requiring users to install it separately. Minimal static binaries for multiple platforms are built from source and embedded directly into the Go binary during compilation. The ffmpeg binary is automatically extracted to the user's cache directory on first use:

- macOS: `~/Library/Caches/vodlabs/ffmpeg/<platform>/ffmpeg`
- Linux: `~/.cache/vodlabs/ffmpeg/<platform>/ffmpeg`
- Windows: `%LocalAppData%\vodlabs\ffmpeg\<platform>\ffmpeg.exe`

## Supported Platforms

- macOS Intel (darwin_amd64)
- macOS Apple Silicon (darwin_arm64)  
- Linux x86_64 (linux_amd64)
- Linux ARM64 (linux_arm64)
- Windows x86_64 (windows_amd64)
- Windows ARM64 (windows_arm64)

## Download Latest Binaries

```bash
./download.sh
```

This downloads minimal FFmpeg binaries from [GitHub releases](https://github.com/vodlabs-io/custom-ffmpeg/releases).
