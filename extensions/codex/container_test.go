package codex_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/extensions/codex"
)

// fakeBroker stands in for the host's container broker: its plan runs the
// launch's argv as it is, on the host, with the secrets exported as the
// container's entry stub would, and it records what it was asked for.
type fakeBroker struct {
	mu       sync.Mutex
	needs    []extension.SandboxNeeds
	launches []extension.SandboxLaunch
	refuse   error
	adopted  []bool
	exit     extension.ContainerExit
}

func (b *fakeBroker) Launch(_ context.Context, n extension.SandboxNeeds, l extension.SandboxLaunch) (extension.SandboxPlan, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.needs, b.launches = append(b.needs, n), append(b.launches, l)
	if b.refuse != nil {
		return extension.SandboxPlan{}, b.refuse
	}
	argv := append(append([]string{"env"}, l.Secrets...), l.Argv...)
	return extension.SandboxPlan{Argv: argv, Env: l.Env, Signals: extension.SandboxTerm}, nil
}

func (b *fakeBroker) Adopt(_ context.Context, alive bool) (extension.ContainerExit, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.adopted = append(b.adopted, alive)
	return b.exit, nil
}

func (b *fakeBroker) last() (extension.SandboxNeeds, extension.SandboxLaunch) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.needs[len(b.needs)-1], b.launches[len(b.launches)-1]
}

// TestCodexInAContainer: a container binding's segment is the /bin/sh
// driver, not the host's: codex runs with the Work's home as HOME and
// CODEX_HOME in it, a Linux PATH, its API key as a secret of the launch
// and never in its environment, the container's own `os` as the MCP server
// taking its endpoint from codex's environment, and needs that name the
// workspace, the home, the token's directory, the toolbelt socket and
// OpenAI's API. An event injected mid-turn is a follow-up turn on the same
// thread, and the next segment resumes the thread from the home.
func TestCodexInAContainer(t *testing.T) {
	f := newFixture(t)
	e := f.executor()
	home := filepath.Join(f.fake, "work-home")
	tokens := filepath.Join(f.fake, "tokens")
	for _, d := range []string{home, tokens} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	barrier := filepath.Join(f.fake, "go")
	cfg := f.config(map[string]any{"secret_env": map[string]string{"openai": "CODEX_API_KEY"}}, map[string]string{"FAKE_BARRIER": barrier, "GH_TOKEN": "gh-secret"})
	b := &fakeBroker{}
	req := func(exec string) extension.RunRequest {
		r := request("work-c", exec, cfg)
		r.Context.Container, r.Context.ContainerHome = b, home
		r.Context.Secrets = map[string]string{"openai": "sk-test"}
		r.Context.ToolbeltEndpoint, r.Context.ToolbeltToken, r.Context.ToolbeltDir = "unix:///tmp/toolbelt.sock", "secret-token", "/opt/oc"
		r.Context.ToolbeltTokenFile = filepath.Join(tokens, "token")
		return r
	}
	if err := os.WriteFile(filepath.Join(tokens, "token"), []byte("secret-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	h, err := e.Run(context.Background(), req("exec-1"))
	if err != nil {
		t.Fatal(err)
	}
	if err := waitFile(filepath.Join(f.fake, "started-1")); err != nil {
		t.Fatal(err)
	}
	if err := e.Inject(context.Background(), "exec-1", domain.Event{ID: "ev-late", Type: "github.issue.comment", Data: domain.Payload{Data: []byte(`{"author":"alex","body":"also this"}`)}}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(barrier, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	res, err := h.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome.Kind != domain.OutcomeHandoff || !slices.Equal(res.ConsumedInputs, []domain.ID{"ev-late"}) {
		t.Fatalf("the contained segment: %+v consumed %v\n%s", res.Outcome, res.ConsumedInputs, f.read("call-1"))
	}

	needs, launch := b.last()
	if launch.Argv[0] != "/bin/sh" || launch.Argv[3] != "codex-segment" || slices.Contains(launch.Argv, codex.SegmentCommand) {
		t.Fatalf("the container runs %q, want the /bin/sh driver", launch.Argv[:4])
	}
	if !slices.Equal(launch.Secrets, []string{"CODEX_API_KEY=sk-test", "GH_TOKEN=gh-secret"}) {
		t.Fatalf("launch secrets %v", launch.Secrets)
	}
	for _, kv := range launch.Env {
		if strings.Contains(kv, "sk-test") || strings.Contains(kv, "gh-secret") || strings.Contains(kv, "secret-token") || strings.HasPrefix(kv, "TMPDIR=") {
			t.Fatalf("the launch environment holds %q", kv)
		}
	}
	if !slices.Equal(needs.Write, []string{workdir(t, f, "work-c"), home}) || !slices.Equal(needs.Read, []string{tokens}) ||
		!slices.Equal(needs.Sockets, []string{"/tmp/toolbelt.sock"}) || !slices.Equal(needs.Hosts, []string{"api.openai.com"}) {
		t.Fatalf("needs %+v", needs)
	}

	env, args := f.read("env-1"), f.read("call-1")
	for _, want := range []string{"HOME=" + home + "\n", "CODEX_HOME=" + filepath.Join(home, ".codex") + "\n", "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin\n",
		"CODEX_API_KEY=sk-test\n", "OS_ENDPOINT=unix:///tmp/toolbelt.sock\n", "OS_TOKEN_FILE=" + filepath.Join(tokens, "token") + "\n"} {
		if !strings.Contains(env, want) {
			t.Fatalf("codex's environment lacks %q:\n%s", want, env)
		}
	}
	for _, want := range []string{`mcp_servers.open-conduit.command="os"`, `mcp_servers.open-conduit.env_vars=["OS_ENDPOINT","OS_TOKEN_FILE"]`, "--json\n", "--output-schema\n", "\n-\n"} {
		if !strings.Contains(args, want) {
			t.Fatalf("codex's args lack %q:\n%s", want, args)
		}
	}
	if strings.Contains(args, "secret-token") || strings.Contains(args, "/opt/oc") || strings.HasPrefix(args, "exec\nresume") {
		t.Fatalf("first turn's args:\n%s", args)
	}
	if follow := f.read("call-2"); !strings.HasPrefix(follow, "exec\nresume\n") || !strings.HasSuffix(follow, "\nthread-1\n-\n") {
		t.Fatalf("follow-up turn:\n%s", follow)
	}
	if in := f.read("stdin-2"); !strings.HasPrefix(in, "## Arrived during this turn\n\n") || !strings.Contains(in, "also this") {
		t.Fatalf("follow-up message:\n%s", in)
	}
	if _, err := os.Stat(filepath.Join(f.fake, "call-3")); err == nil {
		t.Fatal("the driver ran a third turn with nothing pending")
	}

	// The next segment resumes the thread in a new container.
	res = run(t, e, req("exec-2"))
	if d := data(t, res); res.Outcome.Kind != domain.OutcomeHandoff || d.Kind != "events" {
		t.Fatalf("second segment: %+v %+v", res.Outcome, d)
	}
	if second := f.read("call-3"); !strings.HasPrefix(second, "exec\nresume\n") || !strings.HasSuffix(second, "\nthread-1\n-\n") {
		t.Fatalf("second segment's turn:\n%s", second)
	}
	if len(b.launches) != 2 {
		t.Fatalf("%d container launches, want one for each segment", len(b.launches))
	}
}

// workdir is the folder a workspace-free binding's Work runs in.
func workdir(t *testing.T, f *fixture, work string) string {
	t.Helper()
	matches, _ := filepath.Glob(filepath.Join(f.state, "work", "*"))
	for _, m := range matches {
		if info, err := os.Stat(m); err == nil && info.IsDir() {
			return m
		}
	}
	t.Fatalf("no folder for %s", work)
	return ""
}

// TestCodexContainerFailsClosed: a launch the broker refuses, or one the
// host gave no home, runs nothing and fails with the container's reason.
func TestCodexContainerFailsClosed(t *testing.T) {
	f := newFixture(t)
	e := f.executor()
	for _, c := range []struct {
		name, home, reason string
		refuse             error
	}{
		{"refused", filepath.Join(f.fake, "home"), "sandbox_denied", extension.ErrSandboxDenied},
		{"running", filepath.Join(f.fake, "home"), "container_running", extension.ErrContainerRunning},
		{"no_home", "", "sandbox_unavailable", nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			_ = os.MkdirAll(filepath.Join(f.fake, "home"), 0o700)
			r := request("work-"+c.name, "exec-"+c.name, f.config(nil, nil))
			r.Context.Container, r.Context.ContainerHome = &fakeBroker{refuse: c.refuse}, c.home
			res := run(t, e, r)
			if res.Outcome.Kind != domain.OutcomeFailed || data(t, res).Reason != c.reason {
				t.Fatalf("%+v %+v", res.Outcome, data(t, res))
			}
			if _, err := os.Stat(filepath.Join(f.fake, "call-1")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("codex ran")
			}
		})
	}
}

// TestCodexContainerProvider: a binding's provider is the host the
// container needs, its key a secret of the launch.
func TestCodexContainerProvider(t *testing.T) {
	f := newFixture(t)
	b := &fakeBroker{}
	r := request("work-p", "exec-p", f.config(map[string]any{"provider": map[string]any{"id": "router", "base_url": "https://openrouter.ai/api/v1", "api_key_secret": "router"}}, nil))
	r.Context.Container, r.Context.ContainerHome = b, filepath.Join(f.fake, "home")
	_ = os.MkdirAll(r.Context.ContainerHome, 0o700)
	r.Context.Secrets = map[string]string{"router": "sk-or"}
	if res := run(t, f.executor(), r); res.Outcome.Kind == domain.OutcomeFailed {
		t.Fatalf("%+v", res.Outcome)
	}
	needs, launch := b.last()
	if !slices.Equal(needs.Hosts, []string{"openrouter.ai"}) || !slices.Equal(launch.Secrets, []string{"OC_PROVIDER_API_KEY=sk-or"}) {
		t.Fatalf("hosts %v secrets %v", needs.Hosts, launch.Secrets)
	}
}

// TestCodexVersionProbe: the probe doctor runs in a container's image is
// the binding's codex with a container's PATH.
func TestCodexVersionProbe(t *testing.T) {
	argv, env := codex.VersionProbe(json.RawMessage(`{"binary":"/opt/codex"}`))
	if !slices.Equal(argv, []string{"/opt/codex", "--version"}) || !slices.Equal(env, []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"}) {
		t.Fatalf("%v %v", argv, env)
	}
	if argv, _ := codex.VersionProbe(json.RawMessage(`{"sandbox":"nope"}`)); argv != nil {
		t.Fatalf("a refused config probes %v", argv)
	}
}
