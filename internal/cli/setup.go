package cli

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	goruntime "runtime"
	"sort"
	"strings"

	"github.com/charmbracelet/x/term"
)

// Setup (03-05): the v1 wizard and config CLI concepts mapped onto scopes,
// connections and bindings. Everything here edits the local config file and
// validates it offline against the daemon's registrations before saving; a
// running daemon changes only through `config apply`.

// SetupRequest starts a coding agent with the setup guide (`init --with`).
type SetupRequest struct {
	Agent, ConfigPath, StateDir, Cwd string
}

var (
	scopeIDPattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,62}$`)
	repoPattern    = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
	refPattern     = regexp.MustCompile(`^(keyring:[a-z][a-z0-9_-]{0,62}|file:[a-z][a-z0-9_-]{0,62}|env:[A-Za-z_][A-Za-z0-9_]*|cmd:[^\r\n]+)$`)
	remotePattern  = regexp.MustCompile(`github\.com[:/]([A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+?)(?:\.git)?/?$`)
)

// AgentOperations are the toolbelt operations an agent binding gets for a
// GitHub scope (the builtins it uses plus the github.* tools); HoldOperations
// those of the github-hold binding. They mirror the setup guide.
var (
	AgentOperations = []string{"read_context", "wait", "declare_outcome",
		"github.context", "github.issue.view", "github.issue.comment", "github.issue.list", "github.issue.create",
		"github.pr.list", "github.pr.view", "github.pr.create", "github.pr.comment", "github.pr.reply", "github.checks",
		"github.attachment.upload", "github.attachment.get", "github.wait", "github.correction", "github.problem"}
	HoldOperations = []string{"emit", "github.hold.status", "github.hold"}
)

func (o *operator) configPath(flag string) string {
	if flag != "" {
		return o.abs(flag)
	}
	if v := o.env.Getenv("OPEN_CONDUIT_CONFIG"); v != "" {
		return o.abs(v)
	}
	return defaultConfigPath(o.env)
}

// GitHubRepo reads a checkout's GitHub repository from its origin remote
// (v1 wizard's found-git detection). ok is false when there is none.
func GitHubRepo(dir string) (string, bool) {
	out, err := exec.Command("git", "-C", dir, "remote", "get-url", "origin").Output()
	if err != nil {
		return "", false
	}
	m := remotePattern.FindStringSubmatch(strings.TrimSpace(string(out)))
	if m == nil {
		return "", false
	}
	return m[1], true
}

// ScopeID derives a scope id from a repository name.
func ScopeID(repo string) string {
	name := strings.ToLower(repo[strings.LastIndex(repo, "/")+1:])
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	id := strings.Trim(b.String(), "-")
	if id == "" || id[0] < 'a' || id[0] > 'z' {
		id = "repo-" + id
	}
	if len(id) > 63 {
		id = id[:63]
	}
	return id
}

// GitHubScope is what `init` adds for one repository.
type GitHubScope struct {
	Repo, Scope, Connection, Token string
	Checkout                       string // empty: workspace-free scope
	Holds                          bool
}

func obj(kv ...any) map[string]any {
	m := map[string]any{}
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i].(string)] = kv[i+1]
	}
	return m
}

// ScopeConfig is the scope JSON for g: an agent binding on claude-code with
// the GitHub tools, a git work resource when a checkout is given, and
// optionally the hold route and binding.
func (g GitHubScope) ScopeConfig() map[string]any {
	ops := append([]string{}, AgentOperations...)
	authOps := append([]string{"emit"}, AgentOperations...)
	requires := obj("toolbelt", "mcp", "inject", true, "resume", true, "workspace", g.Checkout != "")
	agent := obj("name", "agent", "executor", "claude-code", "connection", g.Connection, "requires", requires,
		"operations", ops, "secret_names", []string{},
		"config", obj("status_schema", "github.status.v1", "branch", "agent/issue-{number}"))
	if g.Checkout != "" {
		agent["resource"] = obj("provider", "git", "config", obj("checkout", g.Checkout, "mode", "work"))
	}
	bindings := []any{agent}
	routes := []any{}
	if g.Holds {
		authOps = append(authOps, "github.hold.status", "github.hold")
		bindings = append(bindings, obj("name", "hold", "executor", "github-hold", "connection", g.Connection,
			"requires", obj("toolbelt", "sdk"), "operations", HoldOperations, "secret_names", []string{}, "config", obj()))
		routes = append(routes, obj("name", "held", "handler", "github-hold", "binding", "hold", "active_policy", "queue", "config", obj()))
	}
	routes = append(routes, obj("name", "issues", "handler", "github", "binding", "agent", "active_policy", "inject", "config", obj()))
	host := "github.com"
	return obj("id", g.Scope, "name", g.Repo,
		"authorization", obj("connections", []string{g.Connection}, "read_scopes", []string{}, "operations", authOps),
		"bindings", bindings, "routes", routes,
		"extensions", []any{obj("name", g.Scope, "type", "github", "connection", g.Connection, "secret_names", []string{},
			"config", obj("namespace", "github/"+host+"/"+g.Repo, "scope", g.Scope, "repo", g.Repo))},
		"secret_refs", obj())
}

func toNode(v any) *node {
	b, _ := json.Marshal(v)
	n, _ := parseNode(b)
	return n
}

// AddGitHubScope merges g into a config document (nil starts a new one).
// An existing connection with g's id is kept; an existing scope with g's id
// is replaced only with force.
func AddGitHubScope(doc []byte, g GitHubScope, force bool) ([]byte, error) {
	var root *node
	if len(bytes.TrimSpace(doc)) == 0 {
		root = toNode(obj("version", 1, "settings", obj("concurrency", 2, "admission_limit", 3), "connections", []any{}, "scopes", []any{}))
	} else {
		var err error
		if root, err = parseNode(doc); err != nil || root.kind != 'o' {
			return nil, fmt.Errorf("existing config is not a JSON object")
		}
	}
	for _, k := range []string{"connections", "scopes"} {
		if root.field(k) == nil {
			_ = root.set([]string{k}, toNode([]any{}))
		}
	}
	conns := root.field("connections")
	if _, ok := conns.index(g.Connection); !ok {
		conns.arr = append(conns.arr, toNode(obj("id", g.Connection, "type", "github", "config", obj(), "secret_refs", obj("token", g.Token))))
	}
	scopes := root.field("scopes")
	scope := toNode(g.ScopeConfig())
	if i, ok := scopes.index(g.Scope); ok {
		if !force {
			return nil, fmt.Errorf("scope %s already exists; choose --scope or pass --force to replace it", g.Scope)
		}
		scopes.arr[i] = scope
	} else {
		scopes.arr = append(scopes.arr, scope)
	}
	return root.bytes(), nil
}

// writePrivate writes data atomically with owner-only permissions.
func writePrivate(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".config-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err = tmp.Chmod(0o600); err == nil {
		if _, err = tmp.Write(data); err == nil {
			err = tmp.Sync()
		}
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// validate checks a candidate offline; problems are printed as pointer,
// code and remedy (like `config validate`).
func (o *operator) validate(data []byte) bool {
	if !json.Valid(data) {
		fmt.Fprintln(o.errOut, "open-conduit: config is not valid JSON")
		return false
	}
	if o.env.ValidateConfig == nil {
		return true
	}
	details, err := o.env.ValidateConfig(data)
	if err != nil {
		fmt.Fprintf(o.errOut, "open-conduit: config could not be validated: %s\n", err)
		return false
	}
	for _, d := range details {
		fmt.Fprintf(o.errOut, "  %s  %s  %s\n", d.Path, d.Code, d.Message)
	}
	return len(details) == 0
}

func (o *operator) init(args []string) int {
	f := newFlags("init", "repo", "scope", "checkout", "token", "connection", "with", "config")
	noWorkspace := f.fs.Bool("no-workspace", false, "")
	holds := f.fs.Bool("holds", false, "")
	force := f.fs.Bool("force", false, "")
	dry := f.fs.Bool("dry-run", false, "")
	if pos, err := f.parse(args); err != nil || len(pos) != 0 {
		return usageError(o.errOut, "invalid init flags")
	}
	path := o.configPath(f.get("config"))
	if agent := f.get("with"); agent != "" {
		if o.env.Setup == nil {
			fmt.Fprintln(o.errOut, "open-conduit: init --with is not available in this binary")
			return 1
		}
		return o.env.Setup(SetupRequest{Agent: agent, ConfigPath: path, StateDir: o.stateDir(), Cwd: o.env.Cwd})
	}
	g := GitHubScope{Repo: f.get("repo"), Scope: f.get("scope"), Connection: f.get("connection"), Token: f.get("token"), Holds: *holds}
	checkout := o.abs(f.get("checkout"))
	if checkout == "" && !*noWorkspace {
		// Found git: the current directory, when it is a GitHub checkout.
		if repo, ok := GitHubRepo(o.env.Cwd); ok && (g.Repo == "" || strings.EqualFold(repo, g.Repo)) {
			checkout = o.env.Cwd
			if g.Repo == "" {
				g.Repo = repo
			}
		}
	} else if checkout != "" && g.Repo == "" {
		g.Repo, _ = GitHubRepo(checkout)
	}
	if *noWorkspace {
		checkout = ""
	}
	g.Checkout = checkout
	if !repoPattern.MatchString(g.Repo) {
		return usageError(o.errOut, "init needs --repo OWNER/NAME (or run it in a GitHub checkout)")
	}
	if g.Scope == "" {
		g.Scope = ScopeID(g.Repo)
	}
	if g.Connection == "" {
		g.Connection = "github"
	}
	if g.Token == "" {
		g.Token = "cmd:gh auth token"
	}
	if !scopeIDPattern.MatchString(g.Scope) || !scopeIDPattern.MatchString(g.Connection) {
		return usageError(o.errOut, "--scope and --connection must match [a-z][a-z0-9_-]*")
	}
	if !refPattern.MatchString(g.Token) {
		return usageError(o.errOut, "--token takes a reference (keyring:NAME, file:NAME, env:VAR or cmd:COMMAND), never a token")
	}
	existing, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return o.emit(nil, fmt.Errorf("config file unreadable"))
	}
	out, err := AddGitHubScope(existing, g, *force)
	if err != nil {
		return o.emit(nil, err)
	}
	if !o.validate(out) {
		fmt.Fprintln(o.errOut, "open-conduit: not saved; nothing changed")
		return 1
	}
	if *dry {
		_, _ = o.out.Write(out)
		return 0
	}
	if err = writePrivate(path, out); err != nil {
		return o.emit(nil, fmt.Errorf("could not save the config"))
	}
	workspace := "none (workspace-free; turns run in a private directory)"
	if g.Checkout != "" {
		workspace = g.Checkout + " (a worktree per issue)"
	}
	fmt.Fprintf(o.out, "Saved %s\n\n  scope      %s\n  repository %s\n  credential %s connection, token %s\n  workspace  %s\n  holds      %t\n\n",
		path, g.Scope, g.Repo, g.Connection, refKind(g.Token), workspace, g.Holds)
	fmt.Fprintf(o.out, "Next:\n  open-conduit serve                 start the daemon (reconcile every 15 minutes; add --webhook-addr for webhooks)\n  open-conduit doctor --check-secrets  check the credential privately and GitHub access\n  open-conduit tui                   watch it work\n")
	if existing != nil {
		fmt.Fprintf(o.out, "A running daemon keeps its configuration until you run: open-conduit config apply %s --generation N\n", path)
	}
	return 0
}

// refKind names a reference's scheme without its text.
func refKind(ref string) string {
	scheme, _, _ := strings.Cut(ref, ":")
	return scheme + ":…"
}

func (o *operator) secretsPath(flag string) string {
	return filepath.Join(filepath.Dir(o.configPath(flag)), "secrets.json")
}

// keyringStore stores a secret in the platform keyring under the
// open-conduit service. The value travels on stdin, never in arguments.
var keyringStore = func(name, value string) error {
	switch goruntime.GOOS {
	case "darwin":
		cmd := exec.Command("/usr/bin/security", "-i")
		cmd.Stdin = strings.NewReader(fmt.Sprintf("add-generic-password -U -s open-conduit -a %s -X %s\n", name, hex.EncodeToString([]byte(value))))
		return cmd.Run()
	case "linux":
		cmd := exec.Command("secret-tool", "store", "--label=open-conduit "+name, "service", "open-conduit", "account", name)
		cmd.Stdin = strings.NewReader(value)
		return cmd.Run()
	}
	return errors.New("no platform keyring")
}

// secret stores one secret value read from stdin (a terminal line or piped
// input) and prints the reference to put in secret_refs. The keyring is
// preferred; the owner-only secrets.json beside the config is the reported
// fallback (B05). The value is never echoed, logged or taken as an argument.
func (o *operator) secret(args []string) int {
	if len(args) == 0 {
		return usageError(o.errOut, "expected secret set NAME | secret path")
	}
	f := newFlags(args[0], "store", "config")
	pos, err := f.parse(args[1:])
	if err != nil {
		return usageError(o.errOut, "invalid secret flags")
	}
	switch args[0] {
	case "path":
		fmt.Fprintln(o.out, o.secretsPath(f.get("config")))
		return 0
	case "set":
	default:
		return usageError(o.errOut, "expected secret set NAME | secret path")
	}
	if len(pos) != 1 || !scopeIDPattern.MatchString(pos[0]) {
		return usageError(o.errOut, "secret set takes one NAME matching [a-z][a-z0-9_-]*")
	}
	name, store := pos[0], f.get("store")
	if store != "" && store != "keyring" && store != "file" {
		return usageError(o.errOut, "--store is keyring or file")
	}
	stdin := o.env.Stdin
	if stdin == nil {
		stdin = os.Stdin
	}
	fmt.Fprintf(o.errOut, "Enter the value for %s, then press Enter (input is read from stdin and never shown again):\n", name)
	var line string
	if file, ok := stdin.(*os.File); ok && term.IsTerminal(file.Fd()) {
		// A terminal does not echo the value.
		b, err := term.ReadPassword(file.Fd())
		fmt.Fprintln(o.errOut)
		if err != nil {
			return o.emit(nil, errors.New("could not read the value"))
		}
		line = string(b)
	} else {
		read, err := bufio.NewReader(io.LimitReader(stdin, 64<<10)).ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return o.emit(nil, errors.New("could not read the value"))
		}
		line = read
	}
	value := strings.TrimRight(line, "\r\n")
	if value == "" {
		return usageError(o.errOut, "empty value; nothing stored")
	}
	ref := ""
	if store != "file" {
		if err := keyringStore(name, value); err == nil {
			ref = "keyring:" + name
		} else if store == "keyring" {
			return o.emit(nil, errors.New("the platform keyring refused the secret"))
		} else {
			fmt.Fprintln(o.errOut, "open-conduit: no usable keyring; falling back to the owner-only secrets file")
		}
	}
	if ref == "" {
		path := o.secretsPath(f.get("config"))
		values := map[string]string{}
		if data, err := os.ReadFile(path); err == nil {
			if json.Unmarshal(data, &values) != nil {
				return o.emit(nil, fmt.Errorf("%s is not a JSON object of strings; fix it by hand", path))
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return o.emit(nil, errors.New("secrets file unreadable"))
		}
		values[name] = value
		names := make([]string, 0, len(values))
		for k := range values {
			names = append(names, k)
		}
		sort.Strings(names)
		var b bytes.Buffer
		b.WriteString("{\n")
		for i, k := range names {
			kb, _ := marshal(k)
			vb, _ := marshal(values[k])
			fmt.Fprintf(&b, "  %s: %s", kb, vb)
			if i < len(names)-1 {
				b.WriteByte(',')
			}
			b.WriteByte('\n')
		}
		b.WriteString("}\n")
		if err := writePrivate(path, b.Bytes()); err != nil {
			return o.emit(nil, errors.New("could not save the secrets file"))
		}
		ref = "file:" + name
		fmt.Fprintf(o.errOut, "Stored in %s (0600)\n", path)
	}
	return o.emit(map[string]string{"reference": ref}, nil)
}

// localConfig implements the file-level config commands; ok is false for
// subcommands that go to the daemon.
func (o *operator) localConfig(args []string) (int, bool) {
	switch args[0] {
	case "path", "show", "check", "get", "set", "unset":
	default:
		return 0, false
	}
	f := newFlags(args[0], "file")
	pos, err := f.parse(args[1:])
	if err != nil {
		return usageError(o.errOut, "invalid config flags"), true
	}
	path := o.configPath(f.get("file"))
	if args[0] == "path" {
		fmt.Fprintln(o.out, path)
		return 0, true
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return o.emit(nil, fmt.Errorf("no config at %s; create one with open-conduit init", path)), true
	}
	root, err := parseNode(data)
	if err != nil || root.kind != 'o' {
		return o.emit(nil, fmt.Errorf("config is not a JSON object")), true
	}
	switch args[0] {
	case "show":
		root.redact()
		_, _ = o.out.Write(root.bytes())
		return 0, true
	case "check":
		if !o.validate(data) {
			return 1, true
		}
		fmt.Fprintln(o.out, "valid")
		return 0, true
	case "get":
		if len(pos) != 1 {
			return usageError(o.errOut, "expected config get POINTER"), true
		}
		parts, err := pointer(pos[0])
		if err != nil {
			return usageError(o.errOut, err.Error()), true
		}
		n, err := root.lookup(parts)
		if err != nil {
			return o.emit(nil, err), true
		}
		n.redact()
		_, _ = o.out.Write(n.bytes())
		return 0, true
	}
	var parts []string
	switch {
	case args[0] == "set" && len(pos) == 2, args[0] == "unset" && len(pos) == 1:
		if parts, err = pointer(pos[0]); err != nil {
			return usageError(o.errOut, err.Error()), true
		}
	default:
		return usageError(o.errOut, "expected config set POINTER JSON | config unset POINTER"), true
	}
	if args[0] == "set" {
		value, err := parseNode([]byte(pos[1]))
		if err != nil {
			return usageError(o.errOut, "the value must be JSON (quote strings: '\"text\"')"), true
		}
		if len(parts) >= 2 && parts[len(parts)-2] == "secret_refs" && (value.kind != 's' || !refPattern.MatchString(strings.Trim(string(value.scalar), `"`))) {
			return usageError(o.errOut, "a secret_refs value is a reference (keyring:, file:, env:, cmd:); store values with open-conduit secret set"), true
		}
		err = root.set(parts, value)
		if err != nil {
			return o.emit(nil, err), true
		}
	} else if err = root.unset(parts); err != nil {
		return o.emit(nil, err), true
	}
	out := root.bytes()
	if !o.validate(out) {
		fmt.Fprintln(o.errOut, "open-conduit: not saved; the config file is unchanged")
		return 1, true
	}
	if err := writePrivate(path, out); err != nil {
		return o.emit(nil, fmt.Errorf("could not save the config")), true
	}
	fmt.Fprintf(o.errOut, "Saved %s. A running daemon changes only with: open-conduit config apply %s --generation N\n", path, path)
	return 0, true
}
