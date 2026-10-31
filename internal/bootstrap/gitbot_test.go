package bootstrap

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/internal/config"
	"github.com/open-conduit/open-conduit/internal/runtime"
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
	binding := func(name, checkout string) config.Binding {
		res, _ := json.Marshal(map[string]any{"checkout": checkout})
		return config.Binding{Name: name, Executor: ClaudeCodeExecutor, Config: json.RawMessage(`{}`), Resource: &config.Resource{Provider: "git", Config: res}}
	}
	snap := func(git string, bs ...config.Binding) *config.Snapshot {
		conn, _ := json.Marshal(map[string]any{"auth": "app", "app_id": "42", "git": git})
		return &config.Snapshot{Order: []domain.ID{"eng"}, Connections: []config.Connection{{ID: "gh", Type: "github", Config: conn}},
			Scopes: map[domain.ID]*config.EffectiveScope{"eng": {ID: "eng", Enabled: true, Bindings: bs, Extensions: []config.Extension{{Name: "widgets", Type: "github", Connection: "gh"}}}}}
	}
	if c := gitBotCheck(context.Background(), snap("owner", binding("mine", "ssh")), root); c.Code != "not_configured" {
		t.Errorf("without git bot: %+v", c)
	}
	c := gitBotCheck(context.Background(), snap("bot", binding("ok", "https")), root)
	if c.Status != runtime.CheckOK || len(c.Items) != 1 || !strings.Contains(c.Items[0], "eng/ok") {
		t.Errorf("an https remote: %+v", c)
	}
	c = gitBotCheck(context.Background(), snap("bot", binding("ok", "https"), binding("ssh", filepath.Join(root, "ssh"))), "")
	if c.Status != runtime.CheckWarn || c.Code != "ssh_remote" || !strings.Contains(strings.Join(c.Items, "\n"), "eng/ssh: remote origin is SSH") {
		t.Errorf("an SSH remote: %+v", c)
	}
}
