#!/bin/sh
# Linux: with inhibit_ipv4 the old bridge IP is gone and the gateway's proxy
# still works by alias.
python3 -m http.server 47178 --bind 0.0.0.0 >/dev/null 2>&1 & A=$!
docker network create --internal -o com.docker.network.bridge.inhibit_ipv4=true oc-inh >/dev/null
docker network inspect oc-inh -f 'subnet {{range .IPAM.Config}}{{.Subnet}}{{end}}'
docker network create oc-inh-eg >/dev/null
docker run -d --name oc-inh-gw --network oc-inh-eg -e GW_ALLOW=api.github.com oc-spike-gw:1 >/dev/null
docker network connect --alias gateway oc-inh oc-inh-gw; sleep 1
for h in 172.18.0.1 172.19.0.1 172.20.0.1; do printf "$h:47178 -> "; docker run --rm --network oc-inh curlimages/curl:8.11.1 -sS -m 3 -o /dev/null -w "%{http_code}\n" http://$h:47178/ 2>&1 | tail -1; done
printf 'proxy allowed -> '; docker run --rm --network oc-inh curlimages/curl:8.11.1 -sS -m 10 -x http://gateway:3128 -o /dev/null -w "%{http_code}\n" https://api.github.com/zen
docker rm -f oc-inh-gw >/dev/null; docker network rm oc-inh oc-inh-eg >/dev/null; kill $A
