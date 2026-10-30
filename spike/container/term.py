"""Item 4: TERM to the executor wrapper's process group must reach the agent
under --init via `docker start -a -i`. Mirrors executors/claudecode's wrapper.
Usage: python3 term.py <bind dir> <runs> <delay seconds>..."""
import os, signal, subprocess, sys, time

WRAPPER = '''d=$1; shift
printf '%s\\n' "$$" > "$d/pid"
trap ':' HUP INT TERM
"$@"
c=$?
trap '' HUP INT TERM
kill -TERM 0 2>/dev/null
printf '%s\\n' "$c" > "$d/exit"
exit "$c"'''
AGENT = "trap 'echo term > /w/got; exit 42' TERM; echo up > /w/up; while :; do sleep 0.05; done"

def docker(*a):
    return subprocess.run(["docker", *a], capture_output=True, text=True).stdout.strip()

bind, runs = sys.argv[1], int(sys.argv[2])
os.makedirs(bind, exist_ok=True)
for delay in map(float, sys.argv[3:]):
    ok, fails = 0, []
    for i in range(runs):
        for f in ("got", "up", "exit", "pid"):
            try: os.remove(os.path.join(bind, f))
            except FileNotFoundError: pass
        cid = docker("create", "--init", "-i", "--user", f"{os.getuid()}:{os.getgid()}",
                     "-v", f"{bind}:/w", "alpine:3.20", "sh", "-c", AGENT)
        p = subprocess.Popen(["/bin/sh", "-c", WRAPPER, "wrap", bind, "docker", "start", "-a", "-i", cid],
                             stdin=subprocess.PIPE, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
                             start_new_session=True)
        time.sleep(delay)
        os.killpg(p.pid, signal.SIGTERM)
        try: p.wait(timeout=20)
        except subprocess.TimeoutExpired: p.kill()
        got = os.path.exists(os.path.join(bind, "got"))
        state = docker("inspect", "-f", "{{.State.Status}} {{.State.ExitCode}}", cid)
        exitf = open(os.path.join(bind, "exit")).read().strip() if os.path.exists(os.path.join(bind, "exit")) else "-"
        if got and state.startswith("exited"):
            ok += 1
        else:
            fails.append(f"run {i}: agent_got_term={got} up={os.path.exists(os.path.join(bind,'up'))} container={state} wrapper_exit={exitf}")
        docker("rm", "-f", cid)
    print(f"delay {delay}s: {ok}/{runs} reached the agent and the container exited")
    for f in fails[:5]: print("  ", f)
