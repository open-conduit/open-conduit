// Package migrate converts an Open Supervisor v1 configuration into a new,
// independent Open Conduit configuration (ticket 06-02). It is explicit,
// non-destructive conversion: it reads the inputs it is given, writes
// nothing, never resolves or copies a secret value and never touches v1
// state. The caller validates the result offline against the daemon's
// registrations before saving it.
//
// Open Conduit itself never reads `.open-supervisor.yaml` or
// `OPEN_SUPERVISOR_*`; this package is the deliberate conversion B05 names.
package migrate

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	gh "github.com/open-conduit/open-conduit/extensions/github"
)

// RepoFile is the v1 repository file a project's checkout could hold.
const RepoFile = ".open-supervisor.yaml"

// Options are the conversion inputs beyond the v1 config itself.
type Options struct {
	// ReadFile reads a file in a project checkout (the repository file and
	// playbook additions); nil reads none.
	ReadFile func(path string) ([]byte, bool)
	// Home expands a leading ~ in v1 paths ("" keeps it).
	Home string
	// TimeZone is the IANA zone v1's local sweep and triage times were read
	// in ("" writes UTC and reports it).
	TimeZone string
	// DetachedTimeoutSeconds is the binding deadline written for v1 status
	// (detached) jobs, which had none (default six hours).
	DetachedTimeoutSeconds int
	// ShellJobs writes plain shell bindings for jobs without a status
	// folder instead of the legacy-job wrapper.
	ShellJobs bool
}

// Item levels.
const (
	// Info is a mapping with the same behavior.
	Info = "info"
	// Changed is a mapping whose behavior or default changed.
	Changed = "changed"
	// Action is a step the operator must take before switching.
	Action = "action"
	// Warn is a conversion the operator should review.
	Warn = "warn"
	// Unsupported is v1 configuration that was not converted.
	Unsupported = "unsupported"
)

// Item is one line of the conversion report. Path names the v1 location
// (never a value), Target the Open Conduit JSON pointer when there is one.
type Item struct {
	Level   string `json:"level"`
	Path    string `json:"path"`
	Target  string `json:"target,omitempty"`
	Message string `json:"message"`
}

// Secret is one secret-reference mapping. From names the v1 location and the
// reference scheme only; reference text and values are never reported.
type Secret struct {
	From   string `json:"from"`
	Scheme string `json:"scheme"`
	To     string `json:"to"`
	Action string `json:"action,omitempty"`
}

// Result is a converted configuration and its report.
type Result struct {
	Config  json.RawMessage `json:"config"`
	Scopes  []string        `json:"scopes"`
	Items   []Item          `json:"items"`
	Secrets []Secret        `json:"secrets"`
}

var (
	idPattern  = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,62}$`)
	envPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	hhmm       = regexp.MustCompile(`^([01]?[0-9]|2[0-3]):([0-5][0-9])$`)
)

// ID turns a v1 name into a config identifier ([a-z][a-z0-9_-]{0,62}).
func ID(name, prefix string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(name) {
		ok := r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_'
		if ok {
			b.WriteRune(r)
			dash = false
		} else if !dash && b.Len() > 0 {
			b.WriteByte('-')
			dash = true
		}
	}
	id := strings.Trim(b.String(), "-_")
	if id == "" || id[0] < 'a' || id[0] > 'z' {
		id = strings.TrimSuffix(prefix+"-"+id, "-")
	}
	if len(id) > 63 {
		id = strings.TrimRight(id[:63], "-_")
	}
	return id
}

type obj = map[string]any

type converter struct {
	v1   *v1Config
	opts Options
	res  Result

	connections []any
	connIDs     map[string]string // v1 connection name → Open Conduit connection id
	scopeIDs    map[string]bool
	posthog     []string
	introNoted  bool
}

func (c *converter) note(level, path, target, format string, args ...any) {
	c.res.Items = append(c.res.Items, Item{Level: level, Path: path, Target: target, Message: fmt.Sprintf(format, args...)})
}

// Convert converts a v1 config.yaml.
func Convert(v1 []byte, opts Options) (Result, error) {
	cfg, err := parseV1(v1)
	if err != nil {
		return Result{}, err
	}
	if opts.DetachedTimeoutSeconds <= 0 {
		opts.DetachedTimeoutSeconds = 6 * 60 * 60
	}
	c := &converter{v1: cfg, opts: opts, connIDs: map[string]string{}, scopeIDs: map[string]bool{}}
	settings := c.settings()
	c.globalNotes()
	c.convertConnections()
	scopes := c.projects()
	if len(scopes) == 0 {
		return Result{}, errors.New("open supervisor config: no project could be converted (see the report)")
	}
	out := obj{"version": 1, "settings": settings, "connections": c.connections, "scopes": scopes}
	if c.res.Config, err = json.MarshalIndent(out, "", "  "); err != nil {
		return Result{}, err
	}
	c.res.Config = append(c.res.Config, '\n')
	if c.res.Items == nil {
		c.res.Items = []Item{}
	}
	if c.res.Secrets == nil {
		c.res.Secrets = []Secret{}
	}
	return c.res, nil
}

// settings maps the global v1 settings with their own defaults; only values
// differing from Open Conduit's defaults are written.
func (c *converter) settings() obj {
	s := c.v1.Settings
	out := obj{}
	if s.MaxTurns > 0 && s.MaxTurns != 4 {
		out["concurrency"] = s.MaxTurns
		c.note(Info, "settings.maxTurns", "/settings/concurrency", "turns at once across every scope")
	}
	if s.MaxOpen > 0 && s.MaxOpen != 10 {
		out["admission_limit"] = s.MaxOpen
	}
	c.note(Changed, "settings.maxOpenPerProject", "/settings/admission_limit", "v1 capped claimed issues per project; Open Conduit's per-scope admission limit is recorded but not yet enforced")
	if s.ReconcileMinutes > 0 && s.ReconcileMinutes != 15 {
		out["reconcile_seconds"] = s.ReconcileMinutes * 60
		c.note(Info, "settings.reconcileMinutes", "/settings/reconcile_seconds", "reconcile every %d minutes", s.ReconcileMinutes)
	}
	attempts := s.MaxAttempts
	if attempts <= 0 {
		attempts = 3
	}
	if attempts != 3 {
		delays := []int{}
		for i := 1; i < attempts; i++ {
			delays = append(delays, 300*i)
		}
		out["retry_delays_seconds"] = delays
	}
	c.note(Changed, "settings.maxAttempts", "/settings/retry_delays_seconds", "%d failure attempts become %d retry delays (300 s apart, growing); jobs keep v1's no-retry rule", attempts, attempts-1)
	c.note(Changed, "settings (turn timeout)", "/settings/timeout_seconds", "every attempt now has one runtime deadline (default 3600 s, per binding timeout_seconds); v1 turns had none")
	c.note(Info, "job keepRuns", "/settings/kept_executions", "one retention for every Work: the newest 20 executions keep their content (v1 kept 30 runs per job)")
	return out
}

// globalNotes reports v1 settings that become serve flags or have no
// configuration counterpart.
func (c *converter) globalNotes() {
	s := c.v1.Settings
	port := s.Port
	if port <= 0 {
		port = 8787
	}
	if s.Tunnel != "" {
		conn := c.v1.Connections[s.Tunnel]
		kind := str(conn["type"])
		flags := "--webhook-addr 127.0.0.1:<port> --tunnel " + kind
		switch kind {
		case "ngrok":
			if d := str(conn["domain"]); d != "" {
				flags += " --tunnel-domain <the reserved domain>"
			}
		case "cloudflared":
			flags += " --tunnel-name <named tunnel> --public-url <hostname>"
		case "static":
			flags = "--webhook-addr 127.0.0.1:<port> --tunnel static --public-url <url>"
		}
		c.note(Action, "settings.tunnel", "", "webhooks are a serve flag, not config: open-conduit serve %s. While v1 still runs other projects it holds port %d and its tunnel: use another port, and move (do not duplicate) an ngrok free endpoint only when its last project transfers", flags, port)
	} else {
		c.note(Info, "settings.tunnel", "", "no tunnel: Open Conduit runs reconcile-only (no --webhook-addr), as v1 did")
	}
	if s.UsageMinutes > 0 {
		c.note(Changed, "settings.usageMinutes", "", "usage windows are read only with serve --claude-usage (doctor claude_code)")
	}
	if s.ChecksDebounceSeconds > 0 {
		c.note(Changed, "settings.checksDebounceSeconds", "", "no checks debounce in Open Conduit: one wake ends a wait (03-04)")
	}
	if s.DryRun {
		c.note(Action, "settings.dryRun", "", "dry run is a serve flag: open-conduit serve --dry-run")
	}
	c.note(Changed, "projects (checkout .env)", "", "the checkout's .env layer is opt-in in Open Conduit; converted git resources set dotenv: true to keep v1's environment")
	if d := s.Host.Disk; d > 0 && d != 90 {
		c.note(Changed, "settings.host.disk", "", "the worktree disk check is per git resource: converted resources set disk_full_percent: %g (at most 10 GiB kept free, disk_reserve_bytes); the six-hourly spare-worktree sweep is the git provider's (serve --sweep-interval)", min(d, 100))
	} else {
		c.note(Info, "settings.host.disk", "", "git resources refuse a new worktree that would leave its disk over 90%% full (disk_full_percent, at most 10 GiB kept free) and the git provider sweeps spare worktrees every 6 hours (serve --sweep-interval), as v1 1.3.0 did")
	}
	c.note(Action, "state tasks/", "", "v1 1.3.0's `agent task` records (tasks/ in each project's state folder) are not read or imported, and Open Conduit has no `agent task` (rejected): before switching a project, let its running tasks end in v1 (`agent task list --all`); a worktree a task runs from stays until the task ends")
	c.note(Info, "paths", "", "Open Conduit keeps its own config (${XDG_CONFIG_HOME:-~/.config}/open-conduit/config.json), state (${XDG_STATE_HOME:-~/.local/state}/open-conduit), secrets.json and keyring service open-conduit; v1's open-supervisor config, state, secrets.yaml and keyring stay untouched")
	c.note(Info, "env OPEN_SUPERVISOR_CONFIG, OPEN_SUPERVISOR_STATE", "", "not read by Open Conduit: use --config/OPEN_CONDUIT_CONFIG and --data-dir/OPEN_CONDUIT_DATA_DIR")
}

// secret maps one v1 secret field to an Open Conduit reference. name is the
// suggested Open Conduit secret id; where says where it came from.
func (c *converter) secret(where, name, value string) string {
	value = strings.TrimSpace(value)
	kind, rest, ok := strings.Cut(value, ":")
	id := ID(name, "secret")
	m := Secret{From: where, To: "keyring:" + id}
	switch {
	case value == "":
		return ""
	case ok && kind == "env" && envPattern.MatchString(rest):
		m.Scheme, m.To = "env", value
	case ok && kind == "cmd" && rest != "":
		m.Scheme, m.To = "cmd", value
	case ok && kind == "keyring":
		m.Scheme = "keyring"
		m.Action = fmt.Sprintf("copy the value from v1's keyring service open-supervisor into open-conduit: macOS `security find-generic-password -s open-supervisor -a %s -w | open-conduit secret set %s`; Linux `secret-tool lookup service open-supervisor username %s | open-conduit secret set %s`", shellWord(rest), id, shellWord(rest), id)
	case ok && kind == "file":
		m.Scheme, m.To = "file", "file:"+id
		m.Action = fmt.Sprintf("copy the value of %s from v1's secrets.yaml with `open-conduit secret set %s --store file` (value on stdin)", shellWord(rest), id)
	default:
		m.Scheme = "literal"
		m.Action = fmt.Sprintf("v1 kept this secret as text in its config; store it with `open-conduit secret set %s` (value on stdin) and rotate it at the provider", id)
		c.note(Warn, where, "", "a literal secret was found; it is not copied (the new config holds only a reference)")
	}
	c.res.Secrets = append(c.res.Secrets, m)
	return m.To
}

func shellWord(s string) string {
	if regexp.MustCompile(`^[A-Za-z0-9_.@-]+$`).MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// convertConnections maps the v1 connections Open Conduit has a connection
// type for. claude-code connections become binding config, tunnels become
// serve flags.
func (c *converter) convertConnections() {
	for _, name := range sortedKeys(c.v1.Connections) {
		v := c.v1.Connections[name]
		typ := str(v["type"])
		id := ID(name, "conn")
		where := "connections." + name
		cfg, refs := obj{}, obj{}
		get := func(k string) string { return str(v[k]) }
		ref := func(field, secretName string) {
			if r := c.secret(where+"."+field, id+"-"+strings.ReplaceAll(secretName, "_", "-"), get(field)); r != "" {
				refs[secretName] = r
			}
		}
		switch typ {
		case "github":
			auth := get("auth")
			if auth == "" {
				auth = "gh"
			}
			switch auth {
			case "gh":
				bin := get("ghBinary")
				if bin == "" {
					bin = "gh"
				}
				refs[gh.SecretToken] = "cmd:" + shellWord(bin) + " auth token"
				c.res.Secrets = append(c.res.Secrets, Secret{From: where + ".auth", Scheme: "gh", To: "cmd:" + shellWord(bin) + " auth token"})
			case "token":
				ref("token", gh.SecretToken)
			case "app":
				cfg["auth"], cfg["app_id"] = "app", get("appId")
				ref("appPrivateKey", gh.SecretAppPrivateKey)
			}
			if b := get("botAppId"); b != "" {
				cfg["bot_app_id"] = b
				ref("botPrivateKey", gh.SecretBotPrivateKey)
			}
			if u := get("apiUrl"); u != "" && u != "https://api.github.com" {
				cfg["api_url"] = u
			}
		case "linear":
			ref("token", "token")
			if a := get("agentAccount"); a != "" {
				cfg["agent_account"] = a
			}
			if u := get("apiUrl"); u != "" && u != "https://api.linear.app/graphql" {
				cfg["api_url"] = u
			}
		case "jira":
			cfg["base_url"] = get("baseUrl")
			if a := get("auth"); a != "" && a != "basic" {
				cfg["auth"] = a
			}
			if e := get("email"); e != "" {
				cfg["email"] = e
			}
			if a := get("agentAccount"); a != "" {
				cfg["agent_account"] = a
			}
			ref("token", "token")
		case "bitbucket":
			if a := get("auth"); a != "" && a != "basic" {
				cfg["auth"] = a
			}
			if u := get("username"); u != "" {
				cfg["username"] = u
			}
			if u := get("apiUrl"); u != "" && u != "https://api.bitbucket.org/2.0" {
				cfg["api_url"] = u
			}
			ref("token", "token")
		case "posthog":
			if h := get("host"); h != "" && h != "https://us.i.posthog.com" {
				cfg["host"] = h
			}
			if d := get("distinctId"); d != "" {
				cfg["distinct_id"] = d
			} else {
				c.note(Changed, where+".distinctId", "", "the default distinct id is now open-conduit@<hostname> (v1: open-supervisor@<hostname>); set distinct_id to keep v1's")
			}
			ref("token", "token")
		case "claude-code":
			c.connIDs[name] = ""
			c.note(Info, where, "", "the Claude Code agent is now an executor binding: its fields move into each scope's claude-code binding config")
			continue
		case "ngrok", "cloudflared", "static":
			c.connIDs[name] = ""
			continue
		default:
			c.note(Unsupported, where, "", "connection type %q has no Open Conduit counterpart", typ)
			continue
		}
		c.connIDs[name] = id
		if typ == "posthog" && slices.Contains(c.v1.Settings.Telemetry, name) {
			c.posthog = append(c.posthog, id)
		}
		c.connections = append(c.connections, obj{"id": id, "type": typ, "config": cfg, "secret_refs": refs})
		if id != name {
			c.note(Changed, where, "/connections/"+id, "renamed to the identifier %s", id)
		}
	}
}

// agentConfig is the claude-code binding config from a v1 claude-code
// connection plus the global handoff settings.
func (c *converter) agentConfig(p *v1Project, where string) obj {
	name := p.Agent
	if name == "" {
		name = c.v1.Settings.Agent
	}
	cfg := obj{}
	v := c.v1.Connections[name]
	if name == "" || v == nil || str(v["type"]) != "claude-code" {
		c.note(Warn, where+".agent", "", "no claude-code agent connection: the binding uses Claude Code's defaults")
		return cfg
	}
	if b := str(v["binary"]); b != "" && b != "claude" {
		cfg["binary"] = c.path(b)
	}
	if m := str(v["model"]); m != "" && m != "opus" {
		cfg["model"] = m
	}
	if m := str(v["permissionMode"]); m != "" && m != "bypassPermissions" {
		cfg["permission_mode"] = m
	}
	if a := strings.Fields(str(v["extraArgs"])); len(a) > 0 {
		cfg["extra_args"] = a
	}
	if b, _ := strconv.ParseBool(str(v["useApiKey"])); b {
		cfg["pass_env"] = []string{"ANTHROPIC_API_KEY"}
		c.note(Changed, "connections."+name+".useApiKey", "", "ANTHROPIC_API_KEY is passed through the sanitized environment (pass_env); prefer a secret reference mapped with secret_env")
	}
	if m := str(v["dryRunModel"]); m != "" && m != "haiku" {
		c.note(Unsupported, "connections."+name+".dryRunModel", "", "serve --dry-run stub turns always use haiku")
	}
	if h := c.v1.Settings.HandoffMinutes; h != nil && *h != 55 {
		cfg["handoff_minutes"] = *h
	}
	if h := c.v1.Settings.HandoffShare; h != nil && *h != 0.5 {
		cfg["handoff_share"] = *h
	}
	return cfg
}

func (c *converter) path(p string) string {
	if c.opts.Home != "" && (p == "~" || strings.HasPrefix(p, "~/")) {
		return filepath.Join(c.opts.Home, strings.TrimPrefix(p, "~"))
	}
	return p
}

// envLayer maps a v1 project env: plain values to binding env, secret
// references to scope secrets selected by name and mapped to the variable.
type envLayer struct {
	env       map[string]string
	secretEnv map[string]string // scope secret name → variable
}

func (l envLayer) names() []string {
	out := sortedKeys(l.secretEnv)
	if out == nil {
		out = []string{}
	}
	return out
}

func isReference(v string) bool {
	kind, _, ok := strings.Cut(strings.TrimSpace(v), ":")
	return ok && (kind == "env" || kind == "keyring" || kind == "file" || kind == "cmd")
}
