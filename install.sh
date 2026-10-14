#!/bin/sh
# Installs the latest claude-rollet release binary.
#
#   curl -fsSL https://raw.githubusercontent.com/ruivalim/claude-rollet/main/install.sh | sh
#
# Knobs (env vars):
#   ROLLET_VERSION      tag to install, e.g. v0.2.0 (default: latest release)
#   ROLLET_INSTALL_DIR  where the binary goes (default: ~/.local/bin)
#   ROLLET_DOWNLOAD_URL base URL holding the release assets (default: GitHub)
set -eu

REPO="ruivalim/claude-rollet"
INSTALL_DIR="${ROLLET_INSTALL_DIR:-$HOME/.local/bin}"

say() { printf '%s\n' "$*" >&2; }
die() { say "error: $*"; exit 1; }
need() { command -v "$1" >/dev/null 2>&1 || die "$1 is required"; }

need curl
need tar

case "$(uname -s)" in
  Linux) os=linux ;;
  Darwin) os=darwin ;;
  *) die "unsupported OS $(uname -s); build from source with Go instead" ;;
esac
case "$(uname -m)" in
  x86_64 | amd64) arch=amd64 ;;
  aarch64 | arm64) arch=arm64 ;;
  *) die "unsupported architecture $(uname -m)" ;;
esac

version="${ROLLET_VERSION:-}"
if [ -z "$version" ]; then
  version=$(curl -fsSL "https://api.github.com/repos/$REPO/releases/latest" |
    sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' | head -n 1)
  [ -n "$version" ] || die "couldn't find the latest release of $REPO"
fi
base="${ROLLET_DOWNLOAD_URL:-https://github.com/$REPO/releases/download/$version}"
asset="rollet_${version#v}_${os}_${arch}.tar.gz"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT INT TERM

say "Downloading rollet $version ($os/$arch)"
curl -fsSL "$base/$asset" -o "$tmp/$asset" || die "download failed: $base/$asset"
curl -fsSL "$base/checksums.txt" -o "$tmp/checksums.txt" || die "download failed: $base/checksums.txt"

expected=$(grep " $asset\$" "$tmp/checksums.txt" | cut -d' ' -f1)
[ -n "$expected" ] || die "$asset is not listed in checksums.txt"
if command -v sha256sum >/dev/null 2>&1; then
  actual=$(sha256sum "$tmp/$asset" | cut -d' ' -f1)
else
  actual=$(shasum -a 256 "$tmp/$asset" | cut -d' ' -f1)
fi
[ "$expected" = "$actual" ] || die "checksum mismatch for $asset, refusing to install"

tar -xzf "$tmp/$asset" -C "$tmp"
mkdir -p "$INSTALL_DIR"
mv "$tmp/rollet_${version#v}_${os}_${arch}/rollet" "$INSTALL_DIR/rollet"
chmod +x "$INSTALL_DIR/rollet"

say "Installed $("$INSTALL_DIR/rollet" version) to $INSTALL_DIR/rollet"
case ":$PATH:" in
  *":$INSTALL_DIR:"*) ;;
  *) say "Note: $INSTALL_DIR is not in your PATH; add it to your shell rc." ;;
esac
say "Next: rollet init"
