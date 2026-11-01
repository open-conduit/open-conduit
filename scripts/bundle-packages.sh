#!/bin/sh
# Lays out the extension packages the release archive bundles, under
# extensions/<name>/ beside open-conduit and os (#243). The setup extension
# lives in github.com/open-conduit/extension-setup, pinned in go.mod with a
# `tool` directive, and is built from the module cache like any dependency.
#
#   scripts/bundle-packages.sh OUT [GOOS_GOARCH...]
#
# writes OUT/<goos>_<goarch>/extensions/setup for each platform (this host's
# when none is given): the package binary, its manifest and certification
# (printed by a build for this host, since they don't depend on the
# platform) and its playbooks. GoReleaser runs it before building and puts
# each platform's directory in that platform's archive; `make packages` runs
# it for this host.
set -eu

out=${1:?usage: bundle-packages.sh OUT [GOOS_GOARCH...]}
shift
go=${GO:-go}
module=github.com/open-conduit/extension-setup
pkg=$module/cmd/setup-extension
ldflags=${LDFLAGS:-}

targets="$*"
[ -n "$targets" ] || targets="$($go env GOOS)_$($go env GOARCH)"

rm -rf "$out"
mkdir -p "$out"
src=$($go list -m -f '{{.Dir}}' "$module")
host="$out/.host-setup-extension"
$go build -trimpath -o "$host" "$pkg"

for target in $targets; do
	dir="$out/$target/extensions/setup"
	mkdir -p "$dir"
	CGO_ENABLED=0 GOOS=${target%_*} GOARCH=${target#*_} $go build -trimpath -ldflags "$ldflags" -o "$dir/setup-extension" "$pkg"
	"$host" package-manifest >"$dir/open-conduit-extension.json"
	"$host" package-certification >"$dir/certification.json"
	cp -R "$src/playbooks" "$dir/playbooks"
	chmod -R u+w "$dir/playbooks"
	echo "bundled $dir"
done
rm -f "$host"
