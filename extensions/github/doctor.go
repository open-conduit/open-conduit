package github

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/extension/inproc"
	"github.com/open-conduit/open-conduit/protocol"
)

// The configuration checks: what the validator accepts but leaves GitHub
// work unable to proceed (github_capability), and bot pushes that would
// still use the operator's SSH key (git_bot). Both read only the host's
// view of the configuration and the machine, so doctor runs them without a
// daemon too.

// agentExecutor is the default executor whose bindings run the work-issue
// playbook; AgentExecutors are all that do.
const agentExecutor = "claude-code"

// AgentExecutors are the coding-agent executors an agent binding can name,
// the default first: Claude Code (in the daemon) and the Codex CLI (the
// bundled codex package).
var AgentExecutors = []string{agentExecutor, "codex"}

// capabilityCheck reports, for each scope a GitHub entry claims issues for:
// no `github` route, an agent binding without the GitHub tools, scheduled
// issues with no hold route, and a hold route after the github route.
func capabilityCheck(_ context.Context, cfg protocol.CheckConfig) inproc.CheckResult {
	check := inproc.CheckResult{Name: "github_capability", Status: inproc.CheckOK, Code: "ok"}
	warn := func(code, item string) {
		if check.Status == inproc.CheckOK {
			check.Status, check.Code = inproc.CheckWarn, code
		}
		check.Items = append(check.Items, item)
	}
	info := func(item string) { check.Items = append(check.Items, item) }
	any := false
	for _, scope := range cfg.Scopes {
		id := scope.ID
		owns := false
		for _, x := range scope.Entries {
			if s, err := ParseExtension(x.Config, x.SecretNames); x.Type == Type && err == nil && s.Scope == id {
				owns = true
			}
		}
		if !owns {
			continue
		}
		any = true
		bindings := map[string]protocol.CheckBinding{}
		for _, b := range scope.Bindings {
			bindings[b.Name] = b
		}
		githubAt, holdAt := -1, -1
		for i, r := range scope.Routes {
			switch r.Handler {
			case Type:
				if githubAt < 0 {
					githubAt = i
				}
				b := bindings[r.Binding]
				if b.Toolbelt == extension.ToolbeltNone {
					warn("no_toolbelt", fmt.Sprintf("%s: route %s binds %s, whose executor has no toolbelt to report status", id, r.Name, r.Binding))
				}
				var missing []string
				for _, op := range []string{OpContext, "declare_outcome"} {
					if !slices.Contains(b.Operations, op) {
						missing = append(missing, op)
					}
				}
				if len(missing) > 0 && slices.Contains(AgentExecutors, b.Executor) {
					warn("agent_operations_missing", fmt.Sprintf("%s: binding %s lacks %s (the work-issue playbook needs them)", id, r.Binding, strings.Join(missing, ", ")))
				}
			case HoldHandler:
				if holdAt < 0 {
					holdAt = i
				}
				if bindings[r.Binding].Executor != HoldExecutor {
					warn("hold_binding", fmt.Sprintf("%s: route %s must bind a github-hold executor", id, r.Name))
				}
			}
		}
		switch {
		case githubAt < 0:
			warn("no_github_route", fmt.Sprintf("%s: no route uses handler github, so issues are never claimed", id))
		case holdAt < 0:
			info(fmt.Sprintf("%s: no github-hold route; issues labelled scheduled are not claimed", id))
		case holdAt > githubAt:
			warn("hold_route_order", fmt.Sprintf("%s: the github-hold route must come before the github route", id))
		default:
			info(fmt.Sprintf("%s: claims issues; scheduled issues are held", id))
		}
	}
	if !any {
		check.Status, check.Code = inproc.CheckInfo, "not_configured"
	}
	sort.Strings(check.Items)
	return check
}

// scpRemote is git's scp-like SSH syntax, [user@]host:path.
var scpRemote = regexp.MustCompile(`^(?:[^@/:]+@)?[^/:]+:`)

// sshRemote reports whether a remote URL is fetched and pushed over SSH,
// with the operator's own key rather than a credential helper.
func sshRemote(url string) bool {
	url = strings.TrimSpace(url)
	switch {
	case strings.HasPrefix(url, "ssh://"), strings.HasPrefix(url, "git+ssh://"), strings.HasPrefix(url, "ssh+git://"):
		return true
	case strings.Contains(url, "://"):
		return false
	}
	return scpRemote.MatchString(url)
}

// gitBotScope reports whether a scope's runs push and commit as a GitHub
// App: one of its GitHub entries is on a connection with git "bot".
func gitBotScope(cfg protocol.CheckConfig, scope protocol.CheckScope) bool {
	for _, x := range scope.Entries {
		if x.Type != Type {
			continue
		}
		for _, c := range cfg.Connections {
			if c.ID == x.Connection && c.Type == Type {
				if conn, err := ParseConnection(c.Config); err == nil && conn.Git == GitBot {
					return true
				}
			}
		}
	}
	return false
}

// gitWorkspace is the part of a `git` workspace's config the check reads.
type gitWorkspace struct {
	Checkout string `json:"checkout"`
	Remote   string `json:"remote"`
}

// gitBotCheck warns about every git workspace binding in a scope whose
// GitHub connection sets git "bot" when the binding isn't granted
// github.git.credential, the only credential helper git is left with, so
// every push is refused; or when the workspace's push remote is SSH: only
// HTTPS goes through the toolbelt's credential helper, so its pushes would
// still use the operator's own SSH key. Relative checkouts resolve
// against cfg.Dir, the config file's directory.
func gitBotCheck(ctx context.Context, cfg protocol.CheckConfig) inproc.CheckResult {
	check := inproc.CheckResult{Name: "git_bot", Status: inproc.CheckOK, Code: "ok"}
	n := 0
	for _, scope := range cfg.Scopes {
		if !gitBotScope(cfg, scope) {
			continue
		}
		for _, b := range scope.Bindings {
			if b.Resource == nil || b.Resource.Provider != "git" {
				continue
			}
			var res gitWorkspace
			if json.Unmarshal(b.Resource.Config, &res) != nil || res.Checkout == "" {
				continue
			}
			if res.Remote == "" {
				res.Remote = "origin"
			}
			n++
			name := scope.ID + "/" + b.Name
			if !slices.Contains(b.Operations, OpGitCredential) {
				// Every push fails: git's only helper is refused.
				check.Status, check.Code = inproc.CheckWarn, "git_credential_not_granted"
				check.Items = append(check.Items, fmt.Sprintf("%s: not granted %s, so every push is refused; add it to the binding's operations and the scope's authorization.operations", name, OpGitCredential))
				continue
			}
			dir := res.Checkout
			if !filepath.IsAbs(dir) && cfg.Dir != "" {
				dir = filepath.Join(cfg.Dir, dir)
			}
			cx, cancel := context.WithTimeout(ctx, 10*time.Second)
			out, err := exec.CommandContext(cx, "git", "-C", dir, "remote", "get-url", "--push", res.Remote).Output()
			cancel()
			switch {
			case err != nil:
				check.Items = append(check.Items, fmt.Sprintf("%s: remote %s not readable", name, res.Remote))
			case sshRemote(string(out)):
				if check.Code != "git_credential_not_granted" {
					check.Code = "ssh_remote"
				}
				check.Status = inproc.CheckWarn
				check.Items = append(check.Items, fmt.Sprintf("%s: remote %s is SSH, so pushes use your own SSH key, not the bot; use its https:// URL", name, res.Remote))
			default:
				check.Items = append(check.Items, fmt.Sprintf("%s: pushes and commits as the GitHub App", name))
			}
		}
	}
	if n == 0 {
		check.Status, check.Code, check.Detail = inproc.CheckInfo, "not_configured", `no git workspace binding is in a scope whose GitHub connection sets git "bot"`
	}
	sort.Strings(check.Items)
	return check
}
