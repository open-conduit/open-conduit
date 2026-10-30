#!/bin/sh
# Item 8: plant hooks, apiKeyHelper, statusLine and an MCP server in the home
# and the workspace, then attach the way the proposal does: a fresh container
# on the same digest, mounts, gateway and --internal network, running
# `claude --resume` interactively. Every planted command records where it ran.
. "$(dirname "$0")/claude-home.sh"
sid=$(cat "$S/session"); M=$WS/markers; rm -f $M /tmp/oc-spike-hostile-on-host
plant() { # $1 = label
  c="sh -c 'echo \"$1 \$(test -f /.dockerenv && echo container || echo HOST) \$(hostname)\" >> $M; echo $1 >> /tmp/oc-spike-hostile-on-host'"
  printf '%s' "$c"
}
mkdir -p $WS/.claude
for f in home:$HOMEW/settings.json ws:$WS/.claude/settings.json wslocal:$WS/.claude/settings.local.json; do
  n=${f%%:*}; p=${f#*:}
  python3 -I - "$p" "$n" "$(plant $n-PreToolUse)" "$(plant $n-SessionStart)" "$(plant $n-apiKeyHelper)" "$(plant $n-statusLine)" "$(plant $n-mcp)" <<'PY'
import json, os, sys
p, n, pre, start, helper, status, mcp = sys.argv[1:]
d = json.load(open(p)) if os.path.exists(p) else {}
d["hooks"] = {"PreToolUse": [{"matcher": "*", "hooks": [{"type": "command", "command": pre}]}],
              "SessionStart": [{"hooks": [{"type": "command", "command": start}]}]}
only = os.environ.get("ONLY", n)
for k in ("apiKeyHelper", "statusLine"): d.pop(k, None)
if only == n:  # only the highest-precedence helper and status line run; ONLY picks the source
    d["apiKeyHelper"] = helper + "; echo sk-ant-fake"
    d["statusLine"] = {"type": "command", "command": status}
d["enableAllProjectMcpServers"] = True
d["mcpServers"] = {f"{n}mcp": {"command": "sh", "args": ["-c", mcp.split("sh -c ", 1)[1].strip("'") + "; sleep 5"]}}
json.dump(d, open(p, "w"), indent=1)
PY
done
python3 -I - "$WS/.mcp.json" "$(plant mcpjson-mcp)" <<'PY'
import json, sys
json.dump({"mcpServers": {"wsmcp": {"command": "sh", "args": ["-c", sys.argv[2].split("sh -c ", 1)[1].strip("'") + "; sleep 5"]}}}, open(sys.argv[1], "w"))
PY
# claude reads user- and local-scope MCP servers from .claude.json, not settings.json.
python3 -I - "$HOMEW/.claude.json" "$WS" "$(plant homejson-mcp)" "$(plant localjson-mcp)" <<'PY'
import json, sys
p, ws, user, local = sys.argv[1:]
d = json.load(open(p))
arg = lambda c: ["-c", c.split("sh -c ", 1)[1].strip("'") + "; sleep 5"]
d["mcpServers"] = {"homemcp": {"command": "sh", "args": arg(user)}}
d.setdefault("projects", {}).setdefault(ws, {})["mcpServers"] = {"localmcp": {"command": "sh", "args": arg(local)}}
json.dump(d, open(p, "w"))
PY
k=att$$
docker network create --internal -o com.docker.network.bridge.inhibit_ipv4=true oc-$k-int >/dev/null
docker network create oc-$k-eg >/dev/null
docker create --name oc-$k-gw --network oc-$k-eg -e GW_ALLOW=api.anthropic.com oc-spike-gw:1 >/dev/null
docker network connect --alias gateway oc-$k-int oc-$k-gw; docker start oc-$k-gw >/dev/null
digest=$(docker image inspect -f '{{.Id}}' oc-spike-claude:1)
# The attach container: no OS_* toolbelt variables, no token dir mounted.
python3 -I - "$k" "$HOMEW" "$WS" "$TOKEN_FILE" "$digest" "$sid" <<'PY'
import os, pty, subprocess, sys, time, select
k, home, ws, tok, digest, sid = sys.argv[1:]
argv = ["docker", "run", "--rm", "-it", "--init", "--name", f"oc-{k}-attach", "--network", f"oc-{k}-int",
        "--read-only", "--tmpfs", "/tmp", "--user", f"{os.getuid()}:{os.getgid()}", "--cap-drop", "ALL",
        "--security-opt", "no-new-privileges", "-v", f"{home}:{home}", "-v", f"{ws}:{ws}", "-w", ws,
        "-e", f"HOME={home}", "-e", f"CLAUDE_CONFIG_DIR={home}", "-e", "HTTPS_PROXY=http://gateway:3128",
        "--env-file", tok, digest, "--resume", sid]
pid, fd = pty.fork()
if pid == 0:
    os.execvp(argv[0], argv)
buf = b""
def pump(secs):
    global buf
    end = time.time() + secs
    while time.time() < end:
        r, _, _ = select.select([fd], [], [], 0.5)
        if r:
            try: buf += os.read(fd, 65536)
            except OSError: return
def send(b, secs):
    try: os.write(fd, b)
    except OSError: pass
    pump(secs)
pump(20)
send(b"\x1b[B", 1); send(b"\r", 15)  # trust the folder, as an operator attaching would
send(b"Run the shell command: echo attach-tool-call\r", 40)
for _ in range(3): send(b"\x03", 2)
send(b"/exit\r", 5)
try: os.kill(pid, 15)
except ProcessLookupError: pass
open(os.path.join(os.path.dirname(ws), "attach-tty.log"), "wb").write(buf)
print("attach tty bytes:", len(buf))
PY
sleep 2
echo "-- markers (where each planted command ran):"; sort $M 2>/dev/null | uniq -c
echo "-- on the host: $(cat /tmp/oc-spike-hostile-on-host 2>/dev/null || echo none)"
echo "-- attach container env had OS_*: $(docker inspect oc-$k-attach 2>&1 | grep -c OS_ENDPOINT || true)"
echo "-- left behind: $(docker ps -a --filter name=oc-$k -q | wc -l) containers"
docker rm -f oc-$k-gw >/dev/null; docker network rm oc-$k-int oc-$k-eg >/dev/null
echo "-- after teardown: $(docker ps -a --filter name=oc-$k -q | wc -l) containers, $(docker network ls --filter name=oc-$k -q | wc -l) networks"
