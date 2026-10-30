"""Item 5: each Recover path. Phases are separate processes so state survives
only in the pin file, as it would across an Open Conduit daemon restart.
  python3 recover.py launch <dir> <exec> <agent-cmd>   create, pin, start wrapper detached
  python3 recover.py recover <dir>                     classify as Recover would
  python3 recover.py second <dir>                      a retry: refuse if one is running
The wrapper is the relay argv from term3.py inside the executor's wrapper."""
import json, os, signal, subprocess, sys, time
from datetime import datetime, timezone

IMAGE = "alpine:3.20"
STATE = "spikestatehash"
RELAY = '''id=$1
trap 'docker kill -s TERM "$id" >/dev/null 2>&1' TERM INT HUP
set -m
docker start -a -i "$id" & a=$!
set +m
while :; do wait $a; s=$?; kill -0 $a 2>/dev/null || break; done
exit "$(docker wait "$id")"'''
WRAPPER = '''d=$1; shift
printf '%s\\n' "$$" > "$d/pid"
trap ':' HUP INT TERM
"$@"
c=$?
trap '' HUP INT TERM
kill -TERM 0 2>/dev/null
printf '%s\\n' "$c" > "$d/exit.tmp" && mv -f "$d/exit.tmp" "$d/exit"
exit "$c"'''

def docker(*a, check=False):
    r = subprocess.run(["docker", *a], capture_output=True, text=True)
    if check and r.returncode: raise RuntimeError(r.stderr.strip())
    return r

def launch(d, ex, cmd):
    os.makedirs(d, exist_ok=True)
    for f in ("pid", "exit", "cid", "pin.json"):
        try: os.remove(os.path.join(d, f))
        except FileNotFoundError: pass
    digest = docker("image", "inspect", "-f", "{{index .RepoDigests 0}}", IMAGE, check=True).stdout.strip()
    image_id = docker("image", "inspect", "-f", "{{.Id}}", digest, check=True).stdout.strip()
    running = live(ex)
    if running:
        print(f"refused: container_running {running}"); return
    pinned_at = datetime.now(timezone.utc).isoformat()
    cid = docker("create", "--name", f"oc-{ex}-1", "--init", "-i", "--restart", "no", "--cidfile", f"{d}/cid",
                 "--label", f"oc.state={STATE}", "--label", f"oc.exec={ex}", "--label", "oc.segment=1",
                 "--label", f"oc.digest={digest}", digest, "sh", "-c", cmd, check=True).stdout.strip()
    json.dump({"id": cid, "exec": ex, "digest": digest, "image_id": image_id, "pinned_at": pinned_at},
              open(f"{d}/pin.json", "w"))
    p = subprocess.Popen(["/bin/sh", "-c", WRAPPER, "wrap", d, "/bin/sh", "-c", RELAY, "relay", cid],
                         stdin=subprocess.DEVNULL, stdout=open(f"{d}/out.log", "a"),
                         stderr=subprocess.STDOUT, start_new_session=True)
    print(f"launched {cid[:12]} wrapper pid {p.pid}")

def live(ex):
    r = docker("ps", "-q", "--filter", f"label=oc.state={STATE}", "--filter", f"label=oc.exec={ex}",
               "--filter", "status=running")
    return r.stdout.split()

def prove(pin):
    r = docker("inspect", pin["id"])
    if r.returncode:
        return None, r.stderr.strip().splitlines()[-1] if r.stderr.strip() else "inspect failed"
    c = json.loads(r.stdout)[0]
    L = c["Config"]["Labels"]
    started = c["State"]["StartedAt"]
    checks = {
        "id": c["Id"] == pin["id"],
        "labels": L.get("oc.state") == STATE and L.get("oc.exec") == pin["exec"] and L.get("oc.digest") == pin["digest"],
        "image": c["Image"] == pin["image_id"],
        "started_after_pin": started[:26] > pin["pinned_at"][:26],
    }
    return c, checks

def alive(pid):
    try: os.kill(pid, 0); return True
    except ProcessLookupError: return False
    except PermissionError: return True

def recover(d):
    pin = json.load(open(f"{d}/pin.json"))
    pid = int(open(f"{d}/pid").read())
    wrapper = alive(pid)
    exitf = open(f"{d}/exit").read().strip() if os.path.exists(f"{d}/exit") else None
    c, checks = prove(pin)
    if c is None:
        print(f"wrapper_alive={wrapper} exit_file={exitf} container: cannot confirm ({checks}) -> ErrNotAdoptable, Stop cannot confirm, replacement blocked")
        return
    st = c["State"]
    print(f"wrapper_alive={wrapper} exit_file={exitf} container={st['Status']} exit={st['ExitCode']} oom={st['OOMKilled']} "
          f"started={st['StartedAt']} finished={st['FinishedAt']} checks={checks}")
    proved = all(checks.values())
    if wrapper and st["Running"]:
        print("-> " + ("adopt: proved pinned container" if proved else "refuse: not the pinned container"))
    elif not st["Running"] and st["Status"] == "exited":
        print(f"-> result: container exit code {st['ExitCode']}")
    elif st["Running"]:
        docker("stop", "-t", "2", pin["id"])
        after = docker("inspect", "-f", "{{.State.Running}} {{.State.Status}} {{.State.ExitCode}}", pin["id"]).stdout.strip()
        print(f"-> ErrNotAdoptable; host stop -> inspect: {after}")
    else:
        print(f"-> state {st['Status']}: not adoptable")

if __name__ == "__main__":
    op = sys.argv[1]
    if op == "launch": launch(sys.argv[2], sys.argv[3], sys.argv[4])
    elif op == "recover": recover(sys.argv[2])
    elif op == "second":
        pin = json.load(open(f"{sys.argv[2]}/pin.json")); r = live(pin["exec"])
        print(f"retry for {pin['exec']}: " + (f"refused: container_running {r}" if r else "free to launch"))
