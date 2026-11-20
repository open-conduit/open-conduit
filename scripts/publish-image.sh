#!/bin/sh
# Builds the daemon's image (image/Dockerfile) from the Linux release
# archives GoReleaser wrote into dist/release.
#
#   scripts/publish-image.sh <version>          linux/amd64 and linux/arm64, pushed as
#                                               <version>, <major>.<minor> and latest
#   scripts/publish-image.sh --check <version>  both platforms, built and kept nowhere
#   scripts/publish-image.sh --load <version>   Docker's own platform, loaded as open-conduit:dev
#
# The release (.releaserc.json) pushes after GoReleaser has published the
# archives; `make image` (run by `make release-snapshot`, so by CI and by the
# release before anything is tagged) checks and loads. The Dockerfile runs
# apt and npm, so building the other platform needs emulation: when the
# builder can't run one of them, this registers QEMU with the host's kernel
# (tonistiigi/binfmt, as docker/setup-qemu-action does), which needs a
# privileged container. A multi-platform build needs a docker-container
# builder, so it uses one of its own (open-conduit-image). Pushing needs a
# login to the registry.
set -eu
mode=push
case "${1:-}" in
--check | --load)
	mode=${1#--}
	shift
	;;
esac
version=${1:?usage: publish-image.sh [--check | --load] <version>}
repo=${IMAGE_REPOSITORY:-ghcr.io/open-conduit/open-conduit}
release=${RELEASE_DIR:-dist/release}
context=dist/image
builder=open-conduit-image
binfmt=tonistiigi/binfmt:qemu-v9.2.2

if [ "$mode" = load ]; then
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

if [ "$mode" = load ]; then
	docker build --build-arg "TARGETARCH=$arches" -f image/Dockerfile -t open-conduit:dev "$context"
	exit 0
fi

# missing prints each platform the builder can't run.
missing() {
	can=$(docker buildx inspect --bootstrap "$builder" | sed -n 's/^Platforms: *//p')
	for arch in $arches; do
		case ", $can," in
		*", linux/$arch,"* | *", linux/$arch*,"*) ;;
		*) echo "linux/$arch" ;;
		esac
	done
}
docker buildx inspect "$builder" >/dev/null 2>&1 || docker buildx create --name "$builder" --driver docker-container >/dev/null
if [ -n "$(missing)" ]; then
	echo "publish-image: registering QEMU for $(missing | tr '\n' ' ')" >&2
	docker run --privileged --rm "$binfmt" --install all >&2
	# A builder reads the kernel's emulators when it starts.
	docker buildx rm "$builder" >/dev/null
	docker buildx create --name "$builder" --driver docker-container >/dev/null
	left=$(missing)
	test -z "$left" || { echo "publish-image: the builder still can't build $left" >&2; exit 1; }
fi

if [ "$mode" = check ]; then
	docker buildx build --builder "$builder" --platform "$platforms" -f image/Dockerfile "$context"
	exit 0
fi
minor=$(echo "$version" | cut -d. -f1-2)
docker buildx build --builder "$builder" --platform "$platforms" -f image/Dockerfile \
	-t "$repo:$version" -t "$repo:$minor" -t "$repo:latest" --push "$context"
