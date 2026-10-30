# The Docker provider's gateway image: the gateway program and, in
# /toolbelt, the Linux os that every container mounts read-only (an image
# mount) and finds first on PATH. It holds nothing else, so the binaries
# are built first, outside it: `make gateway-image` for this machine's
# Docker, scripts/publish-gateway.sh for a release (both architectures).
# The build context is dist/gateway, with linux-<arch>/ for each one.
FROM scratch
ARG TARGETARCH
COPY linux-${TARGETARCH}/open-conduit-gateway /gateway
COPY linux-${TARGETARCH}/os /toolbelt/os
LABEL org.opencontainers.image.source="https://github.com/open-conduit/open-conduit"
LABEL org.opencontainers.image.description="Open Conduit's container gateway: the allowlisting proxy, the toolbelt forward and the Linux os"
ENTRYPOINT ["/gateway"]
