#!/usr/bin/env bash
# Builds release archives into ./release: zips for Windows (with the tray), tarballs
# elsewhere, and checksums.txt.
#
#   scripts/package.sh v0.1.0
set -euo pipefail

version=${1:?usage: package.sh vX.Y.Z}
commit=$(git rev-parse --short HEAD)
date=$(date -u +%Y-%m-%dT%H:%M:%SZ)
pkg=github.com/trustypangolin/mcp-broker/internal/version
ldflags="-s -w -X $pkg.Version=$version -X $pkg.Commit=$commit -X $pkg.Date=$date"
out=release
export CGO_ENABLED=0

rm -rf "$out"
mkdir -p "$out"
for target in windows/amd64 windows/arm64 linux/amd64 linux/arm64 darwin/amd64 darwin/arm64; do
  os=${target%/*} arch=${target#*/}
  name="mcp-broker_${version#v}_${os}_${arch}"
  dir="$out/$name"
  mkdir -p "$dir/docs" "$dir/examples"
  ext=""
  [ "$os" = windows ] && ext=.exe
  GOOS=$os GOARCH=$arch go build -trimpath -ldflags "$ldflags" -o "$dir/mcp-broker$ext" ./cmd/mcp-broker
  if [ "$os" = windows ]; then
    GOOS=$os GOARCH=$arch go build -trimpath -ldflags "$ldflags -H windowsgui" -o "$dir/mcp-broker-tray.exe" ./cmd/mcp-broker-tray
    mkdir -p "$dir/scripts" && cp scripts/firewall.ps1 "$dir/scripts/"
  fi
  cp README.md "$dir/"
  cp docs/*.md "$dir/docs/"
  cp examples/broker.yaml "$dir/examples/"
  if [ "$os" = windows ]; then
    (cd "$out" && "${PYTHON:-python3}" -m zipfile -c "$name.zip" "$name")
  else
    tar -C "$out" -czf "$out/$name.tar.gz" "$name"
  fi
  rm -rf "$dir"
done
(cd "$out" && sha256sum *.zip *.tar.gz > checksums.txt)
ls -l "$out"
