#!/bin/sh
# Lays out the extension packages the release archive bundles, under
# extensions/<name>/ beside open-conduit and os (#243). The setup extension
# lives in github.com/open-conduit/extension-setup, pinned in go.mod with a
# `tool` directive, and is built from the module cache like any dependency.
# The Codex executor (extensions/codex, #360) is served by open-conduit
# itself (`open-conduit extension-run codex`, #428): its package is a
# manifest and certification fixture, with no executable of its own, so a
# binding can name executor codex on a fresh install. `bundled` below names the
# first-party packages served that way.
#
#   scripts/bundle-packages.sh OUT [GOOS_GOARCH...]
#
# writes OUT/<goos>_<goarch>/extensions/{setup,codex,posthog,human} for each platform (this host's
# when none is given): setup's binary, each package's manifest and certification
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
# go list -m reports no Dir until the module is in the cache.
$go mod download "$module"
src=$($go list -m -f '{{.Dir}}' "$module")
[ -n "$src" ] || { echo "bundle-packages: $module is not in the module cache" >&2; exit 1; }
host="$out/.host-setup-extension"
$go build -trimpath -o "$host" "$pkg"
bundled="codex posthog human observe"
hostbin="$out/.host-open-conduit"
$go build -trimpath -o "$hostbin" ./cmd/open-conduit

for target in $targets; do
	dir="$out/$target/extensions/setup"
	mkdir -p "$dir"
	CGO_ENABLED=0 GOOS=${target%_*} GOARCH=${target#*_} $go build -trimpath -ldflags "$ldflags" -o "$dir/setup-extension" "$pkg"
	"$host" package-manifest >"$dir/open-conduit-extension.json"
	"$host" package-certification >"$dir/certification.json"
	cp -R "$src/playbooks" "$dir/playbooks"
	chmod -R u+w "$dir/playbooks"
	echo "bundled $dir"
	for name in $bundled; do
		dir="$out/$target/extensions/$name"
		mkdir -p "$dir"
		"$hostbin" extension-run "$name" package-manifest >"$dir/open-conduit-extension.json"
		"$hostbin" extension-run "$name" package-certification >"$dir/certification.json"
		echo "bundled $dir"
	done
done
rm -f "$host" "$hostbin"
