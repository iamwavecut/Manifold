#!/bin/sh
set -eu

root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
source_dir="$root/skills/manifold/cmd/manifold"
output="$root/skills/manifold/bin"
version=${VERSION:-dev}

build() {
  os=$1
  arch=$2
  target="$output/$os-$arch/manifold"
  mkdir -p "$(dirname "$target")"
  CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build \
    -buildvcs=false -trimpath -ldflags="-s -w -X main.version=$version" \
    -o "$target" "$source_dir"
}

build darwin amd64
build darwin arm64
build linux amd64
build linux arm64
