#!/bin/bash
# Linux side of items 1-3: a host toolbelt.sock (relayed to the macOS daemon
# for the spike) bind-mounted into the gateway.
W=$(dirname "$0")
mkdir -p ~/oc/tok && cp $W/scratch/token ~/oc/tok/ && chmod 600 ~/oc/tok/token
rm -f ~/oc/toolbelt.sock
socat UNIX-LISTEN:$HOME/oc/toolbelt.sock,fork,mode=600 TCP:host.orb.internal:47176 & S=$!
sleep 1; ls -l ~/oc/toolbelt.sock
MODE=linux SOCK=$HOME/oc/toolbelt.sock TOKEN_DIR=$HOME/oc/tok sg docker -c "$W/net-toolbelt.sh"
kill $S
