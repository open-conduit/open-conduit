#!/bin/sh
# Installs the latest open-conduit release:
#   curl -fsSL https://raw.githubusercontent.com/open-conduit/open-conduit/main/install.sh | sh
# Set OPEN_CONDUIT_VERSION=v0.1.0 for a particular one. The archive's
# checksum is verified against the release's checksums.txt before anything is
# installed. With cosign on PATH, checksums.txt is first verified against its
# signature (checksums.txt.sigstore.json), which only the release workflow on
# main can make. A release from before signing began (below first_signed)
# is installed with a warning; from first_signed on, a missing or failed
# bundle download is refused, so leaving the bundle out of a release doesn't
# skip the check.
#
# open-conduit and its `os` toolbelt go together into INSTALL_DIR
# (~/.local/lib/open-conduit), and only open-conduit is linked into BIN_DIR
# (~/.local/bin): `os` is a generic name, so it is kept off PATH, and the
# daemon finds it beside the real binary. The bundled extension packages
# (the archive's extensions/ directory, such as setup) replace the ones in
# INSTALL_DIR/extensions; packages installed in the config directory are not
# touched. Open Supervisor's open-supervisor
# binary and its config and state directories are not touched.
set -eu

repo=open-conduit/open-conduit
# The first release whose checksums.txt is signed.
first_signed=1.15.0
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

# signed_release says whether version $1 (x.y.z) is first_signed or later.
signed_release() {
  set -- "${1%%-*}" "$first_signed"
  a1=${1%%.*} r=${1#*.}; a2=${r%%.*} a3=${r#*.}
  b1=${2%%.*} r=${2#*.}; b2=${r%%.*} b3=${r#*.}
  [ "$a1" -ne "$b1" ] && { [ "$a1" -gt "$b1" ]; return; }
  [ "$a2" -ne "$b2" ] && { [ "$a2" -gt "$b2" ]; return; }
  [ "$a3" -ge "$b3" ]
}

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
echo "downloading open-conduit $version for $os/$arch"
curl -fsSL -o "$tmp/$archive" "$base/$archive"
curl -fsSL -o "$tmp/checksums.txt" "$base/checksums.txt"
if command -v cosign >/dev/null 2>&1; then
  if curl -fsSL -o "$tmp/checksums.txt.sigstore.json" "$base/checksums.txt.sigstore.json" 2>/dev/null; then
    cosign verify-blob --bundle "$tmp/checksums.txt.sigstore.json" \
      --certificate-identity "https://github.com/$repo/.github/workflows/release.yml@refs/heads/main" \
      --certificate-oidc-issuer https://token.actions.githubusercontent.com \
      "$tmp/checksums.txt" >/dev/null 2>&1 || { echo "checksums.txt for $version is not signed by the $repo release workflow" >&2; exit 1; }
    echo "verified the signature on checksums.txt"
  else
    if signed_release "$number"; then
      echo "couldn't download the signature for $version (checksums.txt.sigstore.json); every release from v$first_signed on has one" >&2
      exit 1
    fi
    echo "warning: $version is from before releases were signed (v$first_signed), so there is no signature to check" >&2
  fi
else
  echo "cosign isn't installed, so the release's signature isn't checked (see SECURITY.md)"
fi
expected=$(grep " $archive\$" "$tmp/checksums.txt" | cut -d' ' -f1)
[ -n "$expected" ] || { echo "no checksum listed for $archive" >&2; exit 1; }
if command -v sha256sum >/dev/null 2>&1; then actual=$(sha256sum "$tmp/$archive" | cut -d' ' -f1); else actual=$(shasum -a 256 "$tmp/$archive" | cut -d' ' -f1); fi
[ "$expected" = "$actual" ] || { echo "checksum mismatch for $archive" >&2; exit 1; }

tar -xzf "$tmp/$archive" -C "$tmp"
mkdir -p "$install_dir" "$bin_dir"
install -m 0755 "$tmp/open-conduit" "$install_dir/open-conduit"
install -m 0755 "$tmp/os" "$install_dir/os"
rm -rf "$install_dir/extensions"
if [ -d "$tmp/extensions" ]; then cp -R "$tmp/extensions" "$install_dir/extensions"; fi
ln -sf "$install_dir/open-conduit" "$bin_dir/open-conduit"
echo "installed $install_dir/open-conduit and $install_dir/os, linked $bin_dir/open-conduit"
case ":$PATH:" in *":$bin_dir:"*) ;; *) echo "add $bin_dir to your PATH" ;; esac
echo "next: open-conduit init"
