package extension_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
)

// fakeSandbox is a provider that records what the host handed it and wraps
// the argv in a marker, so the contract is tested without a real sandbox.
type fakeSandbox struct {
	policy extension.SandboxPolicy
	needs  extension.SandboxNeeds
	launch extension.SandboxLaunch
	err    error
	plan   *extension.SandboxPlan
	adopt  struct {
		plan         extension.SandboxPlan
		parent, root int
		called       bool
	}
}

func (f *fakeSandbox) Plan(_ context.Context, _ extension.ResourceOwner, p extension.SandboxPolicy, n extension.SandboxNeeds, l extension.SandboxLaunch) (extension.SandboxPlan, error) {
	f.policy, f.needs, f.launch = p, n, l
	if f.err != nil {
		return extension.SandboxPlan{}, f.err
	}
	if f.plan != nil {
		return *f.plan, nil
	}
	return extension.SandboxPlan{Argv: append([]string{"fake-sandbox", "--"}, l.Argv...), Env: l.Env, Signals: extension.SandboxTerm, Summary: "fake",
		Data: domain.Payload{Schema: "fake.v1", Data: json.RawMessage(`{"policy":1}`)}}, nil
}

func (f *fakeSandbox) Adopt(_ context.Context, plan extension.SandboxPlan, parent, root int) error {
	f.adopt.plan, f.adopt.parent, f.adopt.root, f.adopt.called = plan, parent, root, true
	return nil
}

type sandboxHost struct {
	state, runRoot, run, ws, toolbelt, records string
	pins                                       []extension.SandboxPlan
}

func newSandboxHost(t *testing.T) *sandboxHost {
	t.Helper()
	root, _ := filepath.EvalSymlinks(t.TempDir())
	h := &sandboxHost{state: filepath.Join(root, "state")}
	h.runRoot = filepath.Join(h.state, "executors", "shell", "runs")
	h.run = filepath.Join(h.runRoot, "k", "n")
	h.ws = filepath.Join(h.state, "workspaces", "mine")
	h.toolbelt = filepath.Join(h.state, "runs", "k", "toolbelt.sock")
	h.records = filepath.Join(h.state, "runs", "k", "sandbox")
	for _, d := range []string{h.run, h.ws, filepath.Dir(h.toolbelt), filepath.Join(h.state, "private")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(h.state, "private", "operator.token"), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	return h
}

func (h *sandboxHost) bind(p extension.SandboxProvider, pinned string) *extension.BoundSandbox {
	return extension.BindSandbox(extension.SandboxBinding{Provider: p, Owner: extension.ResourceOwner{ScopeID: "s", WorkID: "w", ExecutionID: "x"},
		Config: domain.Payload{Schema: "core.config.sandbox.v1", Data: json.RawMessage(`{}`)}, Protected: []string{h.state},
		Shared: []string{filepath.Dir(h.toolbelt)}, RunRoot: h.runRoot, Toolbelt: h.toolbelt, Records: h.records,
		Pin: func(p extension.SandboxPlan) error { h.pins = append(h.pins, p); return nil }, Pinned: pinned})
}

func (h *sandboxHost) launch() extension.SandboxLaunch {
	return extension.SandboxLaunch{Argv: []string{"/bin/echo", "hi"}, Dir: h.ws, Env: []string{"A=1"}, RunDir: h.run}
}

func TestWrapLaunchWithoutSandbox(t *testing.T) {
	l := extension.SandboxLaunch{Argv: []string{"/bin/true"}, Env: []string{"A=1"}}
	p, err := extension.WrapLaunch(context.Background(), nil, extension.SandboxNeeds{}, l)
	if err != nil || strings.Join(p.Argv, " ") != "/bin/true" || len(p.Env) != 1 || p.Digest != "" {
		t.Fatalf("plan %+v err %v, want the launch unchanged", p, err)
	}
	if err := extension.AdoptSandboxed(context.Background(), nil, 42); err != nil {
		t.Fatalf("adopt without a sandbox: %v", err)
	}
}

func TestBindSandboxMergesAndPins(t *testing.T) {
	h := newSandboxHost(t)
	f := &fakeSandbox{}
	b := h.bind(f, "")
	res := b.Resources(stubResources{dir: h.ws, needs: &extension.SandboxNeeds{Write: []string{h.ws}, ReadOnly: []string{filepath.Join(h.ws, ".git", "config")}}})
	got, err := res.Acquire(context.Background(), extension.ResourceRequest{})
	if err != nil || got.Sandbox == nil || got.Sandbox.Write[0] != h.ws {
		t.Fatalf("acquire: %+v %v", got, err)
	}
	needs := extension.SandboxNeeds{Write: []string{h.ws, filepath.Join(h.run, "outputs")}, Read: []string{filepath.Dir(h.toolbelt)}, Hosts: []string{"api.example"}, Sockets: []string{h.toolbelt}}
	p, err := extension.WrapLaunch(context.Background(), b, needs, h.launch())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(p.Digest, "sha256:") || p.Digest != extension.SandboxDigest(p.Argv, p.Data) {
		t.Errorf("digest %q does not pin the plan", p.Digest)
	}
	if len(h.pins) != 1 || h.pins[0].Digest != p.Digest {
		t.Errorf("pins %+v, want the plan pinned before it is returned", h.pins)
	}
	if p.Argv[0] != "/bin/sh" || !strings.Contains(strings.Join(p.Argv, " "), "fake-sandbox -- /bin/echo hi") {
		t.Errorf("argv %q, want the root stub then the provider's argv", p.Argv)
	}
	if !contains(f.needs.Deny, h.state) || !contains(f.needs.ReadOnly, filepath.Join(h.ws, ".git", "config")) || !contains(f.needs.Read, filepath.Dir(h.toolbelt)) || !contains(f.needs.Hosts, "api.example") {
		t.Errorf("needs %+v, want the protected set, the resource's and the executor's merged", f.needs)
	}
	if f.policy.Paths.Workspace != h.ws || f.policy.Paths.Run != h.run || f.policy.Paths.Toolbelt != h.toolbelt || !strings.HasPrefix(f.policy.Paths.Records, h.records) {
		t.Errorf("paths %+v", f.policy.Paths)
	}
	plans, _ := filepath.Glob(filepath.Join(h.records, "*", "plan.json"))
	if len(plans) != 1 {
		t.Fatalf("records: %v", plans)
	}
	if info, _ := os.Stat(plans[0]); info.Mode().Perm()&0o222 != 0 {
		t.Errorf("the plan record is writable: %v", info.Mode())
	}
}

func TestBindSandboxRefuses(t *testing.T) {
	cases := []struct {
		name  string
		setup func(h *sandboxHost, l *extension.SandboxLaunch, n *extension.SandboxNeeds, f *fakeSandbox) extension.ResourceBroker
		want  error
	}{
		{"run_dir_outside_the_executor", func(h *sandboxHost, l *extension.SandboxLaunch, _ *extension.SandboxNeeds, _ *fakeSandbox) extension.ResourceBroker {
			l.RunDir = filepath.Join(h.state, "runs", "k")
			return nil
		}, extension.ErrSandboxDenied},
		{"symlinked_run_dir", func(h *sandboxHost, l *extension.SandboxLaunch, _ *extension.SandboxNeeds, _ *fakeSandbox) extension.ResourceBroker {
			link := filepath.Join(h.runRoot, "k", "link")
			_ = os.Symlink(h.state, link)
			l.RunDir = link
			return nil
		}, extension.ErrSandboxDenied},
		{"workspace_holding_the_state", func(h *sandboxHost, l *extension.SandboxLaunch, _ *extension.SandboxNeeds, _ *fakeSandbox) extension.ResourceBroker {
			l.Dir = filepath.Dir(h.state)
			return nil
		}, extension.ErrSandboxDenied},
		{"need_inside_the_state", func(h *sandboxHost, _ *extension.SandboxLaunch, n *extension.SandboxNeeds, _ *fakeSandbox) extension.ResourceBroker {
			n.Read = append(n.Read, filepath.Join(h.state, "private", "operator.token"))
			return nil
		}, extension.ErrSandboxDenied},
		{"need_through_a_dotdot", func(h *sandboxHost, _ *extension.SandboxLaunch, n *extension.SandboxNeeds, _ *fakeSandbox) extension.ResourceBroker {
			n.Write = append(n.Write, filepath.Join(h.ws, "..", "..", "private"))
			return nil
		}, extension.ErrSandboxDenied},
		{"socket_inside_the_state", func(h *sandboxHost, _ *extension.SandboxLaunch, n *extension.SandboxNeeds, _ *fakeSandbox) extension.ResourceBroker {
			n.Sockets = append(n.Sockets, filepath.Join(h.state, "management.sock"))
			return nil
		}, extension.ErrSandboxDenied},
		{"hard_link_to_a_protected_file", func(h *sandboxHost, _ *extension.SandboxLaunch, _ *extension.SandboxNeeds, _ *fakeSandbox) extension.ResourceBroker {
			_ = os.Link(filepath.Join(h.state, "private", "operator.token"), filepath.Join(h.ws, "token"))
			return nil
		}, extension.ErrSandboxDenied},
		{"workspace_without_sandbox_needs", func(h *sandboxHost, _ *extension.SandboxLaunch, _ *extension.SandboxNeeds, _ *fakeSandbox) extension.ResourceBroker {
			return stubResources{dir: h.ws}
		}, extension.ErrSandboxUnavailable},
		{"launch_outside_the_workspace", func(h *sandboxHost, l *extension.SandboxLaunch, _ *extension.SandboxNeeds, _ *fakeSandbox) extension.ResourceBroker {
			other := filepath.Join(h.state, "workspaces", "other")
			_ = os.MkdirAll(other, 0o700)
			l.Dir = other
			return stubResources{dir: h.ws, needs: &extension.SandboxNeeds{}}
		}, extension.ErrSandboxUnavailable},
		{"provider_refuses", func(_ *sandboxHost, _ *extension.SandboxLaunch, _ *extension.SandboxNeeds, f *fakeSandbox) extension.ResourceBroker {
			f.err = extension.ErrSandboxDenied
			return nil
		}, extension.ErrSandboxDenied},
		{"incomplete_plan", func(_ *sandboxHost, _ *extension.SandboxLaunch, _ *extension.SandboxNeeds, f *fakeSandbox) extension.ResourceBroker {
			f.plan = &extension.SandboxPlan{Argv: []string{"x"}}
			return nil
		}, extension.ErrSandboxUnavailable},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newSandboxHost(t)
			f := &fakeSandbox{}
			l, n := h.launch(), extension.SandboxNeeds{Write: []string{h.ws}}
			b := h.bind(f, "")
			if r := c.setup(h, &l, &n, f); r != nil {
				if _, err := b.Resources(r).Acquire(context.Background(), extension.ResourceRequest{}); err != nil {
					t.Fatal(err)
				}
			}
			_, err := extension.WrapLaunch(context.Background(), b, n, l)
			if !errors.Is(err, c.want) {
				t.Fatalf("err %v, want %v", err, c.want)
			}
			if len(h.pins) != 0 {
				t.Error("a refused plan was pinned")
			}
		})
	}
}

func TestBindSandboxAdopt(t *testing.T) {
	h := newSandboxHost(t)
	f := &fakeSandbox{}
	p, err := extension.WrapLaunch(context.Background(), h.bind(f, ""), extension.SandboxNeeds{Write: []string{h.ws}}, h.launch())
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(mustGlob(t, filepath.Join(h.records, "*", "plan.json")))
	if err := os.WriteFile(filepath.Join(dir, "root"), []byte("4242\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := extension.AdoptSandboxed(context.Background(), h.bind(f, p.Digest), 77); err != nil {
		t.Fatalf("adopt the pinned plan: %v", err)
	}
	if !f.adopt.called || f.adopt.parent != 77 || f.adopt.root != 4242 || f.adopt.plan.Argv[0] != "fake-sandbox" {
		t.Errorf("the provider was asked %+v, want the plan without the root stub, the wrapper and the recorded root", f.adopt)
	}
	// A segment launched without a sandbox, now under one.
	if err := extension.AdoptSandboxed(context.Background(), h.bind(f, ""), 77); !errors.Is(err, extension.ErrNotAdoptable) {
		t.Errorf("adopt an unsandboxed segment: %v, want ErrNotAdoptable", err)
	}
	// Another plan's digest.
	if err := extension.AdoptSandboxed(context.Background(), h.bind(f, "sha256:other"), 77); !errors.Is(err, extension.ErrNotAdoptable) {
		t.Errorf("adopt under another digest: %v, want ErrNotAdoptable", err)
	}
	// A record changed since it was pinned.
	plan := filepath.Join(dir, "plan.json")
	raw, _ := os.ReadFile(plan)
	_ = os.Chmod(plan, 0o600)
	_ = os.WriteFile(plan, []byte(strings.Replace(string(raw), `"policy":1`, `"policy":2`, 1)), 0o600)
	if err := extension.AdoptSandboxed(context.Background(), h.bind(f, p.Digest), 77); !errors.Is(err, extension.ErrNotAdoptable) {
		t.Errorf("adopt a changed record: %v, want ErrNotAdoptable", err)
	}
}

func TestSandboxedResourcesMarkTheRequest(t *testing.T) {
	h := newSandboxHost(t)
	var seen extension.ResourceRequest
	r := h.bind(&fakeSandbox{}, "").Resources(stubResources{dir: h.ws, seen: &seen})
	if _, err := r.Acquire(context.Background(), extension.ResourceRequest{Mode: "clone"}); err != nil {
		t.Fatal(err)
	}
	if !seen.Sandboxed || seen.Mode != "clone" {
		t.Errorf("request %+v, want it marked sandboxed", seen)
	}
}

func TestSandboxReason(t *testing.T) {
	if extension.SandboxReason(extension.ErrSandboxDenied) != "sandbox_denied" || extension.SandboxReason(errors.New("x")) != "sandbox_unavailable" {
		t.Fatal("reasons")
	}
}

type stubResources struct {
	dir   string
	needs *extension.SandboxNeeds
	seen  *extension.ResourceRequest
}

func (s stubResources) Acquire(_ context.Context, r extension.ResourceRequest) (extension.Resource, error) {
	if s.seen != nil {
		*s.seen = r
	}
	return extension.Resource{Kind: "workspace", Ref: "r", Dir: s.dir, Sandbox: s.needs}, nil
}
func (s stubResources) Lookup(ctx context.Context, _ string) (extension.Resource, error) {
	return s.Acquire(ctx, extension.ResourceRequest{})
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func mustGlob(t *testing.T, pattern string) string {
	t.Helper()
	m, _ := filepath.Glob(pattern)
	if len(m) != 1 {
		t.Fatalf("%s: %v", pattern, m)
	}
	return m[0]
}
