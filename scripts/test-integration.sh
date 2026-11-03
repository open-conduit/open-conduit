#!/usr/bin/env bash
# Runs the integration package's tests under -race, split into SHARDS
# shards. The package's tests run one after another inside a process (most
# start a real daemon and use t.Setenv, so they can't take t.Parallel), which
# made it about 8 minutes of make check on its own. The test binary is built
# once, and each shard runs its share of the top-level tests in a process of
# its own. The performance budget tests (TestInTransactionRPCBudget*, #428)
# time calls, so they are in no shard: they run on their own once the shards
# are done, never beside them, and their numbers are printed. A run that
# takes only some shards runs them only if it takes shard 1, so CI runs them
# once.
#
# Tests are shared out by expected duration: longest first, each to the
# shard with the least work so far. The slow tests' rough durations are
# listed below; every other test counts as one second. The list only steers
# the balance, so a stale or missing entry makes a shard slower, never skips
# a test: every test lands in exactly one shard.
#
# With no SHARD arguments every shard runs, all at once. With some, only
# those run (1-based), so CI can spread the shards over several runners:
# scripts/test-integration.sh 12 1 2 3 on one, 4 5 6 on the next, and so on.
#
# Usage: scripts/test-integration.sh [shards [shard...]]
# GO and TEST_TIMEOUT come from the environment (the Makefile sets them).
set -euo pipefail

shards=${1:-4}
shift || true
go=${GO:-go}
timeout=${TEST_TIMEOUT:-20m}

if [ $# -eq 0 ]; then
	set -- $(seq 1 "$shards")
fi
for s in "$@"; do
	if ! [ "$s" -ge 1 ] 2>/dev/null || [ "$s" -gt "$shards" ]; then
		echo "shard $s is not between 1 and $shards" >&2
		exit 2
	fi
done

# Seconds each slow test takes on a laptop under -race, measured with
# -test.v. Refresh it when the shards drift apart (each shard prints its
# time).
slow='
TestDaemonExtensionPackages 44
TestExternalToolbeltSharedSuite 23
TestDaemonAllWorkflows 21
TestTutorialFirstExtension 20
TestCertificationFirstPartyPackages 18
TestDaemonClaudeCodeContinuityKill 17
TestDaemonJobParity 15
TestDaemonClaudeCodeExecution 13
TestDaemonGitWorkspaces 13
TestDaemonGitHubHeldIssueAndTools 12
TestSDKStoreEventsSkipOtherScopes 10
TestDaemonWorkflowC 9
TestCertificationInvalidPackage 9
TestExtensionCheckCommand 9
TestDaemonArtifactCleanup 9
TestDaemonCodexExecution 9
TestDev 9
TestExtensionNew 8
TestExtensiontestPackage 8
TestExtensiontestPackageFakeClaude 8
TestCertificationNegativeControls 7
TestDaemonTrackersAndPostHog 6
TestDaemonFreshInstallGitHubIssue 6
TestExternalSchemaEvolution 6
TestDaemonGitHubReconcileToProjection 6
TestSlackPushReconcileOverlapOneWork 5
TestDaemonOperatorCLI 5
TestDaemonGracefulRestart 5
TestInstrumentedRuntimeSharedCases 5
TestRuntimeSpecificationOnReopenedSQLite 5
TestDaemonWorkspaceDiskSafety 3
'

root=$(cd "$(dirname "$0")/.." && pwd)
work=$(mktemp -d "${TMPDIR:-/tmp}/oc-integration-XXXXXX")
trap 'rm -rf "$work"' EXIT

cd "$root/integration"
"$go" test -race -c -o "$work/integration.test" .

budget='^TestInTransactionRPCBudget'
tests=$("$work/integration.test" -test.list '.*' | grep '^Test' | grep -v "$budget" | sort)
if [ -z "$tests" ]; then
	echo "no integration tests to run" >&2
	exit 1
fi

# "shard test" for every test: weight each, sort heaviest first (ties by
# name, so every runner computes the same plan), then give each to the
# lightest shard (the lowest-numbered on a tie).
printf '%s\n' "$slow" >"$work/slow"
printf '%s\n' "$tests" >"$work/tests"
awk 'NR == FNR { if (NF == 2) w[$1] = $2; next } { print ($1 in w ? w[$1] : 1), $1 }' "$work/slow" "$work/tests" |
	sort -k1,1nr -k2,2 |
	awk -v n="$shards" '{
		m = 1
		for (i = 2; i <= n; i++) if (load[i] < load[m]) m = i
		load[m] += $1
		print m, $2
	}' >"$work/plan"

pids=""
for i in "$@"; do
	names=$(awk -v i="$i" '$1 == i { print $2 }' "$work/plan" | paste -sd '|' -)
	if [ -z "$names" ]; then
		continue
	fi
	(
		start=$SECONDS
		status=0
		"$work/integration.test" -test.run "^($names)\$" -test.timeout "$timeout" -test.count 1 >"$work/shard-$i.log" 2>&1 || status=$?
		echo "$((SECONDS - start))s" >"$work/shard-$i.time"
		exit $status
	) &
	pids="$pids $!:$i"
done

status=0
for entry in $pids; do
	pid=${entry%%:*}
	i=${entry#*:}
	if wait "$pid"; then
		echo "--- integration shard $i/$shards ($(cat "$work/shard-$i.time"))"
		tail -n 1 "$work/shard-$i.log"
	else
		status=1
		echo "--- integration shard $i/$shards FAILED ($(cat "$work/shard-$i.time" 2>/dev/null))"
		cat "$work/shard-$i.log"
	fi
done

case " $* " in
*" 1 "*)
	if "$work/integration.test" -test.run "$budget" -test.timeout "$timeout" -test.count 1 -test.v >"$work/budget.log" 2>&1; then
		echo "--- integration performance budget"
		grep ' round [0-9]*:' "$work/budget.log" | sed 's/^ *//' || true
		tail -n 1 "$work/budget.log"
	else
		status=1
		echo "--- integration performance budget FAILED"
		cat "$work/budget.log"
	fi
	;;
esac
exit $status
