#!/bin/bash

# Concatenate already-processed vodN.mp4 files (vod1.mp4, vod2.mp4, ...)
# into a single vod.mp4. The parts are assumed to be fully processed and
# only need joining.
#
# Fast path: if every part shares the same video codec, resolution and audio
# layout, they are joined with a stream copy (no re-encode) — lossless and
# near-instant. Otherwise (e.g. mismatched resolutions), the parts are
# normalized to a common canvas and re-encoded, since a plain stream copy
# cannot join mismatched parts into one playable file.
#
# The re-encode path prefers the platform's hardware H.264 encoder
# (h264_videotoolbox on macOS; h264_nvenc/qsv/amf on Linux) but only after a
# quick self-test proves it works with the installed drivers; otherwise it
# falls back to software libx264 with a warning. Runs on macOS and Linux.
#
# Usage: ./concat.sh <recording_directory>

set -e

# Color output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

# Check if directory argument is provided
if [ $# -ne 1 ]; then
    echo -e "${RED}Error: Missing required argument${NC}"
    echo "Usage: $0 <recording_directory>"
    exit 1
fi

RECORDING_DIR="$1"

# Validate directory exists
if [ ! -d "$RECORDING_DIR" ]; then
    echo -e "${RED}Error: Directory does not exist: $RECORDING_DIR${NC}"
    exit 1
fi

# Check if ffmpeg / ffprobe are available
if ! command -v ffmpeg &> /dev/null; then
    echo -e "${RED}Error: ffmpeg is not installed or not in PATH${NC}"
    exit 1
fi
if ! command -v ffprobe &> /dev/null; then
    echo -e "${RED}Error: ffprobe is not installed or not in PATH${NC}"
    exit 1
fi

RECORDING_DIR_ABS=$(cd "$RECORDING_DIR" && pwd)
OUTPUT_FILE="$RECORDING_DIR/vod.mp4"
LOG_FILE="$RECORDING_DIR/ffmpeg.log"
CONCAT_LIST="$RECORDING_DIR/concat_list.txt"

# Find all vodN.mp4 files (numbered only, so the vod.mp4 output is excluded)
# and sort them in natural order (vod2 before vod10).
vod_files=($(ls "$RECORDING_DIR"/vod[0-9]*.mp4 2>/dev/null | sort -V))

if [ ${#vod_files[@]} -eq 0 ]; then
    echo -e "${RED}Error: No vodN.mp4 files found in $RECORDING_DIR${NC}"
    exit 1
fi

echo -e "${GREEN}Found ${#vod_files[@]} parts to concatenate:${NC}"
for f in "${vod_files[@]}"; do
    echo "  - $(basename "$f")"
done

# Probe every part once. In a single pass we determine:
#   - the largest width/height (target canvas for the re-encode path)
#   - whether every part has an audio stream
#   - whether all parts share an identical signature (codec + WxH + audio
#     codec), which decides whether the fast stream-copy path is safe.
target_w=0
target_h=0
all_have_audio=true
uniform=true
ref_signature=""

for f in "${vod_files[@]}"; do
    vinfo=$(ffprobe -v error -select_streams v:0 \
        -show_entries stream=codec_name,width,height \
        -of csv=p=0:s=x "$f" 2>/dev/null)
    IFS='x' read -r vcodec w h <<< "$vinfo"

    if [ -n "$w" ] && [ "$w" -gt "$target_w" ] 2>/dev/null; then
        target_w="$w"
    fi
    if [ -n "$h" ] && [ "$h" -gt "$target_h" ] 2>/dev/null; then
        target_h="$h"
    fi

    # Audio codec (empty if the part has no audio stream).
    acodec=$(ffprobe -v error -select_streams a:0 \
        -show_entries stream=codec_name \
        -of csv=p=0 "$f" 2>/dev/null)
    if [ -z "$acodec" ]; then
        all_have_audio=false
    fi

    # A part is copy-compatible with the others only if codec, resolution and
    # audio layout all match. Any difference forces the re-encode path.
    signature="${vcodec}:${w}x${h}:${acodec}"
    if [ -z "$ref_signature" ]; then
        ref_signature="$signature"
    elif [ "$signature" != "$ref_signature" ]; then
        uniform=false
    fi
done

if [ "$target_w" -le 0 ] || [ "$target_h" -le 0 ]; then
    echo -e "${RED}Error: Could not determine target resolution${NC}"
    exit 1
fi

# ---------------------------------------------------------------------------
# Hardware-encoder detection.
#
# Candidate H.264 encoders that accept ordinary software frames as input, in
# preference order per platform. (VAAPI is intentionally excluded: it needs
# GPU-surface uploads that don't fit our filter_complex.)
# ---------------------------------------------------------------------------
AVAILABLE_ENCODERS=$(ffmpeg -hide_banner -encoders 2>/dev/null)

case "$(uname -s)" in
    Darwin) HW_CANDIDATES=(h264_videotoolbox) ;;
    Linux)  HW_CANDIDATES=(h264_nvenc h264_qsv h264_amf) ;;
    *)      HW_CANDIDATES=() ;;
esac

# True if ffmpeg was built with the named encoder.
encoder_available() {
    echo "$AVAILABLE_ENCODERS" | grep -qw "$1"
}

# Pick a target bitrate for hardware encoders, scaled to the output height.
# (Software libx264 uses CRF instead and ignores this.)
pick_bitrate() {
    local h="$1"
    if   [ "$h" -ge 2160 ]; then echo "16M"
    elif [ "$h" -ge 1440 ]; then echo "10M"
    elif [ "$h" -ge 1080 ]; then echo "6M"
    elif [ "$h" -ge 720 ];  then echo "4M"
    else                         echo "2M"
    fi
}

# Populate the global ENCODER_ARGS array with the codec + quality options for
# the given encoder. Options differ per encoder, so each has its own recipe;
# VIDEO_BITRATE must be set before calling for the hardware encoders.
read_encoder_args() {
    case "$1" in
        libx264)
            ENCODER_ARGS=(-c:v libx264 -preset medium -crf 20 -pix_fmt yuv420p) ;;
        h264_videotoolbox)
            ENCODER_ARGS=(-c:v h264_videotoolbox -b:v "$VIDEO_BITRATE" -pix_fmt yuv420p) ;;
        h264_nvenc)
            ENCODER_ARGS=(-c:v h264_nvenc -b:v "$VIDEO_BITRATE" -pix_fmt yuv420p) ;;
        h264_qsv)
            ENCODER_ARGS=(-c:v h264_qsv -b:v "$VIDEO_BITRATE" -pix_fmt nv12) ;;
        h264_amf)
            ENCODER_ARGS=(-c:v h264_amf -b:v "$VIDEO_BITRATE" -pix_fmt yuv420p) ;;
        *)
            ENCODER_ARGS=() ;;
    esac
}

# Encode a 1s synthetic clip with the current ENCODER_ARGS to the null muxer.
# Succeeds only if the encoder is present AND actually works (drivers loaded,
# options accepted), which is exactly what we need to know before committing.
#
# The clip uses the real target resolution and bitrate, not a tiny stand-in:
# hardware encoders (e.g. h264_videotoolbox) pick an H.264 level from the
# resolution and refuse to create a session if the bitrate exceeds that
# level's ceiling — so a small test frame at a high -b:v would fail spuriously
# even when the real encode is fine. Errors are appended to the log.
test_encoder() {
    echo "--- self-test: ${ENCODER_ARGS[1]} (${target_w}x${target_h} @ ${VIDEO_BITRATE:-default}) ---" >> "$LOG_FILE"
    ffmpeg -hide_banner -loglevel error \
        -f lavfi -i "testsrc=size=${target_w}x${target_h}:rate=30:duration=1" \
        "${ENCODER_ARGS[@]}" \
        -f null - > /dev/null 2>> "$LOG_FILE"
}

# Choose the best working encoder, setting ENCODER_ARGS and SELECTED_ENCODER.
# Tries each hardware candidate (must pass its self-test), then software
# libx264. Returns non-zero only if nothing works.
select_encoder() {
    local cand
    for cand in "${HW_CANDIDATES[@]}"; do
        encoder_available "$cand" || continue
        read_encoder_args "$cand"
        if test_encoder; then
            SELECTED_ENCODER="$cand"
            echo -e "${GREEN}Using hardware encoder: ${cand} (bitrate ${VIDEO_BITRATE})${NC}"
            return 0
        fi
        echo -e "${YELLOW}⚠ ${cand} is present but failed its self-test (missing drivers?); skipping${NC}"
    done

    if encoder_available libx264; then
        read_encoder_args libx264
        if test_encoder; then
            SELECTED_ENCODER="libx264"
            if [ ${#HW_CANDIDATES[@]} -gt 0 ]; then
                echo -e "${YELLOW}⚠ No working hardware encoder; falling back to software libx264${NC}"
            else
                echo -e "${GREEN}Using software encoder: libx264${NC}"
            fi
            return 0
        fi
    fi

    echo -e "${RED}Error: no working H.264 encoder found (tried: ${HW_CANDIDATES[*]} libx264)${NC}"
    return 1
}

# ---------------------------------------------------------------------------
# Fast path: identical parts -> lossless stream copy via the concat demuxer.
# ---------------------------------------------------------------------------
copy_concat() {
    echo -e "${YELLOW}Stream-copying ${#vod_files[@]} matching parts (no re-encode)...${NC}"

    # The concat demuxer reads a list file; use absolute paths so resolution
    # never depends on the working directory.
    > "$CONCAT_LIST"
    for f in "${vod_files[@]}"; do
        local abs="$RECORDING_DIR_ABS/$(basename "$f")"
        # Escape single quotes for the concat list's 'file' directive.
        echo "file '${abs//\'/\'\\\'\'}'" >> "$CONCAT_LIST"
    done

    ffmpeg -hide_banner \
        -f concat -safe 0 -i "$CONCAT_LIST" \
        -c copy \
        -movflags +faststart \
        -y \
        "$OUTPUT_FILE" 2>&1 | tee -a "$LOG_FILE"
    local status=${PIPESTATUS[0]}

    rm -f "$CONCAT_LIST"
    return "$status"
}

# ---------------------------------------------------------------------------
# Re-encode path: normalize mismatched parts onto a common canvas, then join.
# ---------------------------------------------------------------------------
reencode_concat() {
    # H.264 requires even dimensions.
    target_w=$((target_w - target_w % 2))
    target_h=$((target_h - target_h % 2))

    # Choose (and verify) the video encoder before doing any real work.
    VIDEO_BITRATE=$(pick_bitrate "$target_h")
    select_encoder || return 1

    echo -e "${YELLOW}Re-encoding to a common ${target_w}x${target_h} canvas...${NC}"
    if [ "$all_have_audio" = false ]; then
        echo -e "${YELLOW}Note: at least one part has no audio; output will be video-only${NC}"
    fi

    # Build the ffmpeg command: one -i per part, plus a filter_complex that
    # scales+pads each part to the common canvas before concatenating.
    local inputs=()
    local filter=""
    local concat_labels=""
    local n=${#vod_files[@]}
    local i

    for i in "${!vod_files[@]}"; do
        # -reinit_filter 0: a part can switch resolution mid-file when the
        # stream reconnects at a different rendition. Rebuilding the graph on
        # that change (the default) would reset the concat filter's running
        # timeline offset, dropping every later part back to the start of the
        # output; scale absorbs the change on its own.
        inputs+=(-reinit_filter 0 -i "${vod_files[$i]}")

        # Scale preserving aspect ratio, pad to the canvas, normalize SAR/pixfmt.
        filter+="[${i}:v]scale=${target_w}:${target_h}:force_original_aspect_ratio=decrease,"
        filter+="pad=${target_w}:${target_h}:(ow-iw)/2:(oh-ih)/2:color=black,"
        filter+="setsar=1,format=yuv420p[v${i}];"

        if [ "$all_have_audio" = true ]; then
            # Normalize audio so every segment is concatenable.
            filter+="[${i}:a]aresample=48000,aformat=sample_fmts=fltp:channel_layouts=stereo[a${i}];"
            concat_labels+="[v${i}][a${i}]"
        else
            concat_labels+="[v${i}]"
        fi
    done

    local map_args audio_args
    if [ "$all_have_audio" = true ]; then
        filter+="${concat_labels}concat=n=${n}:v=1:a=1[outv][outa]"
        map_args=(-map "[outv]" -map "[outa]")
        audio_args=(-c:a aac -b:a 192k)
    else
        filter+="${concat_labels}concat=n=${n}:v=1:a=0[outv]"
        map_args=(-map "[outv]")
        audio_args=(-an)
    fi

    ffmpeg -hide_banner \
        "${inputs[@]}" \
        -filter_complex "$filter" \
        "${map_args[@]}" \
        "${ENCODER_ARGS[@]}" \
        "${audio_args[@]}" \
        -movflags +faststart \
        -y \
        "$OUTPUT_FILE" 2>&1 | tee -a "$LOG_FILE"
    return "${PIPESTATUS[0]}"
}

echo "=== Concat started: $(date) ===" >> "$LOG_FILE"

ffmpeg_status=1
if [ "$uniform" = true ]; then
    echo -e "${GREEN}All parts match (${ref_signature}); using fast stream copy${NC}"
    if copy_concat; then
        ffmpeg_status=0
    else
        echo -e "${YELLOW}⚠ Stream copy failed; falling back to re-encode${NC}"
        reencode_concat && ffmpeg_status=0
    fi
else
    echo -e "${GREEN}Parts differ; re-encoding to a common canvas${NC}"
    reencode_concat && ffmpeg_status=0
fi

echo "=== Concat finished: $(date) ===" >> "$LOG_FILE"

if [ "$ffmpeg_status" -eq 0 ] && [ -f "$OUTPUT_FILE" ]; then
    echo -e "${GREEN}✓ Successfully created: $OUTPUT_FILE${NC}"
    exit 0
else
    echo -e "${RED}✗ FFmpeg concatenation failed (see $LOG_FILE)${NC}"
    exit 1
fi
