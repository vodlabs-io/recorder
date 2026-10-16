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
    darwin_amd64|darwin_arm64|linux_amd64|linux_arm64|windows_amd64) ;;
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
RELEASE_TAG="ffmpeg-15-1"
sha256_for_platform() {
    case "$1" in
        darwin_amd64)  echo "4fd0890d8d3548efa08816c99db6c7a9949beb9a2d9dcd61277d2d0c42bba02c" ;;
        darwin_arm64)  echo "bdce9af5449a6ab8362a045019a3060666ec2ead5808e17638abe4f89b8c5739" ;;
        linux_amd64)   echo "86747d31d2e48f6d5f9ba2d5baedd7b4eecbe41383250bb327f2343faa839ef6" ;;
        linux_arm64)   echo "0c921f974b37c20baacc48a409fe015651ce1ccf7c0ab297373ced2c3eb80b54" ;;
        windows_amd64) echo "e83e3ffbe2971a0762d5ab7d6b3325719f388ccb2d1755923830fca2dab10c71" ;;
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
if [ "$PLATFORM" = "windows_amd64" ]; then
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
