#!/bin/sh
# Item 7 behind the gateway: the same as claude-resume.sh, with the agent on an
# --internal network and api.anthropic.com its only allowed host.
k=cr$$
docker network create --internal -o com.docker.network.bridge.inhibit_ipv4=true oc-$k-int >/dev/null
docker network create oc-$k-eg >/dev/null
docker run -d --name oc-$k-gw --network oc-$k-eg -e GW_ALLOW=api.anthropic.com oc-spike-gw:1 >/dev/null
docker network connect --alias gateway oc-$k-int oc-$k-gw
NETARGS="--network oc-$k-int -e HTTPS_PROXY=http://gateway:3128" sh "$(dirname "$0")/claude-resume.sh"
echo "-- gateway log:"; docker logs oc-$k-gw 2>&1 | cut -d' ' -f3- | sort | uniq -c
docker rm -f oc-$k-gw >/dev/null; docker network rm oc-$k-int oc-$k-eg >/dev/null
