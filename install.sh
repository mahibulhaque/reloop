#!/bin/sh
# reloop installer for Linux. Detects arch, downloads the matching
# release binary, and installs it to /usr/local/bin (or $RELOOP_PREFIX/bin
# if set). Defaults to the latest GitHub release; pass RELOOP_VERSION to
# install a specific version.

set -eu

RELOOP_VERSION="${RELOOP_VERSION:-}"
RELOOP_PREFIX="${RELOOP_PREFIX:-/usr/local}"
INSTALL_DIR="${RELOOP_PREFIX}/bin"
TMPDIR="$(mktemp -d)"
trap 'rm -rf "$TMPDIR"' EXIT

uname_os="$(uname -s | tr '[:upper:]' '[:lower:]')"
case "$uname_os" in
    linux) os="linux" ;;
    darwin)
        echo "install.sh is for Linux; on macOS use:"
        echo "  brew tap mahibulhaque/reloop https://github.com/mahibulhaque/reloop"
        echo "  brew install --cask reloop"
        exit 1
        ;;
    *)
        echo "unsupported OS: $uname_os" >&2
        exit 1
        ;;
esac

uname_arch="$(uname -m)"
case "$uname_arch" in
    x86_64|amd64) arch="amd64" ;;
    aarch64|arm64) arch="arm64" ;;
    *)
        echo "unsupported arch: $uname_arch" >&2
        exit 1
        ;;
esac

if [ -z "$RELOOP_VERSION" ] || [ "$RELOOP_VERSION" = "latest" ]; then
    echo "Resolving latest reloop release..."
    latest_json="$(curl -fsSL https://api.github.com/repos/mahibulhaque/reloop/releases/latest)" || {
        echo "failed to resolve latest reloop release" >&2
        exit 1
    }
    RELOOP_VERSION="$(printf '%s\n' "$latest_json" |
        sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' |
        head -n 1)"
    if [ -z "$RELOOP_VERSION" ]; then
        echo "failed to parse latest reloop release tag" >&2
        exit 1
    fi
fi

case "$RELOOP_VERSION" in
    v*) ;;
    *) RELOOP_VERSION="v$RELOOP_VERSION" ;;
esac

asset="reloop-${RELOOP_VERSION}-${os}-${arch}.tar.gz"
url="https://github.com/mahibulhaque/reloop/releases/download/${RELOOP_VERSION}/${asset}"
sums_url="https://github.com/mahibulhaque/reloop/releases/download/${RELOOP_VERSION}/sha256sums.txt"

echo "Downloading $asset..."
curl -fsSL "$url" -o "$TMPDIR/$asset"
curl -fsSL "$sums_url" -o "$TMPDIR/sha256sums.txt"

echo "Verifying checksum..."
(cd "$TMPDIR" && grep " $asset\$" sha256sums.txt | sha256sum -c -)

tar -xzf "$TMPDIR/$asset" -C "$TMPDIR"

if [ ! -w "$INSTALL_DIR" ]; then
    echo "Installing to $INSTALL_DIR (requires sudo)..."
    sudo install -m 0755 "$TMPDIR/reloop" "$INSTALL_DIR/reloop"
else
    install -m 0755 "$TMPDIR/reloop" "$INSTALL_DIR/reloop"
fi

echo "Installed reloop $RELOOP_VERSION to $INSTALL_DIR/reloop"
"$INSTALL_DIR/reloop" --version || true
