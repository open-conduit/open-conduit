#!/bin/sh
# Item 5 without a Docker restart. Each python3 call is a fresh "daemon".
D=${D:-$(dirname "$0")/scratch/rec}; R="python3 $(dirname "$0")/recover.py"
LOOP='trap "exit 0" TERM; while :; do sleep 0.2; done'
echo "== A. wrapper alive across a daemon restart"
$R launch $D/a execA "$LOOP"; sleep 2; $R recover $D/a
echo "== E. second launch beside the live one"
$R second $D/a; $R launch $D/a2 execA "$LOOP"
echo "== B. wrapper dead (SIGKILL group), container exits 7 later"
$R launch $D/b execB 'sleep 3; exit 7'; sleep 1; /bin/kill -KILL -- -$(cat $D/b/pid); sleep 4; $R recover $D/b
echo "== C. wrapper dead (SIGKILL group), container still running"
$R launch $D/c execC "$LOOP"; sleep 1; /bin/kill -KILL -- -$(cat $D/c/pid); sleep 1; $R recover $D/c; $R second $D/c
echo "== D1. container gone"
$R launch $D/d execD "$LOOP"; sleep 1; /bin/kill -KILL -- -$(cat $D/d/pid); docker rm -f $(cat $D/d/cid) >/dev/null; $R recover $D/d
echo "== D2. Docker unreachable"
$R launch $D/e execE "$LOOP"; sleep 1; DOCKER_HOST=unix:///nonexistent.sock $R recover $D/e; DOCKER_HOST=unix:///nonexistent.sock $R second $D/e
echo "== A': adopted container stopped by the host (docker stop), wrapper's exit file"
docker stop -t 5 $(cat $D/a/cid) >/dev/null; sleep 1; echo "exit file: $(cat $D/a/exit 2>/dev/null)"; $R recover $D/a
docker rm -f $(docker ps -aq --filter label=oc.state=spikestatehash) >/dev/null 2>&1
