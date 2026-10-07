#!/usr/bin/env bash
# Authenticode-signs Windows binaries in place as VODLABS LLC through Azure
# Artifact Signing, using jsign so it runs on the Linux and Windows runners
# alike. Called by goreleaser for vod.exe/vlrecd.exe and by the desktop build's
# postBuild hook (scripts/sign-win-bundle.ts).
#
# Needs an `az login` session holding the Artifact Signing Certificate Profile
# Signer role, plus:
#   JSIGN_JAR               path to the jsign jar
#   WINDOWS_SIGN_ENDPOINT   account endpoint host, e.g. eus.codesigning.azure.net
#   WINDOWS_SIGN_ACCOUNT    Artifact Signing account name
#   WINDOWS_SIGN_PROFILE    certificate profile name
# Without WINDOWS_SIGN_ACCOUNT it signs nothing, and the release workflow
# warns that it's publishing unsigned Windows builds.
#
# Usage: sign-windows.sh FILE...
set -euo pipefail

if [ -z "${WINDOWS_SIGN_ACCOUNT:-}" ]; then
	echo "sign-windows: WINDOWS_SIGN_ACCOUNT unset; leaving $# file(s) unsigned" >&2
	exit 0
fi
[ "$#" -gt 0 ] || exit 0

: "${JSIGN_JAR:?}" "${WINDOWS_SIGN_ENDPOINT:?}" "${WINDOWS_SIGN_PROFILE:?}"

# Fetched per call, so a long build never signs with an expired token.
WINDOWS_SIGN_TOKEN="$(az account get-access-token --resource https://codesigning.azure.net --query accessToken -o tsv | tr -d '\r')"
export WINDOWS_SIGN_TOKEN

# The certificates live three days, so every signature is timestamped.
java -jar "$JSIGN_JAR" \
	--storetype TRUSTEDSIGNING \
	--keystore "${WINDOWS_SIGN_ENDPOINT#https://}" \
	--storepass env:WINDOWS_SIGN_TOKEN \
	--alias "$WINDOWS_SIGN_ACCOUNT/$WINDOWS_SIGN_PROFILE" \
	--alg SHA-256 \
	--tsaurl http://timestamp.acs.microsoft.com \
	--tsmode RFC3161 \
	"$@"
