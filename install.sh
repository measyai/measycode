#!/usr/bin/env bash
#
# Install the `measy` command on Linux and macOS.
#
# Downloads the pre-built binary from GitHub Releases and puts it somewhere
# already on PATH, so `measy` starts the agent in whatever folder the
# terminal is sitting in — which is the whole point of the short name.
#
#   curl -fsSL https://github.com/measyai/measycode/releases/latest/download/install.sh | bash
#   ./install.sh                              # ~/.local/bin, no sudo
#   PREFIX=/usr/local/bin ./install.sh
#   VERSION=1.0.0 ./install.sh
#
# ~/.local/bin by default rather than /usr/local/bin: a developer tool that
# needs sudo to install is one people install once and then distrust.

set -euo pipefail

PREFIX="${PREFIX:-$HOME/.local/bin}"
REPO="${REPO:-measyai/measycode}"
VERSION="${VERSION:-}"
TARGET="$PREFIX/measy"

os="$(uname -s | tr '[:upper:]' '[:lower:]')"
case "$os" in
  linux) os=linux ;;
  darwin) os=darwin ;;
  *) echo "error: unsupported OS: $os" >&2; exit 1 ;;
esac

arch="$(uname -m)"
case "$arch" in
  x86_64|amd64) arch=amd64 ;;
  aarch64|arm64) arch=arm64 ;;
  *) echo "error: unsupported architecture: $arch" >&2; exit 1 ;;
esac

file="measy-${os}-${arch}"
if [ -n "$VERSION" ]; then
  tag="$VERSION"
  case "$tag" in v*) ;; *) tag="v$tag" ;; esac
  base="https://github.com/${REPO}/releases/download/${tag}"
else
  base="https://github.com/${REPO}/releases/latest/download"
fi
url="${base}/${file}"

echo "Downloading measy..."
echo "  $url"
mkdir -p "$PREFIX"

tmp="$(mktemp)"
trap 'rm -f "$tmp"' EXIT
curl -fsSL "$url" -o "$tmp"

hash_url="${base}/${file}.sha256"
if hash="$(curl -fsSL "$hash_url" 2>/dev/null)"; then
  expected="${hash%% *}"
  if command -v sha256sum >/dev/null 2>&1; then
    actual="$(sha256sum "$tmp" | awk '{print $1}')"
  else
    actual="$(shasum -a 256 "$tmp" | awk '{print $1}')"
  fi
  if [ "$actual" != "$expected" ]; then
    echo "error: checksum mismatch" >&2
    exit 1
  fi
fi

install -m 755 "$tmp" "$TARGET"

echo
echo "  measy installed"
echo "  $TARGET"
echo

# Only mention PATH when it is actually a problem — an unconditional "add
# this to your shell profile" is noise for the majority who do not need it.
case ":$PATH:" in
  *":$PREFIX:"*) ;;
  *)
    echo "  $PREFIX is not on your PATH. Add this to your shell profile:"
    echo
    case "${SHELL##*/}" in
      fish) echo "    fish_add_path $PREFIX" ;;
      zsh)  echo "    echo 'export PATH=\"$PREFIX:\$PATH\"' >> ~/.zshrc" ;;
      *)    echo "    echo 'export PATH=\"$PREFIX:\$PATH\"' >> ~/.bashrc" ;;
    esac
    echo
    ;;
esac

echo "  cd into a project and run:"
echo "    measy"
echo
