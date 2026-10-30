#!/bin/sh
# Item 5 across a Docker restart: one segment running with its wrapper alive,
# one already exited. RESTART is the platform's restart command.
D=${D:-$(dirname "$0")/scratch/rst}; R="python3 $(dirname "$0")/recover.py"
LOOP='trap "exit 0" TERM; while :; do sleep 0.2; done'
docker rm -f $(docker ps -aq --filter label=oc.state=spikestatehash) >/dev/null 2>&1
$R launch $D/run execR "$LOOP"; $R launch $D/done execX 'exit 5'; sleep 3
id0=$(docker info -f '{{.ID}}'); echo "daemon id before: $id0"
echo "-- before restart:"; $R recover $D/run; $R recover $D/done
echo "-- restarting: $RESTART"; sh -c "$RESTART" >/dev/null 2>&1
i=0; until docker info >/dev/null 2>&1 || [ $i -gt 120 ]; do sleep 1; i=$((i+1)); done
sleep 3; echo "daemon back after ${i}s; id after: $(docker info -f '{{.ID}}')"
echo "-- after restart:"; $R recover $D/run; $R recover $D/done; $R second $D/run
echo "exit files: run=$(cat $D/run/exit 2>/dev/null) done=$(cat $D/done/exit 2>/dev/null)"
docker rm -f $(docker ps -aq --filter label=oc.state=spikestatehash) >/dev/null 2>&1
