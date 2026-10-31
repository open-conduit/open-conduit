package bootstrap

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	gitws "github.com/open-conduit/open-conduit/extensions/git"
	gh "github.com/open-conduit/open-conduit/extensions/github"
	"github.com/open-conduit/open-conduit/internal/config"
	"github.com/open-conduit/open-conduit/internal/runtime"
)

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
func gitBotScope(snapshot *config.Snapshot, scope *config.EffectiveScope) bool {
	for _, x := range scope.Extensions {
		if x.Type != gh.Type {
			continue
		}
		for _, c := range snapshot.Connections {
			if c.ID == x.Connection && c.Type == gh.Type {
				if cfg, err := gh.ParseConnection(c.Config); err == nil && cfg.Git == gh.GitBot {
					return true
				}
			}
		}
	}
	return false
}

// gitBotCheck warns about every git workspace binding in a scope whose
// GitHub connection sets git "bot" when the workspace's push remote is SSH:
// only HTTPS goes through the toolbelt's credential helper, so its pushes
// would still use the operator's own SSH key. Relative checkouts resolve
// against base, the config file's directory.
func gitBotCheck(ctx context.Context, snapshot *config.Snapshot, base string) runtime.Check {
	check := runtime.Check{Name: "git_bot", Status: runtime.CheckOK, Code: "ok"}
	if snapshot == nil {
		check.Status, check.Code = runtime.CheckInfo, "not_configured"
		return check
	}
	n := 0
	for _, id := range snapshot.Order {
		scope := snapshot.Scopes[id]
		if scope == nil || !scope.Enabled || !gitBotScope(snapshot, scope) {
			continue
		}
		for _, b := range scope.Bindings {
			if b.Resource == nil || b.Resource.Provider != gitws.Type {
				continue
			}
			res, err := gitws.ParseConfig(b.Resource.Config)
			if err != nil {
				continue
			}
			n++
			name := string(id) + "/" + b.Name
			dir := res.Checkout
			if !filepath.IsAbs(dir) && base != "" {
				dir = filepath.Join(base, dir)
			}
			cx, cancel := context.WithTimeout(ctx, 10*time.Second)
			out, err := exec.CommandContext(cx, "git", "-C", dir, "remote", "get-url", "--push", res.Remote).Output()
			cancel()
			switch {
			case err != nil:
				check.Items = append(check.Items, fmt.Sprintf("%s: remote %s not readable", name, res.Remote))
			case sshRemote(string(out)):
				check.Status, check.Code = runtime.CheckWarn, "ssh_remote"
				check.Items = append(check.Items, fmt.Sprintf("%s: remote %s is SSH, so pushes use your own SSH key, not the bot; use its https:// URL", name, res.Remote))
			default:
				check.Items = append(check.Items, fmt.Sprintf("%s: pushes and commits as the GitHub App", name))
			}
		}
	}
	if n == 0 {
		check.Status, check.Code, check.Detail = runtime.CheckInfo, "not_configured", `no git workspace binding is in a scope whose GitHub connection sets git "bot"`
	}
	sort.Strings(check.Items)
	return check
}

func (d *daemon) gitBotCheck(ctx context.Context) runtime.Check {
	snapshot, _ := d.configs.Current()
	return gitBotCheck(ctx, snapshot, filepath.Dir(d.o.ConfigPath))
}
