#!/usr/bin/env bash
# Renders the terminal Shots (vod-status.gif/.mp4) with VHS against a Demo
# daemon on its own ports, so neither a real daemon nor an interactive Demo
# daemon is touched. Recording stats tick, so this daemon is not frozen.
#
#   tools/showcase/terminal/render-terminal.sh [OUT_DIR]
set -euo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
repo="$(cd "$here/../../.." && pwd)"
out="${1:-$repo/tools/showcase/out/terminal}"
command -v vhs >/dev/null || { echo "needs vhs: brew install vhs" >&2; exit 1; }

tmp="$(mktemp -d)"
trap 'kill "${daemon:-}" 2>/dev/null || true; rm -rf "$tmp"' EXIT

(cd "$repo" && go build -o "$tmp/demodaemon" ./tools/showcase/demodaemon && go build -o "$tmp/vod" ./cmd/cli)
"$tmp/demodaemon" --dataset "$repo/tools/showcase/datasets/default" \
  --port 14422 --ws-port 0 --http-port 0 2>"$tmp/daemon.log" &
daemon=$!
until grep -q "Demo daemon serving" "$tmp/daemon.log"; do
  kill -0 "$daemon" 2>/dev/null || { cat "$tmp/daemon.log" >&2; exit 1; }
  sleep 0.2
done

# auto_start off: vod must never start a real daemon if this one is gone.
printf '[daemon]\nhost = "127.0.0.1"\nport = 14422\nauto_start = false\n' >"$tmp/vod.toml"

mkdir -p "$out"
cd "$out"
VOD_BIN="$tmp/vod" VOD_CONFIG="$tmp/vod.toml" vhs "$here/vod-status.tape"
echo "wrote $out"
