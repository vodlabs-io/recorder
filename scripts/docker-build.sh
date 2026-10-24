#!/usr/bin/env bash
# Builds the recorder image for this machine and loads it as
# ghcr.io/vodlabs-io/recorder:latest, which compose.yaml runs.
#
#   scripts/docker-build.sh [--platform linux/arm64] [--tag name:tag]
set -euo pipefail
cd "$(dirname "$0")/.."

tag=ghcr.io/vodlabs-io/recorder:latest
args=()
while [ $# -gt 0 ]; do
  case "$1" in
    --tag) tag=$2; shift 2 ;;
    *) args+=("$1"); shift ;;
  esac
done

version=$(git describe --tags --always --dirty 2>/dev/null || echo docker)
commit=$(git rev-parse HEAD 2>/dev/null || echo unknown)

secret=()
if token=$(gh auth token 2>/dev/null); then
  export GITHUB_TOKEN=$token
  secret=(--secret id=github_token,env=GITHUB_TOKEN)
fi

docker buildx build --load \
  --build-arg VERSION="${version#v}" \
  --build-arg COMMIT="$commit" \
  ${secret[@]+"${secret[@]}"} \
  -t "$tag" ${args[@]+"${args[@]}"} .
