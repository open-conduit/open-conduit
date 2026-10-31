package claudecode_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/executors/claudecode"
	"github.com/open-conduit/open-conduit/extension"
)

// fakeContainer runs a launch on the host as planned, with its secrets in
// the environment as the container's entry stub would give them, and keeps
// what it was asked.
type fakeContainer struct {
	needs    []extension.SandboxNeeds
	launches []extension.SandboxLaunch
}

func (b *fakeContainer) Launch(_ context.Context, n extension.SandboxNeeds, l extension.SandboxLaunch) (extension.SandboxPlan, error) {
	b.needs, b.launches = append(b.needs, n), append(b.launches, l)
	return extension.SandboxPlan{Argv: l.Argv, Env: append(append([]string{}, l.Env...), l.Secrets...), Digest: "digest", Signals: extension.SandboxTerm}, nil
}
func (b *fakeContainer) Adopt(context.Context, bool) (extension.ContainerExit, error) {
	return extension.ContainerExit{}, nil
}

func envMap(env []string) map[string]string {
	m := map[string]string{}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		m[k] = v
	}
	return m
}

// TestContainerLaunch: a container launch keeps claude's config in the
// Work's home (HOME and CLAUDE_CONFIG_DIR), seeded on first use from the
// binding's home_seed without its credentials, turns off nonessential
// traffic, passes the sign-in and a GitHub token as secrets (never in the
// environment, git reaching github.com through a helper), points the MCP
// server at the container's own os, and resumes the session in the home
// on the next turn.
func TestContainerLaunch(t *testing.T) {
	h := newHarness(t)
	e := h.executor()
	home := filepath.Join(h.root, "homes", "w")
	seed := filepath.Join(h.root, "seed")
	for _, d := range []string{home, filepath.Join(seed, "plugins")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for name, body := range map[string]string{"settings.json": `{"theme":"dark"}`, "plugins/p.json": "{}", ".credentials.json": "secret"} {
		if err := os.WriteFile(filepath.Join(seed, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	b := &fakeContainer{}
	extra := map[string]any{"home_seed": seed, "secret_env": map[string]string{"claude": "CLAUDE_CODE_OAUTH_TOKEN", "github": "GH_TOKEN"}}
	turn := func(execution string) extension.Result {
		r, _ := h.request("work/c", execution, 1, extra)
		r.Context.Container, r.Context.ContainerHome = b, home
		r.Context.Secrets = map[string]string{"claude": "oauth-token", "github": "gh-token", "other": "x"}
		r.Context.ToolbeltToken, r.Context.ToolbeltEndpoint = "belt", "unix:///run/belt.sock"
		return h.run(e, r)
	}
	if res := turn("exec-1"); res.Outcome.Kind == domain.OutcomeFailed {
		t.Fatalf("container turn failed: %+v", res.Outcome)
	}
	l := b.launches[0]
	env := envMap(l.Env)
	for k, want := range map[string]string{"HOME": home, "CLAUDE_CONFIG_DIR": home, "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1", "GIT_CONFIG_KEY_0": "credential.https://github.com.helper", "OS_ENDPOINT": "unix:///run/belt.sock"} {
		if env[k] != want {
			t.Errorf("%s=%q, want %q", k, env[k], want)
		}
	}
	for _, k := range []string{"CLAUDE_CODE_OAUTH_TOKEN", "GH_TOKEN", "OTHER", "ANTHROPIC_API_KEY", "OS_TOKEN", "TMPDIR"} {
		if _, ok := env[k]; ok {
			t.Errorf("%s is in the container's environment", k)
		}
	}
	if !slices.Equal(l.Secrets, []string{"CLAUDE_CODE_OAUTH_TOKEN=oauth-token", "GH_TOKEN=gh-token", "OTHER=x"}) {
		t.Errorf("secrets %v", l.Secrets)
	}
	if !slices.Contains(b.needs[0].Write, home) || !slices.Contains(b.needs[0].Hosts, "api.anthropic.com") {
		t.Errorf("needs %+v", b.needs[0])
	}
	for name, want := range map[string]bool{"settings.json": true, "plugins/p.json": true, ".claude.json": true, ".credentials.json": false} {
		if _, err := os.Stat(filepath.Join(home, name)); (err == nil) != want {
			t.Errorf("home %s: present %v, want %v", name, err == nil, want)
		}
	}
	var mcp struct {
		MCPServers map[string]map[string]any `json:"mcpServers"`
	}
	raw, _ := os.ReadFile(filepath.Join(filepath.Dir(l.RunDir), filepath.Base(l.RunDir), "mcp.json"))
	if err := json.Unmarshal(raw, &mcp); err != nil || mcp.MCPServers[claudecode.MCPServerName]["command"] != "os" || mcp.MCPServers[claudecode.MCPServerName]["env"] != nil {
		t.Errorf("mcp.json %s (%v)", raw, err)
	}

	// The next turn resumes the session in the home; the seed is not copied
	// over what the agent made of it.
	_ = os.WriteFile(filepath.Join(home, "settings.json"), []byte(`{"theme":"light"}`), 0o600)
	if res := turn("exec-2"); res.Outcome.Kind == domain.OutcomeFailed {
		t.Fatalf("second turn failed: %+v", res.Outcome)
	}
	if inv := h.invocations(); len(inv) != 2 || !strings.Contains(inv[1], "resume=1") {
		t.Errorf("invocations %v, want the second to resume", inv)
	}
	if raw, _ := os.ReadFile(filepath.Join(home, "settings.json")); string(raw) != `{"theme":"light"}` {
		t.Errorf("the seed was copied again: %s", raw)
	}

	// Without a static GitHub token, git's helper is the toolbelt's os,
	// which asks the host for a token minted for the Work's repository.
	r, _ := h.request("work/c", "exec-4", 1, map[string]any{"home_seed": seed, "secret_env": map[string]string{"claude": "CLAUDE_CODE_OAUTH_TOKEN"}})
	r.Context.Container, r.Context.ContainerHome = b, home
	r.Context.Secrets = map[string]string{"claude": "oauth-token"}
	r.Context.ToolbeltToken, r.Context.ToolbeltEndpoint = "belt", "unix:///run/belt.sock"
	if res := h.run(e, r); res.Outcome.Kind == domain.OutcomeFailed {
		t.Fatalf("container turn failed: %+v", res.Outcome)
	}
	env = envMap(b.launches[len(b.launches)-1].Env)
	if env["GIT_CONFIG_COUNT"] != "1" || env["GIT_CONFIG_KEY_0"] != "credential.helper" || env["GIT_CONFIG_VALUE_0"] != "!os git-credential" {
		t.Errorf("git's helper without a static token: %q %q %q", env["GIT_CONFIG_COUNT"], env["GIT_CONFIG_KEY_0"], env["GIT_CONFIG_VALUE_0"])
	}
	if l := b.launches[len(b.launches)-1]; slices.ContainsFunc(l.Secrets, func(kv string) bool { return strings.HasPrefix(kv, "GH_TOKEN=") }) {
		t.Errorf("a GitHub token crossed in: %v", l.Secrets)
	}

	// Without a home from the host, nothing launches.
	r, _ = h.request("work/d", "exec-3", 1, nil)
	r.Context.Container = b
	if res := h.run(e, r); res.Outcome.Kind != domain.OutcomeFailed || dataOf(t, res).Reason != "sandbox_unavailable" {
		t.Errorf("no home: %+v", res.Outcome)
	}
}

// TestContainerAttach: a session that ran in a container attaches in one:
// Attach returns `container attach`, and ContainerAttach is an interactive
// resume of the session in the Work's home and workspace, with the sign-in
// as a secret and no toolbelt. A session that ran on the host has none.
func TestContainerAttach(t *testing.T) {
	h := newHarness(t)
	e := h.executor()
	home := filepath.Join(h.root, "homes", "w")
	_ = os.MkdirAll(home, 0o700)
	extra := map[string]any{"secret_env": map[string]string{"claude": "CLAUDE_CODE_OAUTH_TOKEN"}}
	r, _ := h.request("work/c", "exec-1", 1, extra)
	r.Context.Container, r.Context.ContainerHome = &fakeContainer{}, home
	if res := h.run(e, r); res.Outcome.Kind == domain.OutcomeFailed {
		t.Fatalf("container turn failed: %+v", res.Outcome)
	}
	spec, err := e.Attach(context.Background(), "work/c")
	if err != nil || len(spec.Argv) != 4 || spec.Argv[1] != "container" || spec.Argv[2] != "attach" || spec.Argv[3] != "work/c" {
		t.Fatalf("attach %+v %v", spec, err)
	}
	session, _ := h.store.record(t, "work/c")["session"].(string)
	a, err := e.ContainerAttach(context.Background(), extension.ContainerAttachRequest{Work: r.Work, Config: r.Context.Config, Secrets: map[string]string{"claude": "tok"}, Home: home})
	if err != nil {
		t.Fatal(err)
	}
	l := a.Launch
	if !l.TTY || l.Argv[0] != h.binary || l.Argv[1] != "--resume" || l.Argv[2] != session || !slices.Equal(l.Secrets, []string{"CLAUDE_CODE_OAUTH_TOKEN=tok"}) {
		t.Errorf("launch %+v", l)
	}
	if env := envMap(l.Env); env["HOME"] != home || env["CLAUDE_CONFIG_DIR"] != home || env["OS_ENDPOINT"] != "" {
		t.Errorf("env %v", l.Env)
	}
	if !slices.Contains(a.Needs.Write, home) || !slices.Contains(a.Needs.Write, l.Dir) || len(a.Needs.Sockets) != 0 {
		t.Errorf("needs %+v", a.Needs)
	}

	r, _ = h.request("work/host", "exec-2", 1, nil)
	if res := h.run(e, r); res.Outcome.Kind == domain.OutcomeFailed {
		t.Fatalf("host turn failed: %+v", res.Outcome)
	}
	if _, err := e.ContainerAttach(context.Background(), extension.ContainerAttachRequest{Work: r.Work, Config: r.Context.Config, Home: home}); !errors.Is(err, extension.ErrNothingToAttach) {
		t.Errorf("a host session attached in a container: %v", err)
	}
}

// gitEntries are env's GIT_CONFIG entries in order, as key=value.
func gitEntries(env map[string]string) []string {
	var out []string
	n := 0
	for k := range env {
		if strings.HasPrefix(k, "GIT_CONFIG_KEY_") {
			n++
		}
	}
	for i := 0; i < n; i++ {
		out = append(out, env["GIT_CONFIG_KEY_"+strconv.Itoa(i)]+"="+env["GIT_CONFIG_VALUE_"+strconv.Itoa(i)])
	}
	if env["GIT_CONFIG_COUNT"] != strconv.Itoa(n) {
		out = append(out, "COUNT="+env["GIT_CONFIG_COUNT"])
	}
	return out
}

// signingWorkspace is a resource whose env carries the git provider's
// signing policy, as a workspace without `sign` does.
type signingWorkspace struct{ dir string }

func (w signingWorkspace) Acquire(context.Context, extension.ResourceRequest) (extension.Resource, error) {
	return extension.Resource{Kind: "workspace", Ref: "lease", Dir: w.dir, Env: map[string]string{"GIT_CONFIG_COUNT": "2",
		"GIT_CONFIG_KEY_0": "commit.gpgsign", "GIT_CONFIG_VALUE_0": "false", "GIT_CONFIG_KEY_1": "tag.gpgsign", "GIT_CONFIG_VALUE_1": "false"}}, nil
}
func (w signingWorkspace) Lookup(ctx context.Context, _ string) (extension.Resource, error) {
	return w.Acquire(ctx, extension.ResourceRequest{})
}

// TestContainerGitHelperAfterSigning: the toolbelt's helper goes after the
// workspace's signing policy rather than giving way to it.
func TestContainerGitHelperAfterSigning(t *testing.T) {
	h := newHarness(t)
	e := h.executor()
	home, ws := filepath.Join(h.root, "homes", "w"), filepath.Join(h.root, "ws")
	_ = os.MkdirAll(home, 0o700)
	_ = os.MkdirAll(ws, 0o700)
	b := &fakeContainer{}
	r, _ := h.request("work/s", "exec-s", 1, nil)
	r.Context.Container, r.Context.ContainerHome, r.Context.Resources = b, home, signingWorkspace{ws}
	r.Context.ToolbeltToken, r.Context.ToolbeltEndpoint = "belt", "unix:///run/belt.sock"
	if res := h.run(e, r); res.Outcome.Kind == domain.OutcomeFailed {
		t.Fatalf("container turn failed: %+v", res.Outcome)
	}
	want := []string{"commit.gpgsign=false", "tag.gpgsign=false", "credential.helper=!os git-credential"}
	if got := gitEntries(envMap(b.launches[0].Env)); !slices.Equal(got, want) {
		t.Errorf("git config %q, want %q", got, want)
	}
}

// botLayer is a run environment layer like the GitHub extension's for git
// "bot": the toolbelt as git's only helper and a bot identity.
var botLayer = map[string]string{"GIT_CONFIG_COUNT": "2",
	"GIT_CONFIG_KEY_0": "credential.helper", "GIT_CONFIG_VALUE_0": "",
	"GIT_CONFIG_KEY_1": "credential.helper", "GIT_CONFIG_VALUE_1": "!os git-credential",
	"GIT_AUTHOR_NAME": "acme[bot]", "OS_ENDPOINT": "planted"}

// TestRunEnvironmentLayer: the extensions' run layer (ExecutionContext.Env)
// goes after the workspace's, its git config after the signing policy, and
// a toolbelt variable in it is dropped. A container's credential helper
// then gives way to the layer's. On the host, a run with a layer gets the
// toolbelt client (os on PATH, the endpoint and token file) so the layer's
// `!os git-credential` works; a run without one still gets neither.
func TestRunEnvironmentLayer(t *testing.T) {
	h := newHarness(t)
	e := h.executor()
	home, ws := filepath.Join(h.root, "homes", "w"), filepath.Join(h.root, "ws")
	_ = os.MkdirAll(home, 0o700)
	_ = os.MkdirAll(ws, 0o700)
	b := &fakeContainer{}
	r, _ := h.request("work/b", "exec-b", 1, nil)
	r.Context.Container, r.Context.ContainerHome, r.Context.Resources = b, home, signingWorkspace{ws}
	r.Context.ToolbeltToken, r.Context.ToolbeltEndpoint = "belt", "unix:///run/belt.sock"
	r.Context.Env = botLayer
	if res := h.run(e, r); res.Outcome.Kind == domain.OutcomeFailed {
		t.Fatalf("container turn failed: %+v", res.Outcome)
	}
	env := envMap(b.launches[0].Env)
	want := []string{"commit.gpgsign=false", "tag.gpgsign=false", "credential.helper=", "credential.helper=!os git-credential"}
	if got := gitEntries(env); !slices.Equal(got, want) {
		t.Errorf("git config %q, want %q", got, want)
	}
	if env["GIT_AUTHOR_NAME"] != "acme[bot]" || env["OS_ENDPOINT"] != "unix:///run/belt.sock" {
		t.Errorf("author %q, endpoint %q", env["GIT_AUTHOR_NAME"], env["OS_ENDPOINT"])
	}

	for _, layer := range []map[string]string{botLayer, nil} {
		r, _ := h.request("work/h", "exec-h"+strconv.Itoa(len(layer)), 1, nil)
		r.Context.ToolbeltToken, r.Context.ToolbeltEndpoint = "belt", "unix:///run/belt.sock"
		r.Context.Env = layer
		if res := h.run(e, r); res.Outcome.Kind == domain.OutcomeFailed {
			t.Fatalf("host turn failed: %+v", res.Outcome)
		}
		raw, _ := os.ReadFile(filepath.Join(h.fake, "env"))
		got := envMap(strings.Split(strings.TrimSpace(string(raw)), "\n"))
		if layer == nil {
			if _, ok := got["OS_ENDPOINT"]; ok || got["GIT_AUTHOR_NAME"] != "" {
				t.Errorf("a run without a layer: endpoint %q, author %q", got["OS_ENDPOINT"], got["GIT_AUTHOR_NAME"])
			}
			continue
		}
		if got["GIT_AUTHOR_NAME"] != "acme[bot]" || got["OS_ENDPOINT"] != "unix:///run/belt.sock" || got["OS_TOKEN_FILE"] == "" || !strings.HasPrefix(got["PATH"], filepath.Join(h.root, "bin")+string(os.PathListSeparator)) {
			t.Errorf("host env: author %q, endpoint %q, token file %q, PATH %q", got["GIT_AUTHOR_NAME"], got["OS_ENDPOINT"], got["OS_TOKEN_FILE"], got["PATH"])
		}
		if gitEntries(got)[1] != "credential.helper=!os git-credential" {
			t.Errorf("host git config %q", gitEntries(got))
		}
		if _, ok := got["OS_TOKEN"]; ok {
			t.Error("the token itself reached claude's environment")
		}
	}
}
