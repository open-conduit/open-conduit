package github

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/open-conduit/open-conduit/extension/inproc"
	"github.com/open-conduit/open-conduit/protocol"
)

func TestSSHRemote(t *testing.T) {
	for url, want := range map[string]bool{
		"git@github.com:octo/widgets.git":         true,
		"github.com:octo/widgets.git":             true,
		"ssh://git@github.com/octo/widgets.git":   true,
		"git+ssh://github.com/octo/widgets.git":   true,
		"https://github.com/octo/widgets.git":     false,
		"https://x@github.com/octo/widgets.git\n": false,
		"/srv/git/widgets.git":                    false,
		"file:///srv/git/widgets.git":             false,
	} {
		if got := sshRemote(url); got != want {
			t.Errorf("%q: ssh %v, want %v", url, got, want)
		}
	}
}

// TestCapabilityCheck: for a scope a GitHub entry claims issues for, the
// check warns about a missing github route, a route binding an executor
// with no toolbelt, an agent binding without the GitHub tools, a hold
// route on the wrong executor or after the github route, and says nothing
// of scopes without an entry.
func TestCapabilityCheck(t *testing.T) {
	entry := protocol.CheckEntry{Name: "widgets", Type: Type, Config: json.RawMessage(`{"namespace":"github/w","scope":"eng","repo":"octo/widgets"}`)}
	agent := protocol.CheckBinding{Name: "agent", Executor: agentExecutor, Toolbelt: "mcp", Operations: []string{OpContext, "declare_outcome"}}
	hold := protocol.CheckBinding{Name: "hold", Executor: HoldExecutor, Toolbelt: "none"}
	holdRoute := protocol.CheckRoute{Name: "held", Handler: HoldHandler, Binding: "hold"}
	githubRoute := protocol.CheckRoute{Name: "issues", Handler: Type, Binding: "agent"}
	scope := func(routes []protocol.CheckRoute, bindings ...protocol.CheckBinding) protocol.CheckConfig {
		return protocol.CheckConfig{Scopes: []protocol.CheckScope{{ID: "eng", Entries: []protocol.CheckEntry{entry}, Routes: routes, Bindings: bindings}}}
	}
	for _, tc := range []struct {
		name, status, code string
		cfg                protocol.CheckConfig
	}{
		{"no entry", inproc.CheckInfo, "not_configured", protocol.CheckConfig{Scopes: []protocol.CheckScope{{ID: "eng", Routes: []protocol.CheckRoute{githubRoute}, Bindings: []protocol.CheckBinding{agent}}}}},
		{"held and claimed", inproc.CheckOK, "ok", scope([]protocol.CheckRoute{holdRoute, githubRoute}, agent, hold)},
		{"no github route", inproc.CheckWarn, "no_github_route", scope(nil, agent)},
		{"no toolbelt", inproc.CheckWarn, "no_toolbelt", scope([]protocol.CheckRoute{githubRoute}, protocol.CheckBinding{Name: "agent", Executor: "shell", Toolbelt: "none"})},
		{"operations missing", inproc.CheckWarn, "agent_operations_missing", scope([]protocol.CheckRoute{githubRoute}, protocol.CheckBinding{Name: "agent", Executor: agentExecutor, Toolbelt: "mcp"})},
		{"hold binding", inproc.CheckWarn, "hold_binding", scope([]protocol.CheckRoute{{Name: "held", Handler: HoldHandler, Binding: "agent"}, githubRoute}, agent)},
		{"hold after github", inproc.CheckWarn, "hold_route_order", scope([]protocol.CheckRoute{githubRoute, holdRoute}, agent, hold)},
	} {
		if c := capabilityCheck(context.Background(), tc.cfg); c.Name != "github_capability" || c.Status != tc.status || c.Code != tc.code {
			t.Errorf("%s: %+v", tc.name, c)
		}
	}
	if c := capabilityCheck(context.Background(), scope([]protocol.CheckRoute{githubRoute}, agent)); len(c.Items) != 1 || !strings.Contains(c.Items[0], "no github-hold route") {
		t.Errorf("no hold route: %+v", c)
	}
}

// TestGitBotCheck: doctor warns about a git workspace binding in a scope
// whose GitHub connection sets git "bot" when its checkout pushes over
// SSH, where the operator's own key would push, and says nothing of scopes
// that leave git as the owner's.
func TestGitBotCheck(t *testing.T) {
	root := t.TempDir()
	repo := func(name, url string) {
		dir := filepath.Join(root, name)
		_ = os.MkdirAll(dir, 0o700)
		for _, args := range [][]string{{"init", "-q"}, {"remote", "add", "origin", url}} {
			cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
			cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("git %v: %v %s", args, err, out)
			}
		}
	}
	repo("ssh", "git@github.com:octo/widgets.git")
	repo("https", "https://github.com/octo/widgets.git")
	binding := func(name, checkout string) protocol.CheckBinding {
		res, _ := json.Marshal(map[string]any{"checkout": checkout})
		return protocol.CheckBinding{Name: name, Executor: agentExecutor, Resource: &protocol.CheckResource{Provider: "git", Config: res}}
	}
	view := func(dir, git string, bs ...protocol.CheckBinding) protocol.CheckConfig {
		conn, _ := json.Marshal(map[string]any{"auth": "app", "app_id": "42", "git": git})
		return protocol.CheckConfig{Dir: dir, Connections: []protocol.CheckConnection{{ID: "gh", Type: Type, Config: conn}},
			Scopes: []protocol.CheckScope{{ID: "eng", Bindings: bs, Entries: []protocol.CheckEntry{{Name: "widgets", Type: Type, Connection: "gh"}}}}}
	}
	if c := gitBotCheck(context.Background(), protocol.CheckConfig{}); c.Name != "git_bot" || c.Code != "not_configured" {
		t.Errorf("no configuration: %+v", c)
	}
	if c := gitBotCheck(context.Background(), view(root, "owner", binding("mine", "ssh"))); c.Code != "not_configured" {
		t.Errorf("without git bot: %+v", c)
	}
	c := gitBotCheck(context.Background(), view(root, "bot", binding("ok", "https")))
	if c.Status != inproc.CheckOK || len(c.Items) != 1 || !strings.Contains(c.Items[0], "eng/ok") {
		t.Errorf("an https remote: %+v", c)
	}
	c = gitBotCheck(context.Background(), view("", "bot", binding("ok", "https"), binding("ssh", filepath.Join(root, "ssh"))))
	if c.Status != inproc.CheckWarn || c.Code != "ssh_remote" || !strings.Contains(strings.Join(c.Items, "\n"), "eng/ssh: remote origin is SSH") {
		t.Errorf("an SSH remote: %+v", c)
	}
}
