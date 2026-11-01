#!/usr/bin/env bash
# Runs the integration package's tests under -race in SHARDS processes at
# once. The package's tests run one after another inside a process (most
# start a real daemon and use t.Setenv, so they can't take t.Parallel), which
# made it about 8 minutes of make check on its own. The test binary is built
# once; each shard runs every SHARDS-th top-level test of the sorted list, so
# the shards share the slow daemon tests between them.
#
# Usage: scripts/test-integration.sh [shards]
# GO and TEST_TIMEOUT come from the environment (the Makefile sets them).
set -euo pipefail

shards=${1:-4}
go=${GO:-go}
timeout=${TEST_TIMEOUT:-20m}

root=$(cd "$(dirname "$0")/.." && pwd)
work=$(mktemp -d "${TMPDIR:-/tmp}/oc-integration-XXXXXX")
trap 'rm -rf "$work"' EXIT

cd "$root/integration"
"$go" test -race -c -o "$work/integration.test" .

tests=$("$work/integration.test" -test.list '.*' | grep '^Test' | sort)
if [ -z "$tests" ]; then
	echo "no integration tests to run" >&2
	exit 1
fi

pids=""
for ((i = 0; i < shards; i++)); do
	names=$(printf '%s\n' "$tests" | awk -v n="$shards" -v i="$i" '(NR - 1) % n == i' | paste -sd '|' -)
	if [ -z "$names" ]; then
		continue
	fi
	"$work/integration.test" -test.run "^($names)\$" -test.timeout "$timeout" -test.count 1 >"$work/shard-$i.log" 2>&1 &
	pids="$pids $!:$i"
done

status=0
for entry in $pids; do
	pid=${entry%%:*}
	i=${entry#*:}
	if wait "$pid"; then
		echo "--- integration shard $((i + 1))/$shards"
		tail -n 1 "$work/shard-$i.log"
	else
		status=1
		echo "--- integration shard $((i + 1))/$shards FAILED"
		cat "$work/shard-$i.log"
	fi
done
exit $status
