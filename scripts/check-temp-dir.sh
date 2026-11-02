#!/usr/bin/env bash
# Runs a command with TMPDIR set to a new, empty directory, and fails if the
# command leaves anything in it: tests must not leave files behind in the
# operator's temp directory, where a binary built once per test binary
# and never removed added up to tens of gigabytes (#445).
#
# Usage: scripts/check-temp-dir.sh command [arg...]
set -euo pipefail

if [ $# -eq 0 ]; then
	echo "usage: $0 command [arg...]" >&2
	exit 2
fi

# Under /tmp rather than $TMPDIR: a short path, since tests put Unix sockets
# (104 bytes at most on macOS) in their temp directories.
tmp=$(mktemp -d /tmp/oc-tmpcheck.XXXXXX)
trap 'chmod -R u+w "$tmp" 2>/dev/null; rm -rf "$tmp"' EXIT

status=0
TMPDIR=$tmp "$@" || status=$?

left=$(ls -A "$tmp")
if [ -n "$left" ]; then
	echo "--- left in the temp directory by: $*" >&2
	(cd "$tmp" && du -sh -- $left) >&2 || true
	echo "Tests must remove what they create in the temp directory (t.TempDir, or a cleanup after the run)." >&2
	if [ "$status" -eq 0 ]; then
		status=1
	fi
fi
exit $status
