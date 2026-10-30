#!/bin/sh
# Item 2 (strict): a host port known to be listening must be unreachable from
# an --internal network, by name and by every gateway IP. Item 6: uid of files
# written to a bind mount with and without --user. Needs HOSTPORT, BIND.
k=hp$$
docker network create --internal oc-$k >/dev/null
gw=$(docker network inspect oc-$k -f '{{(index .IPAM.Config 0).Gateway}}')
b0=$(docker network inspect bridge -f '{{(index .IPAM.Config 0).Gateway}}')
echo "== listening host port $HOSTPORT; internal gw $gw; bridge gw $b0"
echo "-- from default bridge (control):"
docker run --rm curlimages/curl:8.11.1 -sS -m 4 -o /dev/null -w "%{http_code}\n" http://host.docker.internal:$HOSTPORT/ 2>&1
echo "-- from --internal:"
for h in host.docker.internal $gw $b0 0.250.250.254 192.168.65.254; do
  printf '%s: ' $h; docker run --rm --network oc-$k curlimages/curl:8.11.1 -sS -m 4 -o /dev/null -w "%{http_code}\n" http://$h:$HOSTPORT/ 2>&1 | tail -1
done
docker network rm oc-$k >/dev/null
echo "== 6. uid: host $(id -u):$(id -g), bind $BIND"
mkdir -p "$BIND"
docker run --rm -v "$BIND":/w alpine:3.20 sh -c 'touch /w/as-root; mkdir /w/dir-root'
docker run --rm --user "$(id -u):$(id -g)" -v "$BIND":/w alpine:3.20 sh -c 'touch /w/as-user; mkdir /w/dir-user'
ls -ln "$BIND"
