"""Item 4, timed: the agent takes 3s to exit 42 after TERM. Records whether
TERM reached it, the attached CLI's exit status and when it returned relative
to the container, for two host argvs:
  proxy:   docker start -a -i <id>                (the proposal)
  relay:   sh relay: docker start -a -i <id> in its own process group
           (set -m), TERM trapped and sent as docker kill -s TERM <id>,
           exit status from docker wait <id>
  helper:  ocrun <id> (Go): the same as relay without depending on sh job control
  signal:  docker start -a -i <id>, Interrupt sent as docker kill -s TERM <id>
           by the host (the executor's Signal call) instead of to the group
Usage: python3 term3.py <runs> <delay>..."""
import os, signal, subprocess, sys, time

AGENT = "trap 'sleep 3; exit 42' TERM; while :; do sleep 0.05; done"
RELAY = '''id=$1
trap 'docker kill -s TERM "$id" >/dev/null 2>&1' TERM INT HUP
set -m
docker start -a -i "$id" & a=$!
set +m
while :; do wait $a; s=$?; kill -0 $a 2>/dev/null || break; done
exit "$(docker wait "$id")"'''
WRAPPER = '''d=$1; shift
trap ':' HUP INT TERM
"$@"
c=$?
trap '' HUP INT TERM
kill -TERM 0 2>/dev/null
exit "$c"'''

def docker(*a):
    return subprocess.run(["docker", *a], capture_output=True, text=True).stdout.strip()

OCRUN = os.path.join(os.path.dirname(os.path.abspath(__file__)), "scratch",
                     "ocrun-darwin" if sys.platform == "darwin" else "ocrun-linux")
MODES = os.environ.get("MODES", "proxy,relay,helper,signal").split(",")
runs = int(sys.argv[1])
for delay in map(float, sys.argv[2:]):
    for mode in MODES:
        res = {}
        for i in range(runs):
            cid = docker("create", "--init", "-i", "alpine:3.20", "sh", "-c", AGENT)
            inner = {"proxy": ["docker", "start", "-a", "-i", cid], "signal": ["docker", "start", "-a", "-i", cid],
                     "relay": ["/bin/sh", "-c", RELAY, "relay", cid], "helper": [OCRUN, cid]}[mode]
            p = subprocess.Popen(["/bin/sh", "-c", WRAPPER, "wrap", "/tmp"] + inner, stdin=subprocess.PIPE,
                                 stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, start_new_session=True)
            time.sleep(delay)
            t0 = time.time()
            if mode == "signal":
                docker("kill", "-s", "TERM", cid)
            else:
                try: os.killpg(p.pid, signal.SIGTERM)
                except PermissionError: pass
            try: rc = p.wait(timeout=30)
            except subprocess.TimeoutExpired: rc = "timeout"
            if docker("inspect", "-f", "{{.State.Running}}", cid) == "true":
                rc = f"{rc}/agent_never_signalled"; docker("stop", "-t", "1", cid)
            t_cli = time.time() - t0
            docker("wait", cid)
            state = docker("inspect", "-f", "{{.State.Status}}:{{.State.ExitCode}}", cid)
            ran = state == "exited:42"
            key = f"agent_trapped={ran} wrapper_rc={rc} wrapper_returned={'after' if t_cli > 2.5 else 'before'}_agent_exit state={state}"
            res[key] = res.get(key, 0) + 1
            docker("rm", "-f", cid)
        print(f"{mode:5} delay {delay}s ({runs} runs):")
        for k, v in sorted(res.items()): print(f"   {v:2}x {k}")
