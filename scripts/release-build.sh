#!/usr/bin/env bash
# release-build.sh — builds cmd/supergraph for every release target and packages
# each as a tarball plus a SHA256SUMS manifest, for `make release-build` and
# .github/workflows/release.yml. Pure-Go build (modernc.org/sqlite, no cgo), so
# every target cross-compiles from a single host with CGO_ENABLED=0.
#
# Usage: scripts/release-build.sh <version> [outdir=dist]
set -euo pipefail

version="${1:?usage: scripts/release-build.sh <version> [outdir=dist]}"
outdir="${2:-dist}"

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

# sha256_file prints "<hex>  <name>" (sha256sum-compatible) for one file, computed
# with sha256sum where available and shasum -a 256 on macOS otherwise.
sha256_file() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1"
	else
		shasum -a 256 "$1"
	fi
}

targets="linux/amd64 linux/arm64 darwin/arm64"

rm -rf "$outdir"
mkdir -p "$outdir"

sums_file="$outdir/SHA256SUMS"
: > "$sums_file"

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

for target in $targets; do
	os="${target%/*}"
	arch="${target#*/}"

	stage="$work/$os-$arch"
	mkdir -p "$stage"

	bin="$stage/supergraph"
	echo "building $os/$arch -> $bin"
	CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build \
		-ldflags "-X main.version=$version" -trimpath \
		-o "$bin" ./cmd/supergraph

	cp LICENSE README.md "$stage/"

	tarball="supergraph_${version}_${os}_${arch}.tar.gz"
	tar -C "$stage" -czf "$outdir/$tarball" supergraph LICENSE README.md

	(cd "$outdir" && sha256_file "$tarball") >> "$sums_file"
done

echo "wrote $outdir/SHA256SUMS:"
cat "$sums_file"
