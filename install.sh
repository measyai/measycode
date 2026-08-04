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

if command -v curl >/dev/null 2>&1; then
  curl -sSL "$url" -o "$tmp" || { echo "error: curl download failed (was it killed by OOM?)" >&2; exit 1; }
elif command -v wget >/dev/null 2>&1; then
  wget -qO "$tmp" "$url" || { echo "error: wget download failed" >&2; exit 1; }
else
  echo "error: neither curl nor wget found" >&2
  exit 1
fi

if [ ! -s "$tmp" ]; then
  echo "error: downloaded file is empty. Release asset might be missing." >&2
  exit 1
fi

# Try to verify checksum using SHA256SUMS
hash_url="${base}/SHA256SUMS"
sums_tmp="$(mktemp)"
trap 'rm -f "$tmp" "$sums_tmp"' EXIT

if command -v curl >/dev/null 2>&1; then
  curl -sSL "$hash_url" -o "$sums_tmp" 2>/dev/null || true
elif command -v wget >/dev/null 2>&1; then
  wget -qO "$sums_tmp" "$hash_url" 2>/dev/null || true
fi

if [ -s "$sums_tmp" ]; then
  expected="$(grep "$file" "$sums_tmp" | awk '{print $1}' || true)"
  if [ -n "$expected" ]; then
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
    # Detect shell profile and add PATH automatically
    case "${SHELL##*/}" in
      fish)
        fish_add_path "$PREFIX" 2>/dev/null || true
        echo "  Added $PREFIX to your fish PATH."
        ;;
      zsh)
        profile="$HOME/.zshrc"
        echo "export PATH=\"$PREFIX:\$PATH\"" >> "$profile"
        echo "  Added $PREFIX to $profile"
        ;;
      *)
        profile="$HOME/.bashrc"
        echo "export PATH=\"$PREFIX:\$PATH\"" >> "$profile"
        echo "  Added $PREFIX to $profile"
        ;;
    esac
    echo
    echo "  To use measy now, run:"
    echo
    case "${SHELL##*/}" in
      fish) echo "    exec fish" ;;
      zsh)  echo "    source ~/.zshrc" ;;
      *)    echo "    source ~/.bashrc" ;;
    esac
    echo
    echo "  Or just open a new terminal."
    echo
    ;;
esac

echo "  cd into a project and run:"
echo "    measy"
echo
