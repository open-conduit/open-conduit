#!/bin/sh
# Items 1-3: toolbelt forward through the gateway, --internal egress and DNS,
# and the image-mounted os binary. MODE=mac forwards to a host loopback port
# via host.docker.internal; MODE=linux bind-mounts a host toolbelt.sock.
# Needs: TOKEN_DIR (holds `token`), and PORT (mac) or SOCK (linux).
set -u
k=spike$$
AGENT=curlimages/curl:8.11.1
docker network create --internal oc-$k-int >/dev/null
docker network create oc-$k-egress >/dev/null
docker volume create oc-$k-shared >/dev/null
if [ "$MODE" = mac ]; then
  target=tcp:host.docker.internal:$PORT; extra=""
else
  target=unix:/run/oc/toolbelt.sock; extra="-v $SOCK:/run/oc/toolbelt.sock"
fi
docker create --name oc-$k-gw --network oc-$k-egress -v oc-$k-shared:/shared $extra \
  -e GW_SOCKET=/shared/toolbelt.sock -e GW_TARGET=$target -e GW_ALLOW=api.github.com \
  oc-spike-gw:1 >/dev/null
docker network connect --alias gateway oc-$k-int oc-$k-gw
docker start oc-$k-gw >/dev/null; sleep 1
run() { docker run --rm --network oc-$k-int --user "$(id -u):$(id -g)" --read-only --cap-drop ALL \
  --security-opt no-new-privileges -v oc-$k-shared:/shared -v "$TOKEN_DIR":/run/oc-token:ro \
  --mount type=image,source=oc-spike-gw:1,target=/opt/oc \
  -e OS_ENDPOINT=unix:///shared/toolbelt.sock -e OS_TOKEN_FILE=/run/oc-token/token \
  --entrypoint sh $AGENT -c "$1" 2>&1; }
echo "== docker $(docker version --format '{{.Server.Version}}')"
echo "== 3. image mount: os from the gateway image"; run 'ls -l /opt/oc/os; /opt/oc/os version'
echo "== 1. toolbelt call through the gateway"; run '/opt/oc/os call github.context | head -c 300; echo'
echo "== 2a. direct internet";    run 'curl -sS -m 5 -o /dev/null -w "%{http_code}\n" https://1.1.1.1 || true'
echo "== 2b. host.docker.internal"; run 'curl -sS -m 5 -o /dev/null -w "%{http_code}\n" http://host.docker.internal:${PORT:-22} || true'
echo "== 2c. host loopback other port (host-gateway)"; run 'getent hosts host.docker.internal; curl -sS -m 5 http://host.docker.internal:22 || true; curl -sS -m 5 http://172.17.0.1:22 || true'
echo "== 2d. outside DNS";        run 'nslookup example.com 2>&1 | tail -3; getent hosts example.com || echo "no resolve"'
echo "== 2e. proxy, allowed";     run 'curl -sS -m 10 -x http://gateway:3128 -o /dev/null -w "%{http_code}\n" https://api.github.com/zen'
echo "== 2f. proxy, not allowed"; run 'curl -sS -m 10 -x http://gateway:3128 -o /dev/null -w "%{http_code}\n" https://example.com || true'
echo "== gateway log"; docker logs oc-$k-gw 2>&1 | tail -5
docker rm -f oc-$k-gw >/dev/null; docker network rm oc-$k-int oc-$k-egress >/dev/null; docker volume rm oc-$k-shared >/dev/null
