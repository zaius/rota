#!/usr/bin/env bash
# Builds standalone release archives: the core binary for each platform with
# the built dashboard beside it in web/, which the binary serves by default.
#
# Usage: scripts/release-binaries.sh <version> [out-dir]
set -euo pipefail

version=${1:?usage: $0 <version> [out-dir]}
out=${2:-dist}
root=$(cd "$(dirname "$0")/.." && pwd)
mkdir -p "$out"
out=$(cd "$out" && pwd)

(cd "$root/dashboard" && pnpm install --frozen-lockfile && pnpm build)

for target in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64; do
  goos=${target%/*}
  goarch=${target#*/}
  name="rota_${version}_${goos}_${goarch}"
  ext=""
  [ "$goos" = windows ] && ext=".exe"

  stage="$out/$name"
  rm -rf "$stage"
  mkdir -p "$stage"
  (cd "$root/core" && CGO_ENABLED=0 GOOS=$goos GOARCH=$goarch go build -trimpath \
    -ldflags "-s -w -X github.com/alpkeskin/rota/core/internal/version.Version=$version" \
    -o "$stage/rota$ext" ./cmd/server)
  cp -R "$root/dashboard/dist" "$stage/web"
  cp "$root/LICENSE" "$root/.env.example" "$stage/"

  (
    cd "$out"
    if [ "$goos" = windows ]; then
      rm -f "$name.zip" && zip -qr "$name.zip" "$name"
    else
      tar -czf "$name.tar.gz" "$name"
    fi
  )
  rm -rf "$stage"
done

(cd "$out" && sha256sum rota_"${version}"_* > checksums.txt)
