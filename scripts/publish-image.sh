#!/bin/sh
# Builds the daemon's image for a release (linux/amd64 and linux/arm64) from
# the Linux release archives GoReleaser wrote into dist/release, and pushes
# it as <version>, <major>.<minor> and latest (image/Dockerfile). The release
# (.releaserc.json) runs it after GoReleaser has published the archives.
# Needs docker buildx and a login to the registry.
#
#   scripts/publish-image.sh <version>
#
# `scripts/publish-image.sh --load <version>` builds this machine's
# Docker's architecture only and loads it into the local Docker as
# open-conduit:dev, pushing nothing (`make image`).
set -eu
load=
if [ "${1:-}" = --load ]; then
	load=1
	shift
fi
version=${1:?usage: publish-image.sh [--load] <version>}
repo=${IMAGE_REPOSITORY:-ghcr.io/open-conduit/open-conduit}
release=${RELEASE_DIR:-dist/release}
context=dist/image

if [ -n "$load" ]; then
	arches=$(docker version --format '{{.Server.Arch}}')
	case $arches in
	amd64 | arm64) ;;
	*) echo "publish-image: Docker's architecture $arches has no release archive" >&2; exit 1 ;;
	esac
else
	arches="amd64 arm64"
fi

rm -rf "$context"
mkdir -p "$context"
cp image/entrypoint.sh "$context/"
platforms=
for arch in $arches; do
	archive="$release/open-conduit_${version}_linux_$arch.tar.gz"
	test -f "$archive" || { echo "publish-image: $archive is missing; run GoReleaser first" >&2; exit 1; }
	mkdir -p "$context/linux-$arch"
	tar -xzf "$archive" -C "$context/linux-$arch" open-conduit os extensions LICENSE
	platforms="${platforms:+$platforms,}linux/$arch"
done

if [ -n "$load" ]; then
	docker build --build-arg "TARGETARCH=$arches" -f image/Dockerfile -t open-conduit:dev "$context"
	exit 0
fi

minor=$(echo "$version" | cut -d. -f1-2)
docker buildx build --platform "$platforms" -f image/Dockerfile \
	-t "$repo:$version" -t "$repo:$minor" -t "$repo:latest" --push "$context"
