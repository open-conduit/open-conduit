#!/bin/sh
# Installs the latest open-conduit release:
#   curl -fsSL https://raw.githubusercontent.com/open-conduit/open-conduit/main/install.sh | sh
# Set OPEN_CONDUIT_VERSION=v0.1.0 for a particular one. The archive's
# checksum is verified against the release's checksums.txt before anything is
# installed.
#
# open-conduit and its `os` toolbelt go together into INSTALL_DIR
# (~/.local/lib/open-conduit), and only open-conduit is linked into BIN_DIR
# (~/.local/bin): `os` is a generic name, so it is kept off PATH, and the
# daemon finds it beside the real binary. Open Supervisor's open-supervisor
# binary and its config and state directories are not touched.
set -eu

repo=open-conduit/open-conduit
bin_dir=${BIN_DIR:-"$HOME/.local/bin"}
install_dir=${INSTALL_DIR:-"$HOME/.local/lib/open-conduit"}

os=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$os" in darwin|linux) ;; *) echo "unsupported OS: $os (macOS and Linux only)" >&2; exit 1 ;; esac
arch=$(uname -m)
case "$arch" in x86_64|amd64) arch=amd64 ;; arm64|aarch64) arch=arm64 ;; *) echo "unsupported architecture: $arch" >&2; exit 1 ;; esac

version=${OPEN_CONDUIT_VERSION:-}
if [ -z "$version" ]; then
  version=$(curl -fsSL "https://api.github.com/repos/$repo/releases/latest" | sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' | head -1)
fi
[ -n "$version" ] || { echo "couldn't find the latest release" >&2; exit 1; }
number=${version#v}
archive="open-conduit_${number}_${os}_${arch}.tar.gz"
base="https://github.com/$repo/releases/download/$version"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
echo "downloading open-conduit $version for $os/$arch"
curl -fsSL -o "$tmp/$archive" "$base/$archive"
curl -fsSL -o "$tmp/checksums.txt" "$base/checksums.txt"
expected=$(grep " $archive\$" "$tmp/checksums.txt" | cut -d' ' -f1)
[ -n "$expected" ] || { echo "no checksum listed for $archive" >&2; exit 1; }
if command -v sha256sum >/dev/null 2>&1; then actual=$(sha256sum "$tmp/$archive" | cut -d' ' -f1); else actual=$(shasum -a 256 "$tmp/$archive" | cut -d' ' -f1); fi
[ "$expected" = "$actual" ] || { echo "checksum mismatch for $archive" >&2; exit 1; }

tar -xzf "$tmp/$archive" -C "$tmp"
mkdir -p "$install_dir" "$bin_dir"
install -m 0755 "$tmp/open-conduit" "$install_dir/open-conduit"
install -m 0755 "$tmp/os" "$install_dir/os"
ln -sf "$install_dir/open-conduit" "$bin_dir/open-conduit"
echo "installed $install_dir/open-conduit and $install_dir/os, linked $bin_dir/open-conduit"
case ":$PATH:" in *":$bin_dir:"*) ;; *) echo "add $bin_dir to your PATH" ;; esac
echo "next: open-conduit init"
