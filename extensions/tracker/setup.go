package tracker

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/alsiola/open-conduit/extension"
)

// Playbook is the tracker work-issue playbook skill (Claude Code plugin).
const Playbook = "work-tracker-issue"

// RefPattern is a secret reference (never a secret value).
var RefPattern = regexp.MustCompile(`^(keyring:[a-z][a-z0-9_-]{0,62}|file:[a-z][a-z0-9_-]{0,62}|env:[A-Za-z_][A-Za-z0-9_]*|cmd:[^\r\n]+)$`)

// IDPattern is a config identifier (scope, connection).
var IDPattern = idPattern

// ScopeSpec is what a tracker setup adds: one scope working the entry's
// issues with a claude-code agent binding carrying the tracker's tools, an
// optional git work resource, the tracker route and the entry.
type ScopeSpec struct {
	Type, Scope, Name, Connection string
	// ConnectionConfig is the connection's nonsecret config; Secrets maps
	// connection secret names to references.
	ConnectionConfig map[string]any
	Secrets          map[string]string
	// Entry is the tracker-specific entry config (namespace and scope are
	// filled in).
	Entry     map[string]any
	Namespace string
	// Checkout is a git checkout for a work resource ("" means workspace-free).
	Checkout string
	// CodeHost optionally adds a code host (Bitbucket) to the scope: its
	// connection, entry, route and the agent's tools.
	CodeHost *CodeHost
	// Summary lines describe the choices.
	Summary []string
}

func obj(kv ...any) map[string]any {
	m := map[string]any{}
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i].(string)] = kv[i+1]
	}
	return m
}

// CodeHost is a code host entry a tracker setup adds beside the tracker.
type CodeHost struct {
	Type, Name, Connection string
	ConnectionConfig       map[string]any
	Secrets                map[string]string
	// Entry is the code host's entry config (scope is filled in).
	Entry      map[string]any
	Operations []string
}

// BitbucketOperations are the Bitbucket extension's tool operations (kept in
// step with extensions/bitbucket.Operations by an integration test; the
// tracker packages cannot import the code host).
var BitbucketOperations = []string{"bitbucket.context", "bitbucket.pr.list", "bitbucket.pr.view", "bitbucket.pr.create", "bitbucket.pr.comment", "bitbucket.pr.reply", "bitbucket.checks"}

// BitbucketCodeHost is the Bitbucket Cloud code host for a tracker scope:
// repo is workspace/slug, the PRs correlate with the tracker entry's issues
// by its key prefixes, and issueRef is how a PR body refers to an issue.
func BitbucketCodeHost(repo, username, token, connection, trackerNS string, keys []string, issueRef string) *CodeHost {
	cfg := map[string]any{"username": username}
	if username == "" {
		cfg = map[string]any{"auth": "bearer"}
	}
	if connection == "" {
		connection = "bitbucket"
	}
	return &CodeHost{Type: "bitbucket", Name: "code", Connection: connection, ConnectionConfig: cfg, Secrets: map[string]string{"token": token},
		Entry: map[string]any{"namespace": "bitbucket/" + strings.ToLower(repo), "repo": repo, "issue_ref": issueRef,
			"tracker": map[string]any{"namespace": trackerNS, "keys": keys}},
		Operations: BitbucketOperations}
}

// ScopeConfig is the scope JSON.
func (g ScopeSpec) ScopeConfig() map[string]any {
	ops := AgentOperations(g.Type)
	connections := []string{g.Connection}
	if g.CodeHost != nil {
		ops = append(ops, g.CodeHost.Operations...)
		connections = append(connections, g.CodeHost.Connection)
	}
	authOps := append([]string{"emit"}, ops...)
	requires := obj("toolbelt", "mcp", "inject", true, "resume", true, "workspace", g.Checkout != "")
	agent := obj("name", "agent", "executor", "claude-code", "connection", g.Connection, "requires", requires,
		"operations", ops, "secret_names", []string{},
		"config", obj("status_schema", StatusSchema(g.Type), "playbook", Playbook, "args", "{key}", "branch", "agent/{key}"))
	if g.Checkout != "" {
		agent["resource"] = obj("provider", "git", "config", obj("checkout", g.Checkout, "mode", "work"))
	}
	entry := obj("namespace", g.Namespace, "scope", g.Scope)
	for k, v := range g.Entry {
		entry[k] = v
	}
	name := g.Name
	if name == "" {
		name = g.Scope
	}
	routes := []any{obj("name", "issues", "handler", g.Type, "binding", "agent", "active_policy", "inject", "config", obj())}
	extensions := []any{obj("name", g.Scope, "type", g.Type, "connection", g.Connection, "secret_names", []string{}, "config", entry)}
	if h := g.CodeHost; h != nil {
		code := obj("scope", g.Scope)
		for k, v := range h.Entry {
			code[k] = v
		}
		routes = append(routes, obj("name", "prs", "handler", h.Type, "binding", "agent", "active_policy", "inject", "config", obj()))
		extensions = append(extensions, obj("name", h.Name, "type", h.Type, "connection", h.Connection, "secret_names", []string{}, "config", code))
	}
	return obj("id", g.Scope, "name", name,
		"authorization", obj("connections", connections, "read_scopes", []string{}, "operations", authOps),
		"bindings", []any{agent}, "routes", routes, "extensions", extensions, "secret_refs", obj())
}

// Result is the setup result.
func (g ScopeSpec) Result() (extension.SetupResult, error) {
	cfg := g.ConnectionConfig
	if cfg == nil {
		cfg = obj()
	}
	refs := obj()
	for k, v := range g.Secrets {
		refs[k] = v
	}
	conn, err := json.Marshal(obj("id", g.Connection, "type", g.Type, "config", cfg, "secret_refs", refs))
	if err != nil {
		return extension.SetupResult{}, err
	}
	scope, err := json.Marshal(g.ScopeConfig())
	if err != nil {
		return extension.SetupResult{}, err
	}
	connections := []json.RawMessage{conn}
	if h := g.CodeHost; h != nil {
		hrefs := obj()
		for k, v := range h.Secrets {
			hrefs[k] = v
		}
		hc, err := json.Marshal(obj("id", h.Connection, "type", h.Type, "config", h.ConnectionConfig, "secret_refs", hrefs))
		if err != nil {
			return extension.SetupResult{}, err
		}
		connections = append(connections, hc)
	}
	workspace := "none (workspace-free; turns run in a private directory)"
	if g.Checkout != "" {
		workspace = g.Checkout + " (a worktree per issue)"
	}
	summary := append([]string{"scope      " + g.Scope, "namespace  " + g.Namespace}, g.Summary...)
	for name, ref := range g.Secrets {
		scheme, _, _ := strings.Cut(ref, ":")
		summary = append(summary, fmt.Sprintf("credential %s connection, %s %s:…", g.Connection, name, scheme))
	}
	summary = append(summary, "workspace  "+workspace)
	if h := g.CodeHost; h != nil {
		summary = append(summary, fmt.Sprintf("code host  %s %v (%s connection)", h.Type, h.Entry["repo"], h.Connection))
	}
	return extension.SetupResult{
		Connections: connections,
		Scope:       scope,
		Summary:     summary,
		Next: []string{
			"open-conduit serve                 start the daemon (reconcile every 15 minutes; add --webhook-addr for webhooks)",
			"open-conduit doctor --check-secrets  check the credential privately and " + g.Type + " access",
			"open-conduit tui                   watch it work"},
	}, nil
}

// SetupUsage wraps an invalid-argument refusal for the host.
func SetupUsage(msg string) error { return fmt.Errorf("%w: %s", extension.ErrSetupUsage, msg) }
