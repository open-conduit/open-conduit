#!/bin/sh
# Builds the gateway image for a release (linux/amd64 and linux/arm64, the
# gateway and the Linux os at the release's version), pushes it, and prints
# its reference pinned by digest. The release (.releaserc.json) passes that
# to GoReleaser as GATEWAY_IMAGE, which pins it in the open-conduit binary
# (extensions/docker.GatewayImage). Needs docker buildx and a login to the
# registry. Usage: scripts/publish-gateway.sh <version>
set -eu
version=$1
repo=${GATEWAY_REPOSITORY:-ghcr.io/open-conduit/open-conduit-gateway}
commit=$(git rev-parse --short HEAD)
rm -rf dist/gateway
for arch in amd64 arm64; do
	for binary in open-conduit-gateway os; do
		CGO_ENABLED=0 GOOS=linux GOARCH=$arch go build -trimpath \
			-ldflags "-s -w -X main.version=$version -X main.commit=$commit" \
			-o "dist/gateway/linux-$arch/$binary" "./cmd/$binary"
	done
done
docker buildx build --platform linux/amd64,linux/arm64 -f extensions/docker/gateway/Dockerfile \
	-t "$repo:$version" --push --metadata-file dist/gateway-metadata.json dist/gateway >&2
digest=$(sed -n 's/.*"containerimage.digest": *"\(sha256:[0-9a-f]\{64\}\)".*/\1/p' dist/gateway-metadata.json | head -n 1)
test -n "$digest"
echo "$repo@$digest"
