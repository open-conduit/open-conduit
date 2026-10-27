package github

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strings"

	"github.com/open-conduit/open-conduit/extension"
)

// Setup (03-05, moved behind the generic extension.Setup hook in 04-02):
// `open-conduit init github` adds one scope for a repository. The v1 wizard
// outcomes are kept: found-git remote detection, an optional git work
// resource, the agent binding with the GitHub tools, the github route and,
// with --holds, the github-hold binding and route.

// AgentOperations are the toolbelt operations an agent binding gets for a
// GitHub scope (the builtins it uses plus the github.* tools); HoldOperations
// those of the github-hold binding. They mirror the setup guide.
var (
	AgentOperations = []string{"read_context", "wait", "declare_outcome",
		"github.context", "github.issue.view", "github.issue.comment", "github.issue.list", "github.issue.create",
		"github.pr.list", "github.pr.view", "github.pr.create", "github.pr.comment", "github.pr.reply", "github.pr.review", "github.checks",
		"github.attachment.upload", "github.attachment.get", "github.wait"}
	HoldOperations = []string{"emit", "github.hold.status", "github.hold"}
)

var (
	remotePattern = regexp.MustCompile(`github\.com[:/]([A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+?)(?:\.git)?/?$`)
	// RefPattern is a secret reference (never a secret value).
	RefPattern = regexp.MustCompile(`^(keyring:[a-z][a-z0-9_-]{0,62}|file:[a-z][a-z0-9_-]{0,62}|env:[A-Za-z_][A-Za-z0-9_]*|cmd:[^\r\n]+)$`)
)

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

// GitHubScope is what `init github` adds for one repository.
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
		"config", obj("branch", "agent/issue-{number}"))
	if g.Checkout != "" {
		agent["resource"] = obj("provider", "git", "config", obj("checkout", g.Checkout, "mode", "work"))
	}
	bindings := []any{agent}
	routes := []any{}
	if g.Holds {
		authOps = append(authOps, "github.hold.status", "github.hold")
		bindings = append(bindings, obj("name", "hold", "executor", HoldExecutor, "connection", g.Connection,
			"requires", obj("toolbelt", "sdk"), "operations", HoldOperations, "secret_names", []string{}, "config", obj()))
		routes = append(routes, obj("name", "held", "handler", HoldHandler, "binding", "hold", "active_policy", "queue", "config", obj()))
	}
	routes = append(routes, obj("name", "issues", "handler", Type, "binding", "agent", "active_policy", "inject", "config", obj()))
	host := "github.com"
	return obj("id", g.Scope, "name", g.Repo,
		"authorization", obj("connections", []string{g.Connection}, "read_scopes", []string{}, "operations", authOps),
		"bindings", bindings, "routes", routes,
		"extensions", []any{obj("name", g.Scope, "type", Type, "connection", g.Connection, "secret_names", []string{},
			"config", obj("namespace", "github/"+host+"/"+g.Repo, "scope", g.Scope, "repo", g.Repo))},
		"secret_refs", obj())
}

// ConnectionConfig is the connection JSON g adds when none with its id exists.
func (g GitHubScope) ConnectionConfig() map[string]any {
	return obj("id", g.Connection, "type", Type, "config", obj(), "secret_refs", obj(SecretToken, g.Token))
}

// Result is the setup result for g.
func (g GitHubScope) Result() (extension.SetupResult, error) {
	conn, err := json.Marshal(g.ConnectionConfig())
	if err != nil {
		return extension.SetupResult{}, err
	}
	scope, err := json.Marshal(g.ScopeConfig())
	if err != nil {
		return extension.SetupResult{}, err
	}
	workspace := "none (workspace-free; turns run in a private directory)"
	if g.Checkout != "" {
		workspace = g.Checkout + " (a worktree per issue)"
	}
	scheme, _, _ := strings.Cut(g.Token, ":")
	return extension.SetupResult{
		Connections: []json.RawMessage{conn},
		Scope:       scope,
		Summary: []string{"scope      " + g.Scope, "repository " + g.Repo,
			fmt.Sprintf("credential %s connection, token %s:…", g.Connection, scheme),
			"workspace  " + workspace, fmt.Sprintf("holds      %t", g.Holds)},
		Next: []string{
			"open-conduit serve                 start the daemon (reconcile every 15 minutes; store settings.webhooks for webhooks)",
			"open-conduit doctor --check-secrets  check the credential privately and GitHub access",
			"open-conduit tui                   watch it work"},
	}, nil
}

// Setup is the GitHub extension's `init github`.
type Setup struct{}

func (Setup) SetupInfo() extension.SetupInfo {
	return extension.SetupInfo{Type: Type, Summary: "add a scope that works a GitHub repository's issues",
		Usage: "[--repo OWNER/NAME] [--scope ID] [--checkout DIR | --no-workspace] [--token REF] [--connection ID] [--holds]"}
}

func usage(msg string) error { return fmt.Errorf("%w: %s", extension.ErrSetupUsage, msg) }

func (Setup) Setup(_ context.Context, host extension.SetupHost, args []string) (extension.SetupResult, error) {
	fs := flag.NewFlagSet("init github", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	repo := fs.String("repo", "", "")
	scope := fs.String("scope", "", "")
	checkoutFlag := fs.String("checkout", "", "")
	token := fs.String("token", "", "")
	connection := fs.String("connection", "", "")
	noWorkspace := fs.Bool("no-workspace", false, "")
	holds := fs.Bool("holds", false, "")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		if errors.Is(err, flag.ErrHelp) {
			return extension.SetupResult{}, usage("init github " + Setup{}.SetupInfo().Usage)
		}
		return extension.SetupResult{}, usage("invalid init github flags")
	}
	g := GitHubScope{Repo: *repo, Scope: *scope, Connection: *connection, Token: *token, Holds: *holds}
	checkout := *checkoutFlag
	if checkout != "" && !strings.HasPrefix(checkout, "/") {
		checkout = strings.TrimSuffix(host.Cwd(), "/") + "/" + checkout
	}
	if checkout == "" && !*noWorkspace {
		// Found git: the current directory, when it is a GitHub checkout.
		if found, ok := GitHubRepo(host.Cwd()); ok && (g.Repo == "" || strings.EqualFold(found, g.Repo)) {
			checkout = host.Cwd()
			if g.Repo == "" {
				g.Repo = found
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
		return extension.SetupResult{}, usage("init github needs --repo OWNER/NAME (or run it in a GitHub checkout)")
	}
	if g.Scope == "" {
		g.Scope = ScopeID(g.Repo)
	}
	if g.Connection == "" {
		g.Connection = Type
	}
	if g.Token == "" {
		g.Token = "cmd:gh auth token"
	}
	if !idPattern.MatchString(g.Scope) || !idPattern.MatchString(g.Connection) {
		return extension.SetupResult{}, usage("--scope and --connection must match [a-z][a-z0-9_-]*")
	}
	if !RefPattern.MatchString(g.Token) {
		return extension.SetupResult{}, usage("--token takes a reference (keyring:NAME, file:NAME, env:VAR or cmd:COMMAND), never a token")
	}
	return g.Result()
}
