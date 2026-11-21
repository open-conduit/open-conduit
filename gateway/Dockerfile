# The egress gateway's image, which any container provider may run (its
# `serve` flags are a contract: architecture/sandboxes.md): the gateway
# program and, in /toolbelt, the Linux os that a Docker container mounts
# read-only (an image mount) and finds first on PATH. It holds nothing else, so the binaries
# are built first, outside it: `make gateway-image` for this machine's
# Docker, scripts/publish-gateway.sh for a release (both architectures).
# The build context is dist/gateway, with linux-<arch>/ for each one.
FROM scratch
ARG TARGETARCH
COPY linux-${TARGETARCH}/open-conduit-gateway /gateway
COPY linux-${TARGETARCH}/os /toolbelt/os
LABEL org.opencontainers.image.source="https://github.com/open-conduit/open-conduit"
LABEL org.opencontainers.image.description="Open Conduit's egress gateway: the allowlisting proxy, the toolbelt forward and the Linux os"
ENTRYPOINT ["/gateway"]
