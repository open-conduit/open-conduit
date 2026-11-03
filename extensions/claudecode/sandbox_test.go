package claudecode_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/extensions/claudecode"
)

// TestSandboxConfig: config load refuses a sandboxed binding whose claude
// config isn't its own (config.SandboxChecker): none set, a relative one,
// the operator's ~/.claude, a directory inside it, the home that holds it,
// and a link to it.
func TestSandboxConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	_ = os.MkdirAll(filepath.Join(home, ".claude"), 0o700)
	_ = os.Symlink(filepath.Join(home, ".claude"), filepath.Join(home, "linked"))
	own := filepath.Join(t.TempDir(), "claude")
	other := t.TempDir()
	e := &claudecode.Executor{}
	with := func(env map[string]string) json.RawMessage {
		b, _ := json.Marshal(map[string]any{"env": env})
		return b
	}
	if err := e.SandboxConfig(with(map[string]string{"CLAUDE_CONFIG_DIR": own})); err != nil {
		t.Fatalf("its own config directory refused: %v", err)
	}
	if err := e.SandboxConfig(with(map[string]string{"CLAUDE_CONFIG_DIR": filepath.Join(home, ".claude-sandboxed")})); err != nil {
		t.Fatalf("a sibling of ~/.claude refused: %v", err)
	}
	for name, env := range map[string]map[string]string{
		"unset":                        nil,
		"empty":                        {"CLAUDE_CONFIG_DIR": ""},
		"relative":                     {"CLAUDE_CONFIG_DIR": "claude"},
		"~/.claude":                    {"CLAUDE_CONFIG_DIR": filepath.Join(home, ".claude")},
		"inside":                       {"CLAUDE_CONFIG_DIR": filepath.Join(home, ".claude", "sandboxed")},
		"the home":                     {"CLAUDE_CONFIG_DIR": home},
		"a link":                       {"CLAUDE_CONFIG_DIR": filepath.Join(home, "linked")},
		"real home under another HOME": {"CLAUDE_CONFIG_DIR": filepath.Join(home, ".claude"), "HOME": t.TempDir()},
		"the other HOME's":             {"CLAUDE_CONFIG_DIR": filepath.Join(other, ".claude"), "HOME": other},
	} {
		if err := e.SandboxConfig(with(env)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// recordingBroker plans a launch unchanged and keeps the needs it got.
type recordingBroker struct{ needs []extension.SandboxNeeds }

func (b *recordingBroker) Plan(_ context.Context, needs extension.SandboxNeeds, l extension.SandboxLaunch) (extension.SandboxPlan, error) {
	b.needs = append(b.needs, needs)
	return extension.SandboxPlan{Argv: l.Argv, Env: l.Env, Digest: "digest", Signals: "term"}, nil
}
func (b *recordingBroker) Adopt(context.Context, int) error { return nil }

// TestSandboxedConfigDir: a sandboxed launch writes its own config
// directory and denies the operator's ~/.claude and ~/.claude.json; one
// whose resolved config directory is missing or is the operator's own is
// refused as sandbox_denied before the sandbox is asked for a plan.
func TestSandboxedConfigDir(t *testing.T) {
	h := newHarness(t)
	e := h.executor()
	b := &recordingBroker{}
	r, _ := h.request("work/a", "exec-a", 1, nil)
	r.Context.Sandbox = b
	if res := h.run(e, r); res.Outcome.Kind == domain.OutcomeFailed {
		t.Fatalf("sandboxed run with its own config failed: %+v", res.Outcome)
	}
	if len(b.needs) != 1 || !slices.Contains(b.needs[0].Write, h.config) ||
		!slices.Equal(b.needs[0].Deny, []string{filepath.Join(h.root, ".claude"), filepath.Join(h.root, ".claude.json")}) {
		t.Fatalf("needs %+v", b.needs)
	}
	_ = os.MkdirAll(filepath.Join(h.root, ".claude"), 0o700)
	_ = os.Symlink(filepath.Join(h.root, ".claude"), filepath.Join(h.root, "linked"))
	for i, dir := range []string{"", filepath.Join(h.root, ".claude"), filepath.Join(h.root, "linked")} {
		work := []string{"work/b", "work/c", "work/d"}[i]
		r, _ := h.request(work, "exec-"+work, 1, map[string]any{"env": map[string]string{"FAKE_DIR": h.fake, "CLAUDE_CONFIG_DIR": dir}})
		r.Context.Sandbox = b
		if res := h.run(e, r); res.Outcome.Kind != domain.OutcomeFailed || dataOf(t, res).Reason != "sandbox_denied" {
			t.Errorf("config dir %q: %+v", dir, res.Outcome)
		}
	}
	if len(b.needs) != 1 || len(h.invocations()) != 1 {
		t.Fatalf("a refused launch was planned or ran: %d plans, %v", len(b.needs), h.invocations())
	}

	// A binding env that sets HOME elsewhere still keeps the daemon's own
	// home's claude config out of reach, and its own HOME's too.
	other := filepath.Join(h.root, "other-home")
	_ = os.MkdirAll(other, 0o700)
	r, _ = h.request("work/e", "exec-e", 1, map[string]any{"env": map[string]string{"FAKE_DIR": h.fake, "HOME": other}})
	r.Context.Sandbox = b
	if res := h.run(e, r); res.Outcome.Kind == domain.OutcomeFailed {
		t.Fatalf("sandboxed run with HOME overridden failed: %+v", res.Outcome)
	}
	if want := []string{filepath.Join(h.root, ".claude"), filepath.Join(h.root, ".claude.json"), filepath.Join(other, ".claude"), filepath.Join(other, ".claude.json")}; len(b.needs) != 2 || !slices.Equal(b.needs[1].Deny, want) {
		t.Fatalf("deny with HOME overridden %+v, want %v", b.needs, want)
	}
	r, _ = h.request("work/f", "exec-f", 1, map[string]any{"env": map[string]string{"FAKE_DIR": h.fake, "HOME": other, "CLAUDE_CONFIG_DIR": filepath.Join(h.root, ".claude")}})
	r.Context.Sandbox = b
	if res := h.run(e, r); res.Outcome.Kind != domain.OutcomeFailed || dataOf(t, res).Reason != "sandbox_denied" {
		t.Errorf("the daemon's ~/.claude under another HOME: %+v", res.Outcome)
	}
}

// TestSandboxReadsToolbelt: the MCP server runs `os` from the host's
// toolbelt directory, which may sit in the state directory the sandbox
// hides, so a sandboxed launch with a toolbelt asks to read it. One without
// a toolbelt token doesn't, and nor does a container, which has its own `os`.
func TestSandboxReadsToolbelt(t *testing.T) {
	h := newHarness(t)
	e := h.executor()
	belt := filepath.Join(h.root, "bin")
	b := &recordingBroker{}
	r, _ := h.request("work/a", "exec-a", 1, nil)
	r.Context.Sandbox = b
	r.Context.ToolbeltToken, r.Context.ToolbeltEndpoint = "belt", "unix:///run/belt.sock"
	if res := h.run(e, r); res.Outcome.Kind == domain.OutcomeFailed {
		t.Fatalf("sandboxed run failed: %+v", res.Outcome)
	}
	if len(b.needs) != 1 || !slices.Contains(b.needs[0].Read, belt) {
		t.Fatalf("needs %+v, want read of %s", b.needs, belt)
	}
	r, _ = h.request("work/b", "exec-b", 1, nil)
	r.Context.Sandbox = b
	if res := h.run(e, r); res.Outcome.Kind == domain.OutcomeFailed {
		t.Fatalf("sandboxed run without a toolbelt failed: %+v", res.Outcome)
	}
	if len(b.needs) != 2 || slices.Contains(b.needs[1].Read, belt) {
		t.Fatalf("without a toolbelt, needs %+v", b.needs)
	}
	c := &fakeContainer{}
	home := filepath.Join(h.root, "home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	r, _ = h.request("work/c", "exec-c", 1, nil)
	r.Context.Container, r.Context.ContainerHome = c, home
	r.Context.ToolbeltToken, r.Context.ToolbeltEndpoint = "belt", "unix:///run/belt.sock"
	if res := h.run(e, r); res.Outcome.Kind == domain.OutcomeFailed {
		t.Fatalf("container run failed: %+v", res.Outcome)
	}
	if len(c.needs) != 1 || slices.Contains(c.needs[0].Read, belt) {
		t.Fatalf("container needs %+v, want no read of %s", c.needs, belt)
	}
}

// TestRunDirectoriesWin: the toolbelt and playbook directories the host
// names for a run win over the executor's own, so an executor out of
// process, away from the host's os, still reaches the host's.
func TestRunDirectoriesWin(t *testing.T) {
	h := newHarness(t)
	e := h.executorWith(func(o *claudecode.Options) { o.PluginDir = filepath.Join(h.root, "own-plugins") })
	hostBelt, hostBooks := filepath.Join(h.root, "host-belt"), filepath.Join(h.root, "host-plugins")
	b := &recordingBroker{}
	r, _ := h.request("work/a", "exec-a", 1, nil)
	r.Context.Sandbox = b
	r.Context.ToolbeltToken, r.Context.ToolbeltEndpoint = "belt", "unix:///run/belt.sock"
	r.Context.ToolbeltDir, r.Context.PlaybookDir = hostBelt, hostBooks
	if res := h.run(e, r); res.Outcome.Kind == domain.OutcomeFailed {
		t.Fatalf("sandboxed run failed: %+v", res.Outcome)
	}
	if len(b.needs) != 1 || !slices.Contains(b.needs[0].Read, hostBelt) || !slices.Contains(b.needs[0].Read, hostBooks) {
		t.Fatalf("needs %+v, want reads of %s and %s", b.needs, hostBelt, hostBooks)
	}
	if slices.Contains(b.needs[0].Read, filepath.Join(h.root, "bin")) || slices.Contains(b.needs[0].Read, filepath.Join(h.root, "own-plugins")) {
		t.Fatalf("needs %+v read the executor's own directories", b.needs)
	}
}

// TestHostEnv: the executor names CLAUDE_CONFIG_DIR and a binding's
// pass_env as the host variables it admits, and a run takes the host's
// values of them (ExecutionContext.HostEnv) over its own process's; a host
// variable the binding doesn't admit is dropped.
func TestHostEnv(t *testing.T) {
	h := newHarness(t)
	e := h.executor()
	cfg := h.bindingConfig(map[string]any{"pass_env": []string{"HTTPS_PROXY"}})
	if got := e.HostEnv(cfg); !slices.Equal(got, []string{"CLAUDE_CONFIG_DIR", "HTTPS_PROXY"}) {
		t.Fatalf("HostEnv %v", got)
	}
	r, _ := h.request("work/a", "exec-a", 1, map[string]any{"pass_env": []string{"HTTPS_PROXY"}})
	r.Context.HostEnv = map[string]string{"HTTPS_PROXY": "http://proxy.test:3128", "NOT_ADMITTED": "x"}
	if res := h.run(e, r); res.Outcome.Kind == domain.OutcomeFailed {
		t.Fatalf("run failed: %+v", res.Outcome)
	}
	env, _ := os.ReadFile(filepath.Join(h.fake, "env"))
	if !strings.Contains(string(env), "HTTPS_PROXY=http://proxy.test:3128\n") || strings.Contains(string(env), "NOT_ADMITTED") {
		t.Fatalf("claude's environment:\n%s", env)
	}
}
