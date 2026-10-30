"""Item 4, traced: like term.py but records docker events per run, and runs
the agent both bare and inside the executor wrapper.
Usage: python3 term2.py <bind dir> <runs> <delay>"""
import os, signal, subprocess, sys, time
from term import WRAPPER, AGENT, docker
bind, runs, delay = sys.argv[1], int(sys.argv[2]), float(sys.argv[3])
os.makedirs(bind, exist_ok=True)
for mode in ("bare", "wrapper"):
    ok = 0
    for i in range(runs):
        for f in ("got", "up"):
            try: os.remove(os.path.join(bind, f))
            except FileNotFoundError: pass
        cid = docker("create", "--init", "-i", "-v", f"{bind}:/w", "alpine:3.20", "sh", "-c", AGENT)
        ev = subprocess.Popen(["docker", "events", "--filter", f"container={cid}", "--format",
                               '{{.Action}}{{with index .Actor.Attributes "signal"}}:{{.}}{{end}}'],
                              stdout=subprocess.PIPE, text=True)
        time.sleep(0.3)
        argv = ["docker", "start", "-a", "-i", cid]
        if mode == "wrapper":
            argv = ["/bin/sh", "-c", WRAPPER, "wrap", bind] + argv
        p = subprocess.Popen(argv, stdin=subprocess.PIPE, stdout=subprocess.DEVNULL,
                             stderr=subprocess.PIPE, start_new_session=True)
        time.sleep(delay)
        os.killpg(p.pid, signal.SIGTERM)
        try: rc = p.wait(timeout=20)
        except subprocess.TimeoutExpired: rc = "timeout"
        time.sleep(0.5)
        state = docker("inspect", "-f", "{{.State.Status}}:{{.State.ExitCode}}", cid)
        ev.terminate(); events = " ".join(ev.communicate()[0].split())
        good = state == "exited:42"
        ok += good
        if not good:
            print(f"  {mode} run {i}: rc={rc} state={state} events=[{events}] stderr={p.stderr.read().decode().strip()[:120]}")
        docker("rm", "-f", cid)
    print(f"{mode} delay {delay}s: {ok}/{runs} exited 42 (agent trapped TERM)")
