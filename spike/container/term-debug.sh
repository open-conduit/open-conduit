#!/bin/sh
# One TERM run with docker events recorded, with and without -i.
B=$1; mkdir -p $B
for flags in "-a -i" "-a"; do
  rm -f $B/got
  cid=$(docker create --init -i -v $B:/w alpine:3.20 sh -c "trap 'echo term > /w/got; exit 42' TERM; while :; do sleep 0.05; done")
  docker events --filter container=$cid --format '{{.Action}} {{index .Actor.Attributes "signal"}}' > $B/events & E=$!
  python3 -I -c "
import subprocess,os,signal,time,sys
p=subprocess.Popen(['docker','start']+sys.argv[2].split()+[sys.argv[1]],stdin=subprocess.PIPE,start_new_session=True)
time.sleep(1.5); os.killpg(p.pid,signal.SIGTERM)
try: print('cli exit',p.wait(timeout=10))
except subprocess.TimeoutExpired: print('cli still running')
" $cid "$flags"
  sleep 1; kill $E
  echo "flags [$flags]: state $(docker inspect -f '{{.State.Status}} {{.State.ExitCode}}' $cid) got=$(cat $B/got 2>/dev/null)"; echo "events:"; cat $B/events | tr '\n' ';'; echo
  docker rm -f $cid >/dev/null
done
