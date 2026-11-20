#!/bin/sh
# The image's entry point. With no arguments, or only flags (bar --version
# and --help), it runs `open-conduit serve --no-updates` with them: the
# image, not the daemon, is what changes version. Anything else is an
# open-conduit command (`docker run … ghcr.io/open-conduit/open-conduit
# config check`), which sees the mounted config too.
#
# The daemon reads its config and secrets.json only from regular,
# owner-only (0600) files its own user owns, which a Kubernetes ConfigMap
# or Secret mount, a Docker config or a bind mount from the host usually
# isn't. So a config.json or secrets.json mounted in
# $OPEN_CONDUIT_CONFIG_SOURCE (default /etc/open-conduit) is copied over
# the daemon's own at each start: the mount is the source of truth, and an
# edit made inside the container lasts until the next start. Each copy is
# marked (<file>.from-mount), and a marked copy whose mounted file has gone
# is removed, with the copy the daemon keeps of its last version
# (<file>.last-good), so a value taken out of the mount doesn't live on in
# the volume. A file nobody mounted, with no marker, is left alone.
set -eu

source_dir=${OPEN_CONDUIT_CONFIG_SOURCE:-/etc/open-conduit}
config=${OPEN_CONDUIT_CONFIG:-${XDG_CONFIG_HOME:-$HOME/.config}/open-conduit/config.json}
config_dir=$(dirname "$config")

install_private() {
	from=$1 to=$2
	if [ ! -f "$from" ]; then
		if [ -f "$to.from-mount" ]; then
			rm -f "$to" "$to.last-good" "$to.from-mount"
			echo "open-conduit-entrypoint: removed $to, since $from is no longer mounted" >&2
		fi
		return 0
	fi
	# OPEN_CONDUIT_CONFIG naming the mount itself leaves it to the daemon.
	[ ! "$from" -ef "$to" ] || return 0
	mkdir -p "$config_dir"
	chmod 0700 "$config_dir"
	tmp="$to.tmp.$$"
	(umask 077 && cat "$from" >"$tmp")
	mv -f "$tmp" "$to"
	(umask 077 && echo "$from" >"$to.from-mount")
	echo "open-conduit-entrypoint: installed $from as $to" >&2
}

case "${1:-}" in
--version | --help | -h)
	exec /usr/local/bin/open-conduit "$@"
	;;
esac

install_private "$source_dir/config.json" "$config"
install_private "$source_dir/secrets.json" "$config_dir/secrets.json"
case "${1:-}" in
"" | -*) exec /usr/local/bin/open-conduit serve --no-updates "$@" ;;
*) exec /usr/local/bin/open-conduit "$@" ;;
esac
