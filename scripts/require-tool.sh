#!/usr/bin/env bash
# Fails fast, with a message saying what to do, when a make target's tool is
# missing or too old, instead of letting the target fail halfway through.
#
# Usage: scripts/require-tool.sh TOOL HINT [MIN_VERSION]
#
# TOOL is a command on PATH or a path to one. HINT says how to get it. With
# MIN_VERSION (for example 22.18), the first dotted number `TOOL --version`
# (or, when that fails, `TOOL version`, as for go) prints must be at least
# that.
set -euo pipefail

tool=$1
hint=$2
want=${3:-}

if ! command -v "$tool" >/dev/null 2>&1; then
	echo "$tool not found: $hint" >&2
	exit 1
fi
[ -n "$want" ] || exit 0

out=$("$tool" --version 2>&1) || out=$("$tool" version 2>&1) || true
have=$(printf '%s\n' "$out" | sed -n 's/^[^0-9]*\([0-9][0-9]*\(\.[0-9][0-9]*\)*\).*/\1/p' | head -n 1)
if [ -z "$have" ]; then
	echo "$tool: can't read its version: $hint" >&2
	exit 1
fi
if ! awk -v have="$have" -v want="$want" 'BEGIN {
	split(have, h, "."); split(want, w, ".")
	for (i = 1; i <= 3; i++) {
		if (h[i] + 0 > w[i] + 0) exit 0
		if (h[i] + 0 < w[i] + 0) exit 1
	}
	exit 0
}'; then
	echo "$tool $have is older than $want: $hint" >&2
	exit 1
fi
