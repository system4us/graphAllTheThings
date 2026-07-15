#!/bin/sh
# gatt installer for Linux and macOS.
#   curl -fsSL https://raw.githubusercontent.com/system4us/graphAllTheThings/main/scripts/install.sh | sh
# Env overrides: GATT_VERSION (tag, default latest), GATT_INSTALL_DIR (default /usr/local/bin or ~/.local/bin)
set -eu

REPO="system4us/graphAllTheThings"

os=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$os" in
	linux|darwin) ;;
	*) echo "error: unsupported OS: $os (use install.ps1 on Windows)" >&2; exit 1 ;;
esac

arch=$(uname -m)
case "$arch" in
	x86_64|amd64) arch=amd64 ;;
	aarch64|arm64) arch=arm64 ;;
	*) echo "error: unsupported architecture: $arch" >&2; exit 1 ;;
esac

version="${GATT_VERSION:-}"
if [ -z "$version" ]; then
	version=$(curl -fsSL "https://api.github.com/repos/$REPO/releases/latest" \
		| grep '"tag_name"' | head -1 | cut -d'"' -f4)
	[ -n "$version" ] || { echo "error: could not resolve latest release" >&2; exit 1; }
fi

asset="gatt-$os-$arch"
url="https://github.com/$REPO/releases/download/$version/$asset"

if [ -n "${GATT_INSTALL_DIR:-}" ]; then
	dir="$GATT_INSTALL_DIR"
elif [ -w /usr/local/bin ]; then
	dir=/usr/local/bin
else
	dir="$HOME/.local/bin"
fi
mkdir -p "$dir"

tmp=$(mktemp)
trap 'rm -f "$tmp"' EXIT
echo "downloading gatt $version ($os/$arch)..."
curl -fsSL -o "$tmp" "$url"

expected=$(curl -fsSL "https://github.com/$REPO/releases/download/$version/checksums.txt" \
	| grep " $asset\$" | cut -d' ' -f1 || true)
if [ -n "$expected" ]; then
	actual=$(sha256sum "$tmp" 2>/dev/null | cut -d' ' -f1 || shasum -a 256 "$tmp" | cut -d' ' -f1)
	[ "$expected" = "$actual" ] || { echo "error: checksum mismatch" >&2; exit 1; }
fi

install -m 0755 "$tmp" "$dir/gatt"
echo "installed $dir/gatt"

case ":$PATH:" in
	*":$dir:"*) ;;
	*) echo "note: $dir is not in PATH; add it to your shell profile" ;;
esac

"$dir/gatt" version
