#!/bin/sh
# Item 7: sign in with CLAUDE_CODE_OAUTH_TOKEN alone, then resume the session
# in a second container. TOKEN_FILE holds CLAUDE_CODE_OAUTH_TOKEN=... (0600).
. "$(dirname "$0")/claude-home.sh"
out=$(crun -p 'Remember the word PERIWINKLE. Reply only OK.' --output-format json)
sid=$(printf '%s' "$out" | python3 -I -c 'import json,sys; d=json.load(sys.stdin); print(d["session_id"]); print(d["is_error"], d["result"][:80], file=sys.stderr)')
echo "first container session: $sid"
crun -p 'What word did I ask you to remember? Reply with the word only.' --resume "$sid" --output-format json |
  python3 -I -c 'import json,sys; d=json.load(sys.stdin); print("second container:", d["is_error"], d["result"][:80], d["session_id"])'
ls -ln "$HOMEW" | sed 1d; find "$HOMEW/projects" -name '*.jsonl' | head -3
echo "$sid" > "$S/session"
