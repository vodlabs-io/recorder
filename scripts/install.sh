#!/bin/sh
# Installs the VodLabs Recorder CLI (vod) on macOS and Linux:
#
#   curl -fsSL https://vodlabs.io/install.sh | sh
#
# Options, set as environment variables on the `sh` side of the pipe:
#   VOD_VERSION      release to install, e.g. 0.1.2 (default: latest)
#   VOD_INSTALL_DIR  where to put vod (default: ~/.local/bin)
#
# vod fetches the matching recorder daemon (vlrecd) itself when it first needs
# one, and `vod update` replaces vod in place, so the install directory has to
# stay writable by you: no sudo.

set -eu

RELEASES="https://github.com/vodlabs-io/assets/releases"

say() { printf '%s\n' "$*"; }
die() { printf 'install.sh: %s\n' "$*" >&2; exit 1; }

fetch() {
	if command -v curl >/dev/null 2>&1; then
		curl -fsSL --proto '=https' --tlsv1.2 -o "$2" "$1"
	elif command -v wget >/dev/null 2>&1; then
		wget -q -O "$2" "$1"
	else
		die "needs curl or wget"
	fi
}

sha256() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1" | cut -d' ' -f1
	else
		shasum -a 256 "$1" | cut -d' ' -f1
	fi
}

main() {
	case "$(uname -s)" in
		Darwin) os=darwin ;;
		Linux) os=linux ;;
		*) die "unsupported OS $(uname -s); on Windows, run install.ps1 from PowerShell" ;;
	esac
	case "$(uname -m)" in
		x86_64 | amd64) arch=amd64 ;;
		arm64 | aarch64) arch=arm64 ;;
		*) die "unsupported CPU $(uname -m)" ;;
	esac
	# A shell running under Rosetta reports x86_64 on Apple Silicon.
	if [ "$os" = darwin ] && [ "$arch" = amd64 ] &&
		[ "$(sysctl -n sysctl.proc_translated 2>/dev/null || true)" = 1 ]; then
		arch=arm64
	fi

	if [ -n "${VOD_VERSION:-}" ]; then
		base="$RELEASES/download/v${VOD_VERSION#v}"
	else
		base="$RELEASES/latest/download"
	fi
	archive="cli_${os}_${arch}.tar.gz"
	dir="${VOD_INSTALL_DIR:-$HOME/.local/bin}"

	tmp="$(mktemp -d)"
	trap 'rm -rf "$tmp"' EXIT

	say "Downloading $archive..."
	fetch "$base/$archive" "$tmp/$archive" || die "download failed: $base/$archive"
	fetch "$base/checksums.txt" "$tmp/checksums.txt" || die "download failed: $base/checksums.txt"

	want="$(awk -v f="$archive" '$2 == f { print $1 }' "$tmp/checksums.txt")"
	[ -n "$want" ] || die "$archive is not listed in checksums.txt"
	[ "$(sha256 "$tmp/$archive")" = "$want" ] || die "checksum mismatch for $archive"

	tar -xzf "$tmp/$archive" -C "$tmp"
	mkdir -p "$dir" || die "cannot create $dir; set VOD_INSTALL_DIR to a directory you own"
	chmod 755 "$tmp/vod"
	mv -f "$tmp/vod" "$dir/vod" || die "cannot write to $dir; set VOD_INSTALL_DIR to a directory you own"

	say "Installed $("$dir/vod" --version) to $dir/vod"

	case ":$PATH:" in
		*":$dir:"*) ;;
		*)
			case "${SHELL:-}" in
				*/zsh) rc="$HOME/.zshrc" ;;
				*/bash) [ "$os" = darwin ] && rc="$HOME/.bash_profile" || rc="$HOME/.bashrc" ;;
				*) rc="" ;;
			esac
			say ""
			say "$dir is not on your PATH. To add it, run:"
			say ""
			if [ -n "$rc" ]; then
				say "  echo 'export PATH=\"$dir:\$PATH\"' >> $rc && . $rc"
			else
				say "  export PATH=\"$dir:\$PATH\"   (and add that line to your shell's startup file)"
			fi
			;;
	esac

	say ""
	say "Get started: vod --help"
}

main "$@"
