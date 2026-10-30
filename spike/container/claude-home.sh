#!/bin/sh
# Item 7 setup: a per-Work home seeded from an operator-named seed, mounted at
# the same path, under a read-only root and --user. TOKEN_FILE optional.
S=$(cd "$(dirname "$0")" && pwd)/scratch
SEED=$S/seed; HOMEW=$S/homes/work1; WS=$S/ws1
mkdir -p $SEED $WS && printf '{"permissions":{"allow":["Bash(echo:*)"]}}\n' > $SEED/settings.json
if [ ! -d $HOMEW ]; then
  mkdir -p $HOMEW && cp -R $SEED/. $HOMEW/
  printf '{"hasCompletedOnboarding":true,"bypassPermissionsModeAccepted":true}\n' > $HOMEW/.claude.json
fi
crun() {
  docker run --rm -i --init --read-only --tmpfs /tmp --user "$(id -u):$(id -g)" --cap-drop ALL \
    --security-opt no-new-privileges -v $HOMEW:$HOMEW -v $WS:$WS -w $WS \
    ${NETARGS} -e HOME=$HOMEW -e CLAUDE_CONFIG_DIR=$HOMEW ${TOKEN_FILE:+--env-file $TOKEN_FILE} \
    oc-spike-claude:1 "$@"
}
