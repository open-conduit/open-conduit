package ssh

import (
	"sort"
	"strings"
)

// The remote side is POSIX sh, sent with each call: nothing of Open
// Conduit is installed there but the toolbelt's os. Every script starts
// with R, the prefix of every mirrored path ("" but for a fake remote that
// shares the host's filesystem), and D, the run records directory.
//
// A run's records, in ~/.open-conduit/runs/<id>/ on the remote, are
// owner-only: record.json (its labels, pin and roots), start (its entry
// stub), secrets (removed by the stub before anything runs), started (the
// host's time the helper started it, so a recovered run is proved against
// the host's clock), pid (the stub's, the leader of its own session and
// group), stamp (when its upload finished: anything newer comes back) and
// exit (the stub's status).

// runsDir is where a remote keeps its runs' records, under its home.
const runsDir = ".open-conduit/runs"

// marker is in the stub's argument vector, with the run's ID: an
// unguessable nonce, so a reused PID is never taken for the run, as the
// local executors prove theirs.
const marker = "open-conduit-remote-"

// prelude defines alive: whether the run in directory $1 with ID $2 still
// has its stub, the leader of its own process group (p, its PID) whose
// arguments carry the run's nonce.
const prelude = `D="$HOME/` + runsDir + `"
alive() {
  [ -f "$1/pid" ] || return 1
  p=$(cat "$1/pid")
  case $p in ''|*[!0-9]*) return 1;; esac
  g=$(ps -o pgid= -p "$p" 2>/dev/null | tr -d ' ')
  [ "$g" = "$p" ] || return 1
  ps -o args= -p "$p" 2>/dev/null | grep -F -q -- "` + marker + `$2"
}
show() {
  d="$D/$1"
  if [ ! -f "$d/record.json" ]; then echo "gone $1"; return; fi
  printf 'record %s ' "$1"; tr -d '\n' < "$d/record.json"; echo
  if [ -f "$d/started" ]; then echo "started $1 $(cat "$d/started")"; fi
  if [ -f "$d/exit" ]; then echo "exit $1 $(cat "$d/exit")"; fi
  if [ -f "$d/pid" ]; then echo "pid $1"; fi
  if alive "$d" "$1"; then echo "running $1"; fi
}
guard() {
  case "$R$1" in /|'') echo "refusing to replace /" >&2; exit 3;; esac
  case "$HOME/" in "$R$1"/*) echo "refusing to replace $R$1, which holds the remote's home" >&2; exit 3;; esac
}
`

// Scripts, each run as `sh -c <script> open-conduit <args>`.
const (
	// scriptInspect <id>...: each run's state, or gone.
	scriptInspect = `for id in "$@"; do show "$id"; done`
	// scriptList: every run's state.
	scriptList = `[ -d "$D" ] || exit 0
for d in "$D"/*/; do [ -d "$d" ] || continue; id=${d%/}; id=${id##*/}; show "$id"; done`
	// scriptPrepare <id>: the run's records, from a tar on stdin.
	scriptPrepare = `umask 077; d="$D/$1"
[ ! -e "$d" ] || { echo "run $1 exists" >&2; exit 3; }
mkdir -p "$d" && tar -xf - -C "$d"`
	// scriptUpload <id> <root>...: the roots replaced by a tar on stdin,
	// then the stamp.
	scriptUpload = `d="$D/$1"; shift
[ -f "$d/record.json" ] || { echo "no run records" >&2; exit 3; }
for r in "$@"; do guard "$r"; done
for r in "$@"; do rm -rf -- "$R$r" || exit 3; done
mkdir -p "$R/" && tar -xf - -C "$R/" && : > "$d/stamp"`
	// scriptRun <id> <started>: starts the run's stub in a session of its
	// own, records its PID and waits for it. A run starts once.
	scriptRun = `d="$D/$1"; cd "$d" || exit 125
[ ! -e started ] || { echo "run $1 has already started" >&2; exit 125; }
echo "$2" > started
exec 3<&0
setsid env -i /bin/sh "$d/start" "` + marker + `$1" <&3 3<&- &
c=$!
exec 3<&-
echo "$c" > pid.tmp && mv pid.tmp pid
wait "$c"`
	// scriptSignal <id> <signal>: the signal to the run's stub, which
	// passes it on.
	scriptSignal = `d="$D/$1"; alive "$d" "$1" || exit 0; kill -"$2" "$p"`
	// scriptStop <id> <tenths>: TERM to the stub, then, after the grace
	// (in tenths of a second), KILL to its whole group.
	scriptStop = `d="$D/$1"; alive "$d" "$1" || exit 0
kill -TERM "$p" 2>/dev/null
i=0; while [ "$i" -lt "$2" ] && kill -0 "$p" 2>/dev/null; do sleep 0.1; i=$((i+1)); done
kill -KILL -"$p" 2>/dev/null; exit 0`
	// scriptRemove <id> <root>...: the run killed if it runs, its copies
	// of the roots and its records removed.
	scriptRemove = `d="$D/$1"; id=$1; shift
if alive "$d" "$id"; then kill -KILL -"$p" 2>/dev/null; sleep 0.1; fi
for r in "$@"; do guard "$r"; done
for r in "$@"; do rm -rf -- "$R$r" || exit 3; done
rm -rf -- "$d"`
	// scriptList0 <root>...: every path under the roots, NUL-separated.
	scriptList0 = `cd "$R/" || exit 3
for r in "$@"; do [ ! -e "$R$r" ] && [ ! -L "$R$r" ] || find ".$r" -print0 || exit 3; done`
	// scriptDownload <id> <root>...: a tar of every directory under the
	// roots and every other path newer than the stamp.
	scriptDownload = `d="$D/$1"; shift
[ -f "$d/stamp" ] || { echo "no upload stamp" >&2; exit 3; }
cd "$R/" || exit 3
for r in "$@"; do [ ! -e "$R$r" ] && [ ! -L "$R$r" ] || find ".$r" \( -type d -o -newer "$d/stamp" \) -print0 || exit 3; done | tar --null --no-recursion -T - -cf -`
	// scriptHealth: what a run needs of the remote, one line each.
	scriptHealth = `for c in sh setsid ps tar find env; do command -v "$c" >/dev/null 2>&1 || echo "missing $c"; done
PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin; command -v os >/dev/null 2>&1 || echo "no-os"
mkdir -p "$D" 2>/dev/null && [ -w "$D" ] || echo "unwritable $D"
for r in "$@"; do mkdir -p "$R$r" 2>/dev/null && [ -w "$R$r" ] || echo "unwritable $R$r"; done
echo "uname $(uname -sm)"`
)

// stub is a run's entry, run by scriptRun with an empty environment as
// the leader of its own session: it reads and removes the credentials
// file, sets the launch's environment, runs the launch's argv in its
// working directory with the session's stdin, writes the TERM marker when
// it is sent TERM, passes TERM, INT and HUP on and records the command's
// status before exiting with it. Its wait loop is the Docker entry stub's.
func stub(root string, env []string, dir, termMarker string, argv []string) string {
	var b strings.Builder
	b.WriteString("d=${0%/*}\n")
	b.WriteString(`if [ -f "$d/secrets" ]; then . "$d/secrets" || exit 125; rm -f "$d/secrets"; fi` + "\n")
	sorted := append([]string(nil), env...)
	sort.Strings(sorted)
	for _, kv := range sorted {
		k, v, _ := strings.Cut(kv, "=")
		b.WriteString("export " + k + "=" + quote(v) + "\n")
	}
	b.WriteString("cd " + quote(root+dir) + " || exit 125\n")
	b.WriteString("m=" + quote(root+termMarker) + "\n")
	b.WriteString(`c=; p=; g=; l=
f() { g=1; if [ -n "$c" ]; then kill -"$1" "$c" 2>/dev/null; else p=$1; fi; }
trap ': > "$m"; f TERM' TERM
trap 'f INT' INT
trap 'f HUP' HUP
exec 3<&0
`)
	for i, a := range argv {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(quote(a))
	}
	b.WriteString(` <&3 3<&- &
c=$!
exec 3<&-
if [ -n "$p" ]; then kill -"$p" "$c" 2>/dev/null; fi
while :; do g=; wait "$c"; s=$?; if [ -z "$g" ]; then if [ "$s" = 127 ] && [ -n "$l" ]; then s=$l; fi; echo "$s" > "$d/exit.tmp" && mv -f "$d/exit.tmp" "$d/exit"; exit "$s"; fi; l=$s; done
`)
	return b.String()
}

// quote is s as one POSIX shell word.
func quote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// command is the remote command line for script with args: ssh joins its
// arguments into one line for the remote's shell, so each is quoted.
func command(root, script string, args ...string) string {
	parts := []string{"sh", "-c", quote("R=" + quote(root) + "\n" + prelude + script), "open-conduit"}
	for _, a := range args {
		parts = append(parts, quote(a))
	}
	return strings.Join(parts, " ")
}
