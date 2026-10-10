#!/bin/bash
set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# Allow override via environment variables, otherwise detect current platform
GOOS="${GOOS:-$(uname -s | tr '[:upper:]' '[:lower:]')}"
GOARCH="${GOARCH:-$(uname -m)}"

# Normalize OS name
case "$GOOS" in
    darwin) GOOS="darwin" ;;
    linux) GOOS="linux" ;;
    mingw*|msys*|cygwin*|windows) GOOS="windows" ;;
esac

# Normalize architecture
case "$GOARCH" in
    x86_64|amd64) GOARCH="amd64" ;;
    arm64|aarch64) GOARCH="arm64" ;;
esac

PLATFORM="${GOOS}_${GOARCH}"
BINARY_NAME="ffmpeg"
PROBE_NAME="ffprobe"
if [ "$GOOS" = "windows" ]; then
    BINARY_NAME="ffmpeg.exe"
    PROBE_NAME="ffprobe.exe"
fi

REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)/"
FFMPEG_DIR="${REPO_ROOT}internal/ffmpeg"
BINARY_PATH="${FFMPEG_DIR}/bin/${PLATFORM}/${BINARY_NAME}"
PROBE_PATH="${FFMPEG_DIR}/bin/${PLATFORM}/${PROBE_NAME}"

# ffprobe (or its placeholder) is required for //go:embed; re-download when the
# cache lacks it.
if [ ! -f "$BINARY_PATH" ] || [ ! -f "$PROBE_PATH" ]; then
    echo ""
    echo "FFmpeg binary not found at $BINARY_PATH"
    echo "Downloading ffmpeg binaries..."
    echo ""
    
    # Run download script
    if [ ! -f "${FFMPEG_DIR}/download.sh" ]; then
        echo "ERROR: download.sh not found at ${FFMPEG_DIR}/download.sh"
        exit 1
    fi
    
    if ! (cd "$FFMPEG_DIR" && bash download.sh); then
        echo ""
        echo "ERROR: Failed to download ffmpeg binaries"
        exit 1
    fi
    
    # Verify the binary was downloaded successfully
    if [ ! -f "$BINARY_PATH" ]; then
        echo ""
        echo "ERROR: Binary still not found after download at $BINARY_PATH"
        echo "The download may have failed or doesn't include binaries for your platform."
        exit 1
    fi
    
    BINARY_SIZE=$(ls -lh "$BINARY_PATH" | awk '{print $5}')
    echo ""
    echo "Successfully downloaded ffmpeg binary for $PLATFORM ($BINARY_SIZE)"
else
    BINARY_SIZE=$(ls -lh "$BINARY_PATH" | awk '{print $5}')
    echo "Found ffmpeg binary for $PLATFORM ($BINARY_SIZE)"
fi

cd "$REPO_ROOT"

# A fresh clone, or a shallow one, has no tags. Build anyway as "devel", the
# version of an unstamped build: the VodLabs service accepts it only from
# allowlisted development machines, and the desktop app never auto-updates it.
# Only release tags (v<number>...) count: other tags, like a draft-build marker,
# are not versions and the service rejects them as malformed.
GIT_TAG=$(git describe --tags --abbrev=0 --match 'v[0-9]*' 2>/dev/null || echo "devel")
SEMVER_STRING="${GIT_TAG#v}"
VERSION_PATH="github.com/vodlabs-io/recorder/internal/version.Version=$SEMVER_STRING"
COMMIT_PATH="github.com/vodlabs-io/recorder/internal/version.Commit=$(git rev-parse HEAD)"
DAEMON_BINARY_NAME="vlrecd"
CLI_BINARY_NAME="vod"
if [ "$GOOS" = "windows" ]; then
    DAEMON_BINARY_NAME="vlrecd.exe"
    CLI_BINARY_NAME="vod.exe"
fi

echo "Building daemon for $PLATFORM..."
GOOS="$GOOS" GOARCH="$GOARCH" go build -ldflags "-X $VERSION_PATH -X $COMMIT_PATH" -o "${REPO_ROOT}${DAEMON_BINARY_NAME}" ./cmd/daemon
echo "Built daemon: ${REPO_ROOT}${DAEMON_BINARY_NAME}"

echo "Building cli for $PLATFORM..."
GOOS="$GOOS" GOARCH="$GOARCH" go build -ldflags "-X $VERSION_PATH -X $COMMIT_PATH" -o "${REPO_ROOT}${CLI_BINARY_NAME}" ./cmd/cli
echo "Built cli: ${REPO_ROOT}${CLI_BINARY_NAME}"