#!/bin/bash

# Regenerate cross-platform desktop app icons from a single high-res source.
#
# Sources: desktop/logo-src.png     full-colour app icon (square, >= 1024x1024)
#          desktop/logo-simple.png  flat silhouette w/ alpha, for the tray
# Outputs:
#   desktop/icon.iconset/*.png   macOS — Apple-named set; Electrobun -> .icns
#   desktop/icon.png             Linux — single 512x512 PNG
#   desktop/icon.ico             Windows — multi-res .ico (256/48/32/16)
#   desktop/tray.png             system tray template image (32x32)
#
# These paths are wired into electrobun.config.ts (build.mac.icons /
# build.linux.icon / build.win.icon / build.copy) and the Tray in
# desktop/src/bun/index.ts. Re-run this after editing a source image.

set -e

# Color output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

DESK="${REPO_ROOT}/desktop"
SRC="${DESK}/logo-src.png"
SET="${DESK}/icon.iconset"
TRAY_SRC="${DESK}/logo-simple.png"

# --- preflight -------------------------------------------------------------

for f in "$SRC" "$TRAY_SRC"; do
	if [ ! -f "$f" ]; then
		echo -e "${RED}Error: source image not found: $f${NC}"
		exit 1
	fi
done

# sips + iconutil ship with macOS; this script is macOS-only by design since
# iconutil (the .iconset -> .icns compiler) has no portable equivalent.
for tool in sips iconutil; do
	if ! command -v "$tool" &> /dev/null; then
		echo -e "${RED}Error: '$tool' not found (macOS-only script).${NC}"
		exit 1
	fi
done

# ImageMagick builds the multi-resolution Windows .ico. Prefer `magick`,
# fall back to the legacy `convert` name.
if command -v magick &> /dev/null; then
	MAGICK="magick"
elif command -v convert &> /dev/null; then
	MAGICK="convert"
else
	echo -e "${RED}Error: ImageMagick not found (need 'magick' or 'convert').${NC}"
	echo -e "${YELLOW}Install with: brew install imagemagick${NC}"
	exit 1
fi

# Source must be square so downscales aren't distorted.
W=$(sips -g pixelWidth "$SRC" | awk '/pixelWidth/{print $2}')
H=$(sips -g pixelHeight "$SRC" | awk '/pixelHeight/{print $2}')
if [ "$W" != "$H" ]; then
	echo -e "${YELLOW}Warning: source is ${W}x${H}, not square — icons may look squished.${NC}"
fi
echo -e "${GREEN}Source: $SRC (${W}x${H})${NC}"

# --- macOS iconset ---------------------------------------------------------

# Every size is resampled straight from the full-res source (no chained
# downscaling) for the sharpest result at each resolution.
mkdir -p "$SET"
gen() { sips -s format png -z "$2" "$2" "$SRC" --out "$SET/$1" > /dev/null; }

echo -e "${YELLOW}Generating macOS iconset...${NC}"
gen icon_16x16.png        16
gen icon_16x16@2x.png     32
gen icon_32x32.png        32
gen icon_32x32@2x.png     64
gen icon_128x128.png      128
gen icon_128x128@2x.png   256
gen icon_256x256.png      256
gen icon_256x256@2x.png   512
gen icon_512x512.png      512
gen icon_512x512@2x.png   1024

# Validate the iconset compiles to a real .icns (what Electrobun does at
# build time). iconutil rejects any non-standard filename in the directory.
iconutil -c icns "$SET" --output /tmp/gen-icons-validate.icns
rm -f /tmp/gen-icons-validate.icns
echo -e "${GREEN}✓ iconset valid (compiles to .icns)${NC}"

# --- Linux PNG -------------------------------------------------------------

echo -e "${YELLOW}Generating Linux icon...${NC}"
sips -s format png -z 512 512 "$SRC" --out "${DESK}/icon.png" > /dev/null
echo -e "${GREEN}✓ desktop/icon.png (512x512)${NC}"

# --- Windows ICO -----------------------------------------------------------

echo -e "${YELLOW}Generating Windows icon...${NC}"
"$MAGICK" "$SRC" -background none -define icon:auto-resize=256,48,32,16 "${DESK}/icon.ico"
echo -e "${GREEN}✓ desktop/icon.ico (256/48/32/16)${NC}"

# --- System tray template image -------------------------------------------

# Flat silhouette sized for the menu bar / tray. 32px is the @2x of the Tray's
# default 16pt size, so it stays crisp on Retina. macOS template mode (Tray
# template:true) ignores colour and tints the alpha channel to match the menu
# bar, so the silhouette adapts to light/dark automatically.
echo -e "${YELLOW}Generating tray icon...${NC}"
sips -s format png -z 32 32 "$TRAY_SRC" --out "${DESK}/tray.png" > /dev/null
echo -e "${GREEN}✓ desktop/tray.png (32x32)${NC}"

echo -e "${GREEN}Done. All icons regenerated.${NC}"
