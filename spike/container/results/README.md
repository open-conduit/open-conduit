# Container spike (#176): how to rerun

Throwaway evidence for #171. Platforms: OrbStack (Docker 29.4.0) and an Ubuntu 24.04
OrbStack machine running its own Docker 29.9.2 (`orb -m spike176`). Docker Desktop was
not installed. Findings: see the comment on #171.

| Item | Script | Notes |
| --- | --- | --- |
| 1-3 | `net-toolbelt.sh` (macOS: `MODE=mac PORT=… TOKEN_DIR=…`), `linux-net.sh` | needs `/tmp/oc-spike-fwd` (fwd/) forwarding a loopback port to the daemon socket |
| 2 strict | `hostport-uid.sh`, `linux-internal-gw.sh`, `linux-inhibit-check.sh` | Linux bridge-IP leak and the `inhibit_ipv4` fix |
| 4 | `term3.py` (`MODES=proxy,relay,helper,signal`) | `ocrun/` is the helper stand-in |
| 5 | `recover.py`, `recover-run.sh`, `docker-restart.sh` | |
| 6 | `hostport-uid.sh` | |
| 7 | `claude-resume.sh`, `claude-resume-gw.sh` | `TOKEN_FILE` holds `CLAUDE_CODE_OAUTH_TOKEN=` |
| 8 | `hostile-attach.sh` (`ONLY=home\|ws\|wslocal\|none`) | |
