#!/bin/bash

# Script to download minimal ffmpeg binaries from GitHub releases
set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BIN_DIR="$SCRIPT_DIR/bin"

# Detect current platform unless explicitly overridden
GOOS="${GOOS:-$(uname -s | tr '[:upper:]' '[:lower:]')}"
GOARCH="${GOARCH:-$(uname -m)}"

# Normalize OS name
case "$GOOS" in
    darwin) GOOS="darwin" ;;
    linux) GOOS="linux" ;;
    mingw*|msys*|cygwin*|windows) GOOS="windows" ;;
    *)
        echo "ERROR: Unsupported OS: $GOOS"
        exit 1
        ;;
esac

# Normalize architecture
case "$GOARCH" in
    x86_64|amd64) GOARCH="amd64" ;;
    arm64|aarch64) GOARCH="arm64" ;;
    *)
        echo "ERROR: Unsupported architecture: $GOARCH"
        exit 1
        ;;
esac

PLATFORM="${GOOS}_${GOARCH}"

# Limit supported binaries to known release artifacts
case "$PLATFORM" in
    darwin_amd64|darwin_arm64|linux_amd64|linux_arm64|windows_amd64|windows_arm64) ;;
    *)
        echo "ERROR: No prebuilt ffmpeg binary available for platform: $PLATFORM"
        exit 1
        ;;
esac

# Repository information
REPO_OWNER="vodlabs-io"
REPO_NAME="custom-ffmpeg"
REPO="$REPO_OWNER/$REPO_NAME"

# Pinned release and per-platform checksums (custom-ffmpeg's build-ffmpeg.yml,
# FFmpeg n9.0.2 + x264 commit b35605ace3ddf7c1a5d67a2eb553f034aef41d55). Bump
# both together, deliberately, in a commit that names the new release.
RELEASE_TAG="ffmpeg-21-1"
sha256_for_platform() {
    case "$1" in
        darwin_amd64)  echo "7022762bc1b8a8984cf9159fd7860643eccab3a6c05fb3309e1a4d9671f45451" ;;
        darwin_arm64)  echo "361e846fcc142559b06cff2d65688e3c7c7703909c78809b67720c47d758000f" ;;
        linux_amd64)   echo "74948b9af757a1d95b09c8eece7ba0446a1b4b1e9e705d0feee4b15e504dbcc7" ;;
        linux_arm64)   echo "218b4468528f08ad4216eed05180178d2ca46a127b6a23ab6cb61e3d7e93013a" ;;
        windows_amd64) echo "20ca42c0712eab8c84b61b7cf97c3d17d070098ed2fdf531dc0107dfd0039c41" ;;
        windows_arm64) echo "4cf96550d68467b1e3c748c3aa2cb3488b7aa3657f26ee00b3b5065cf50bec16" ;;
    esac
}

sha256() {
    if command -v sha256sum >/dev/null 2>&1; then
        sha256sum "$1" | cut -d' ' -f1
    else
        shasum -a 256 "$1" | cut -d' ' -f1
    fi
}

echo "=========================================="
echo "FFmpeg Binary Download Script"
echo "=========================================="
echo ""
echo "This script downloads a minimal ffmpeg binary"
echo "from GitHub releases for the current platform: $PLATFORM"
echo ""

# Check for required tools
if ! command -v curl &> /dev/null; then
    echo "ERROR: curl is required but not installed."
    exit 1
fi

if ! command -v tar &> /dev/null; then
    echo "ERROR: tar is required but not installed."
    exit 1
fi

echo "Release: $RELEASE_TAG"
echo ""

# Create bin directory structure
mkdir -p "$BIN_DIR"

# Download binary for the current platform
echo "----------------------------------------"
echo "Downloading: $PLATFORM"
echo "----------------------------------------"

mkdir -p "$BIN_DIR/$PLATFORM"

DOWNLOAD_URL="https://github.com/$REPO/releases/download/$RELEASE_TAG/ffmpeg-$PLATFORM.tar.gz"
ARCHIVE_PATH="/tmp/ffmpeg-$PLATFORM.tar.gz"

echo "URL: $DOWNLOAD_URL"

# Download with progress bar
if ! curl -L --fail --progress-bar "$DOWNLOAD_URL" -o "$ARCHIVE_PATH" 2>&1; then
    echo "ERROR: Download failed for $PLATFORM"
    echo "You can try again later or download manually from: https://github.com/$REPO/releases"
    exit 1
fi

# Verify against the checksum pinned above before extracting anything from it.
EXPECTED_SHA256="$(sha256_for_platform "$PLATFORM")"
ACTUAL_SHA256="$(sha256 "$ARCHIVE_PATH")"
if [ "$ACTUAL_SHA256" != "$EXPECTED_SHA256" ]; then
    rm -f "$ARCHIVE_PATH"
    echo "ERROR: Checksum mismatch for $PLATFORM"
    echo "  expected: $EXPECTED_SHA256"
    echo "  actual:   $ACTUAL_SHA256"
    exit 1
fi
echo "Checksum OK: $ACTUAL_SHA256"

# Extract and clean archive
if ! tar xzf "$ARCHIVE_PATH" -C "$BIN_DIR/$PLATFORM" --strip-components=1; then
    rm -f "$ARCHIVE_PATH"
    echo "ERROR: Failed to extract archive for $PLATFORM"
    exit 1
fi
rm -f "$ARCHIVE_PATH"

# Verify extraction
if [ "$GOOS" = "windows" ]; then
    BINARY="$BIN_DIR/$PLATFORM/ffmpeg.exe"
    PROBE_BINARY="$BIN_DIR/$PLATFORM/ffprobe.exe"
else
    BINARY="$BIN_DIR/$PLATFORM/ffmpeg"
    PROBE_BINARY="$BIN_DIR/$PLATFORM/ffprobe"
fi

if [ ! -f "$BINARY" ]; then
    echo "ERROR: Expected binary not found after extraction: $BINARY"
    exit 1
fi

# Make executable (Unix platforms)
chmod +x "$BINARY" 2>/dev/null || true

# ffprobe ships in the same tarball starting with the releases that enable
# the merge feature. Older releases don't contain it; embed a 0-byte
# placeholder so //go:embed still resolves — the daemon then reports
# "ffprobe is not bundled in this build" instead of failing to compile.
if [ -f "$PROBE_BINARY" ]; then
    chmod +x "$PROBE_BINARY" 2>/dev/null || true
    echo "ffprobe: $PROBE_BINARY ($(ls -lh "$PROBE_BINARY" | awk '{print $5}'))"
else
    : > "$PROBE_BINARY"
    echo "WARNING: release $RELEASE_TAG does not include ffprobe for $PLATFORM;"
    echo "         created empty placeholder (probe features will be unavailable)."
fi

echo "Download complete for $PLATFORM"
echo "Binary: $BINARY"
echo "Size: $(ls -lh "$BINARY" | awk '{print $5}')"
echo "You can now build the application with embedded ffmpeg binary."
