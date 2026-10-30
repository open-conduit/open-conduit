#!/bin/sh
# Linux: is the host reachable from an --internal network through the bridge's
# own IP, for 0.0.0.0 and 127.0.0.1 listeners, and does inhibit_ipv4 close it?
python3 -m http.server 47178 --bind 0.0.0.0 >/dev/null 2>&1 & A=$!
python3 -m http.server 47179 --bind 127.0.0.1 >/dev/null 2>&1 & B=$!
sleep 1
for opt in "" "-o com.docker.network.bridge.inhibit_ipv4=true"; do
  docker network create --internal $opt oc-gwt >/dev/null || continue
  g=$(docker network inspect oc-gwt -f '{{range .IPAM.Config}}{{.Gateway}}{{end}}')
  echo "== network opts [$opt] gateway [$g]; host addrs: $(ip -4 -o addr show | awk '{print $4}' | tr '\n' ' ')"
  docker run -d --name oc-gwt-peer --network oc-gwt alpine:3.20 sleep 60 >/dev/null
  for h in ${g:-172.18.0.1} 172.17.0.1 $(hostname -I | awk '{print $1}'); do
    for p in 47178 47179; do
      printf '%s:%s -> ' $h $p
      docker run --rm --network oc-gwt curlimages/curl:8.11.1 -sS -m 3 -o /dev/null -w "%{http_code}\n" http://$h:$p/ 2>&1 | tail -1
    done
  done
  printf 'container-to-container on the network: '
  docker run --rm --network oc-gwt alpine:3.20 ping -c1 -W2 oc-gwt-peer >/dev/null 2>&1 && echo ok || echo FAIL
  docker rm -f oc-gwt-peer >/dev/null; docker network rm oc-gwt >/dev/null
done
kill $A $B
