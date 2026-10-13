#!/bin/sh
# Build the release artifacts for one version: the client tarballs
# deploy/install-client.sh downloads (linux and darwin, amd64 and arm64), the
# raw client binaries the signer's dist directory serves, the linux server
# binaries, and checksums.txt covering every asset. This is the canonical
# recipe — .github/workflows/release.yml runs it on every release, so a hand
# build and a release build can never drift.
#
# Usage: scripts/build-release.sh <version> [output-dir]
#
#   <version>     release version, with or without the leading v (a tag name
#                 works as-is)
#   output-dir    where dist/v<version>/ is created (default: ./dist)
#
# Requires only go, tar, mktemp, and sha256sum.

set -eu

usage() {
	echo "usage: $0 <version> [output-dir]" >&2
	exit 2
}

[ $# -ge 1 ] && [ $# -le 2 ] || usage
version=${1#v}
[ -n "$version" ] || usage
out=${2:-dist}
dir=$out/v$version
mkdir -p "$dir"

# 1. Client binary for every platform install-client.sh advertises: the raw
#    binary (kept, the signer serves the linux ones from its dist directory)
#    and the checksum-verified tarball the installer downloads.
for os in linux darwin; do
	for arch in amd64 arm64; do
		CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build -trimpath \
			-o "$dir/git-remote-sign-$os-$arch" ./cmd/git-remote-sign
		staged=$(mktemp -d)
		cp "$dir/git-remote-sign-$os-$arch" "$staged/git-remote-sign"
		tar -czf "$dir/git-remote-sign_${version}_${os}_${arch}.tar.gz" \
			-C "$staged" git-remote-sign
		rm -rf "$staged"
	done
done

# 2. Server binary for the platforms a signer runs on.
for arch in amd64 arm64; do
	CGO_ENABLED=0 GOOS=linux GOARCH=$arch go build -trimpath \
		-o "$dir/git-signer-server-linux-$arch" ./cmd/git-signer-server
done

# 3. checksums.txt in sha256sum format, one line per asset. The asset list is
#    spelled out (never a glob) so a stray file in the directory can never
#    leak into the manifest, and the filenames are bare (no ./ prefix) so
#    install-client.sh's `awk '$2 == artifact'` lookup matches exactly.
assets=
for os in linux darwin; do
	for arch in amd64 arm64; do
		assets="$assets git-remote-sign_${version}_${os}_${arch}.tar.gz"
	done
done
for os in linux darwin; do
	for arch in amd64 arm64; do
		assets="$assets git-remote-sign-$os-$arch"
	done
done
for arch in amd64 arm64; do
	assets="$assets git-signer-server-linux-$arch"
done
# shellcheck disable=SC2086  # word splitting is the point: $assets is a list
(cd "$dir" && sha256sum $assets >checksums.txt)

echo "==> built release $version in $dir" >&2
