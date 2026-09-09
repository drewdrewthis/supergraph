#!/bin/sh
# install-remote.sh — curl|sh installer for supergraph release binaries.
#
#   curl -fsSL https://raw.githubusercontent.com/drewdrewthis/supergraph/main/scripts/install-remote.sh | sh
#
# Downloads the release tarball matching this host's OS/arch from the latest (or
# SUPERGRAPH_VERSION-pinned) GitHub release, verifies its SHA256, and installs the
# `supergraph` binary into SUPERGRAPH_INSTALL_DIR (default $HOME/.local/bin). No
# sudo; POSIX sh so it runs under `sh` even when the user's default shell differs.
set -eu

repo="drewdrewthis/supergraph"
install_dir="${SUPERGRAPH_INSTALL_DIR:-$HOME/.local/bin}"

log() { printf '%s\n' "$*" >&2; }
die() {
	log "install-remote: $*"
	exit 1
}

# --- detect platform ---
os_raw="$(uname -s)"
case "$os_raw" in
Linux) os="linux" ;;
Darwin) os="darwin" ;;
*) die "unsupported OS: $os_raw" ;;
esac

arch_raw="$(uname -m)"
case "$arch_raw" in
x86_64 | amd64) arch="amd64" ;;
aarch64 | arm64) arch="arm64" ;;
*) die "unsupported architecture: $arch_raw" ;;
esac

# --- resolve version ---
version="${SUPERGRAPH_VERSION:-}"
if [ -z "$version" ]; then
	log "resolving latest release for $repo..."
	version="$(curl -fsSL "https://api.github.com/repos/$repo/releases/latest" |
		grep '"tag_name"' | head -n1 | sed -E 's/.*"tag_name": *"([^"]+)".*/\1/')"
	[ -n "$version" ] || die "could not resolve latest release tag"
fi

tarball="supergraph_${version}_${os}_${arch}.tar.gz"
base_url="https://github.com/$repo/releases/download/$version"

work_dir="$(mktemp -d)"
trap 'rm -rf "$work_dir"' EXIT

log "downloading $tarball ($version)..."
curl -fsSL -o "$work_dir/$tarball" "$base_url/$tarball" ||
	die "download failed: $base_url/$tarball"
curl -fsSL -o "$work_dir/SHA256SUMS" "$base_url/SHA256SUMS" ||
	die "download failed: $base_url/SHA256SUMS"

# --- verify checksum ---
expected="$(grep " $tarball\$" "$work_dir/SHA256SUMS" | awk '{print $1}')"
[ -n "$expected" ] || die "no checksum entry for $tarball in SHA256SUMS"

if command -v sha256sum >/dev/null 2>&1; then
	actual="$(sha256sum "$work_dir/$tarball" | awk '{print $1}')"
else
	actual="$(shasum -a 256 "$work_dir/$tarball" | awk '{print $1}')"
fi

[ "$expected" = "$actual" ] || die "checksum mismatch for $tarball: expected $expected, got $actual"

# --- install ---
mkdir -p "$install_dir"
tar -C "$work_dir" -xzf "$work_dir/$tarball" supergraph
mv "$work_dir/supergraph" "$install_dir/supergraph"
chmod +x "$install_dir/supergraph"

log "installed supergraph $version to $install_dir/supergraph"

case ":$PATH:" in
*":$install_dir:"*) ;;
*) log "note: $install_dir is not on your PATH — add it, e.g. export PATH=\"$install_dir:\$PATH\"" ;;
esac

# --- install agent tooling (justfile + plugin recipes) ---
# Version-locked to the binary: extracted from the same verified tarball,
# overwriting any previous install's copies.
data_dir="${SUPERGRAPH_DATA_DIR:-$HOME/.local/share/supergraph}"
mkdir -p "$data_dir"
tar -C "$work_dir" -xzf "$work_dir/$tarball" justfile plugins
rm -rf "$data_dir/plugins"
mv "$work_dir/justfile" "$data_dir/justfile"
mv "$work_dir/plugins" "$data_dir/plugins"
log "installed agent tooling to $data_dir"

# --- register global justfile import ---
just_config_dir="${XDG_CONFIG_HOME:-$HOME/.config}/just"
global_justfile="$just_config_dir/justfile"
import_line="import \"$data_dir/justfile\""

if [ -L "$global_justfile" ]; then
	log "warning: $global_justfile is a symlink — a symlinked global justfile breaks mod resolution."
	log "warning: replace it with a real file, then re-run this installer to register supergraph."
elif [ -e "$global_justfile" ]; then
	if ! grep -qF "$import_line" "$global_justfile"; then
		printf '%s\n' "$import_line" >>"$global_justfile"
	fi
else
	mkdir -p "$just_config_dir"
	printf '%s\n' "$import_line" >"$global_justfile"
fi

# --- just presence check ---
if command -v just >/dev/null 2>&1; then
	log ""
	log "alias sg='just -g'"
	log "usage: sg --list"
	log "       sg github pr-status owner/repo 38"
else
	case "$os" in
	darwin) log "note: 'just' not found — install it: brew install just" ;;
	linux) log "note: 'just' not found — install it: apt install just (or your distro's equivalent)" ;;
	esac
fi
