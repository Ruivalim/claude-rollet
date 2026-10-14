#!/usr/bin/env bash
# Builds release tarballs for every supported platform into dist/, plus a
# checksums.txt. Used by `make dist` and by the release workflow, so a local
# run produces exactly what gets published.
#
# Windows is not a target: the session log and the Codex token are guarded
# with flock(2), which Windows doesn't have.
set -euo pipefail

VERSION="${1:?usage: scripts/dist.sh <version>}"
OUT="${DIST_DIR:-dist}"
TARGETS="linux/amd64 linux/arm64 darwin/amd64 darwin/arm64"

rm -rf "$OUT"
mkdir -p "$OUT"
for target in $TARGETS; do
  os="${target%/*}"
  arch="${target#*/}"
  name="rollet_${VERSION#v}_${os}_${arch}"
  stage="$OUT/$name"
  mkdir -p "$stage"
  echo "▸ $os/$arch"
  CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" \
    go build -trimpath -ldflags "-s -w -X main.version=$VERSION" -o "$stage/rollet" ./cmd/rollet
  cp README.md LICENSE "$stage/"
  tar -C "$OUT" -czf "$OUT/$name.tar.gz" "$name"
  rm -rf "$stage"
done

cd "$OUT"
if command -v sha256sum >/dev/null; then
  sha256sum ./*.tar.gz | sed 's# \./# #' > checksums.txt
else
  shasum -a 256 ./*.tar.gz | sed 's# \./# #' > checksums.txt
fi
cat checksums.txt
