# The daemon's image, ghcr.io/open-conduit/open-conduit: open-conduit, the
# os beside it and the bundled extensions/ from the release archive, with
# git, Node (for TypeScript packages) and a pinned Claude Code. It runs
# `open-conduit serve --no-updates` as the unprivileged conduit user (uid
# 10001), whose home, /home/conduit, is the one volume: the state, the
# config directory and Claude Code's own config directory. A mounted
# /etc/open-conduit/config.json (and secrets.json) is installed into it at
# each start (image/entrypoint.sh). The guide is
# site/src/content/docs/guides/headless.md.
#
# The build context holds linux-<arch>/, each the extracted release archive
# for that architecture: scripts/publish-image.sh for a release (both
# architectures), `make image` for this machine's Docker from
# `make release-snapshot`'s output.
FROM node:24-bookworm-slim
ARG TARGETARCH
# Claude Code, pinned so an image is reproducible. A release moves it by
# changing this line; a deployment that wants another version builds FROM
# this image and installs it.
ARG CLAUDE_CODE_VERSION=2.1.311
RUN apt-get update \
	&& apt-get install -y --no-install-recommends ca-certificates git openssh-client tini \
	&& rm -rf /var/lib/apt/lists/* \
	&& npm install -g --no-audit --no-fund "@anthropic-ai/claude-code@${CLAUDE_CODE_VERSION}" \
	&& npm cache clean --force \
	&& useradd --uid 10001 --user-group --create-home --home-dir /home/conduit --shell /bin/sh conduit \
	&& mkdir -p /etc/open-conduit
COPY linux-${TARGETARCH}/ /opt/open-conduit/
COPY entrypoint.sh /usr/local/bin/open-conduit-entrypoint
RUN ln -s /opt/open-conduit/open-conduit /usr/local/bin/open-conduit \
	&& chmod 0755 /usr/local/bin/open-conduit-entrypoint \
	&& /opt/open-conduit/open-conduit --version
ENV HOME=/home/conduit
USER 10001:10001
WORKDIR /home/conduit
VOLUME ["/home/conduit"]
LABEL org.opencontainers.image.source="https://github.com/open-conduit/open-conduit"
LABEL org.opencontainers.image.description="Open Conduit's daemon, with git, Node and Claude Code, for a headless deployment"
# tini is PID 1: it passes SIGTERM on to the daemon, which stops cleanly
# and releases its leases, and reaps the processes runs leave behind.
ENTRYPOINT ["/usr/bin/tini", "--", "/usr/local/bin/open-conduit-entrypoint"]
