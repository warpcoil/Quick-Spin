#!/usr/bin/env bash
# Builds QuickSpin for every supported platform.
#
#   bash scripts/build.sh [version] [output-dir]
#
# version defaults to the current git tag (v2.3.0 -> 2.3.0) or "dev-<commit>".
# QuickSpin is pure Go (no cgo), so every target cross-compiles from any OS.
set -euo pipefail

cd "$(dirname "$0")/.."

version="${1:-}"
if [ -z "$version" ]; then
	if tag="$(git describe --tags --exact-match 2>/dev/null)"; then
		version="${tag#v}"
	else
		version="dev-$(git rev-parse --short HEAD 2>/dev/null || echo local)"
	fi
fi
out="${2:-dist}"

# os arch output-name
targets=(
	"windows amd64 quickspin.exe"
	"darwin  arm64 quickspin-mac-arm64"
	"linux   amd64 quickspin-linux-x86_64"
	"linux   arm64 quickspin-linux-arm64"
)

rm -rf "$out"
mkdir -p "$out"
for t in "${targets[@]}"; do
	read -r goos goarch name <<<"$t"
	echo "building $name (QuickSpin $version, $goos/$goarch)"
	CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" \
		go build -trimpath -ldflags "-s -w -X main.appVersion=${version}" -o "$out/$name" .
done

cp README.md "$out/"
(
	cd "$out"
	if command -v sha256sum >/dev/null; then
		sha256sum quickspin* >SHA256SUMS.txt
	else
		shasum -a 256 quickspin* >SHA256SUMS.txt
	fi
)
echo "done: $out"
ls -l "$out"
