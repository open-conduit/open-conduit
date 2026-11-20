package codex

import (
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/open-conduit/open-conduit/extension"
)

// A container binding runs the segment in a container the host owns
// (ExecutionContext.Container). The host's segment driver (RunSegment) is
// this package's own binary, which the container doesn't have, so in a
// container the driver is containerDriver: the same loop in /bin/sh, run
// as the container's command. It runs each turn as RunSegment does and
// takes the follow-up turn's events from the .md beside each inbox entry,
// which Inject renders on the host.

// containerPath is a container's PATH unless the binding's env sets one:
// the daemon's own names host directories.
const containerPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// openaiHost is the API a binding without a provider reaches.
const openaiHost = "api.openai.com"

// containerDriver is RunSegment in /bin/sh: $1 is the run directory, $2
// the thread to resume ("" for a new one), $3 the --output-schema file
// ("" for none), and the rest codex and the options every turn passes. It
// exits with the last turn's status.
var containerDriver = `d=$1 t=$2 s=$3; shift 3
turn() {
  m=$1; shift
  b=$1; shift
  rm -f "$d/` + lastFile + `"
  if [ -n "$t" ]; then set -- exec resume "$@"; else set -- exec "$@"; fi
  set -- "$@" --json --output-last-message "$d/` + lastFile + `"
  if [ -n "$s" ]; then set -- "$@" --output-schema "$s"; fi
  if [ -n "$t" ]; then set -- "$@" "$t"; fi
  "$b" "$@" - <"$m" >>"$d/` + eventsFile + `" 2>>"$d/` + errFile + `"
}
thread() {
  sed -n 's/.*"type":"thread\.started".*"thread_id":"\([^"]*\)".*/\1/p;s/.*"thread_id":"\([^"]*\)".*"type":"thread\.started".*/\1/p' "$d/` + eventsFile + `" 2>/dev/null | head -n 1
}
turn "$d/` + promptFile + `" "$@"; c=$?
n=0
while :; do
  [ -n "$t" ] || t=$(thread)
  if [ "$c" -ne 0 ] || [ -z "$t" ] || [ "$n" -ge ` + fmt.Sprint(maxFollowUp) + ` ]; then exit "$c"; fi
  p=
  for f in "$d/` + inboxDir + `"/*.md; do
    [ -e "$f" ] || continue
    g=${f##*-}; g=${g%.md}
    [ -e "$d/` + inboxDir + `/consumed/$g" ] || p="$p $g"
  done
  [ -n "$p" ] || exit 0
  {
    cat "$d/` + interjectionFile + `"; sep=
    for g in $p; do printf '%s' "$sep"; cat "$d/` + inboxDir + `"/*-"$g.md"; sep='

'; done
  } >"$d/` + followUpFile + `"
  turn "$d/` + followUpFile + `" "$@"; c=$?
  for g in $p; do : >"$d/` + inboxDir + `/consumed/$g"; done
  n=$((n+1))
done`

// Files a container segment's driver reads from its run directory.
const (
	interjectionFile = "interjection.md"
	followUpFile     = "follow-up.md"
)

// containerArgv is the container's command for a segment: the driver,
// its run directory, the thread, the schema file and codex's argv.
func containerArgv(dir string, spec segmentSpec) []string {
	return append([]string{"/bin/sh", "-c", containerDriver, "codex-segment", dir, spec.Thread, spec.Schema, spec.Binary}, spec.Args...)
}

// credentialSuffixes mark a variable as a credential by its name.
var credentialSuffixes = []string{"_TOKEN", "_API_KEY", "_SECRET", "_PASSWORD"}

func credentialVar(name string) bool {
	for _, s := range credentialSuffixes {
		if strings.HasSuffix(strings.ToUpper(name), s) {
			return true
		}
	}
	return false
}

// containerHome points a container launch's codex at the Work's home
// (HOME, and CODEX_HOME in it, where codex keeps its sessions and any
// login made there) and a Linux PATH, and moves every credential out of
// env into the secrets the provider keeps in a file: each selected secret,
// the provider's key and any variable named as a credential
// (credentialVar). The host's own codex login is never in the home: a
// container binding signs in with a selected secret.
func containerHome(cfg Config, home string, env []string, selected map[string]string) ([]string, []string, error) {
	if home == "" || !filepath.IsAbs(home) {
		return nil, nil, fmt.Errorf("%w: the host gave the container no home for the Work", extension.ErrSandboxUnavailable)
	}
	codexHome := filepath.Join(home, ".codex")
	if err := os.MkdirAll(codexHome, 0o700); err != nil {
		return nil, nil, fmt.Errorf("%w: could not prepare the Work's home: %v", extension.ErrSandboxUnavailable, err)
	}
	secret := map[string]bool{}
	for name := range selected {
		if cfg.providerKey(name) {
			secret[providerKeyVar] = true
		} else {
			secret[cfg.secretVariable(name)] = true
		}
	}
	var out, secrets []string
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		switch {
		case k == "HOME" || k == "CODEX_HOME" || k == "TMPDIR" || k == "PATH" && cfg.Env["PATH"] == "":
		case secret[k] || credentialVar(k):
			secrets = append(secrets, kv)
		default:
			out = append(out, kv)
		}
	}
	out = append(out, "HOME="+home, "CODEX_HOME="+codexHome)
	if cfg.Env["PATH"] == "" {
		out = append(out, "PATH="+containerPath)
	}
	sort.Strings(out)
	sort.Strings(secrets)
	return out, secrets, nil
}

// apiHost is the host a binding's turns reach: its provider's, else
// OpenAI's.
func (c Config) apiHost() string {
	if c.Provider != nil {
		if u, err := url.Parse(c.Provider.BaseURL); err == nil && u.Hostname() != "" {
			if port := u.Port(); port != "" {
				return net.JoinHostPort(u.Hostname(), port)
			}
			return u.Hostname()
		}
	}
	return openaiHost
}

// VersionProbe is `<binary> --version` with the PATH a container launch
// runs it with (extension.VersionProber), so a container binding's codex
// is checked in its image; none for a config the executor refuses.
func VersionProbe(raw json.RawMessage) ([]string, []string) {
	c, err := ParseConfig(raw)
	if err != nil {
		return nil, nil
	}
	path := c.Env["PATH"]
	if path == "" {
		path = containerPath
	}
	return []string{c.Binary, "--version"}, []string{"PATH=" + path}
}
