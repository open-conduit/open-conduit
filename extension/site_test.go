package extension_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
)

// fakePlacer is a fake container provider that places launches on nodes.
type fakePlacer struct {
	*fakeContainers
	node     string
	prefers  []string
	ran      []extension.SiteCommand
	placeErr error
	// createdOn, when set, is the daemon Create reports instead of the
	// placement's.
	createdOn string
}

func newFakePlacer() *fakePlacer {
	f := &fakePlacer{fakeContainers: newFakeContainers(), node: "node-1"}
	f.daemon = "daemon-node-1"
	return f
}

func (f *fakePlacer) Place(_ context.Context, _ extension.ResourceOwner, p extension.ContainerPolicy, prefer string) (extension.Placement, error) {
	f.prefers = append(f.prefers, prefer)
	if f.placeErr != nil {
		return extension.Placement{}, f.placeErr
	}
	if p.Paths != (extension.SandboxPaths{}) {
		return extension.Placement{}, errors.New("placed with paths: nothing is acquired yet")
	}
	return extension.Placement{ID: f.node, Daemon: "daemon-" + f.node, Summary: "on " + f.node}, nil
}

func (f *fakePlacer) SiteRun(_ context.Context, placement string, cmd extension.SiteCommand) (extension.SiteResult, error) {
	if placement != f.node {
		return extension.SiteResult{}, extension.ErrSiteUnavailable
	}
	f.ran = append(f.ran, cmd)
	return extension.SiteResult{Stdout: []byte(strings.Join(cmd.Argv, " "))}, nil
}

func (f *fakePlacer) Create(ctx context.Context, p extension.ContainerPlan, labels map[string]string) (extension.ContainerCreated, error) {
	c, err := f.fakeContainers.Create(ctx, p, labels)
	if f.createdOn != "" {
		c.Daemon = f.createdOn
	}
	return c, err
}

// siteResources is a resource provider that makes its workspace at the
// acquisition's site when it has one, at a path nothing on the host holds.
type siteResources struct {
	dir   string
	seen  *extension.ResourceAcquisition
	site  string // overrides the site the resource reports
	first string // the output of the first command run at the site
}

func (s *siteResources) Acquire(ctx context.Context, a extension.ResourceAcquisition) (extension.Resource, error) {
	*s.seen = a
	res := extension.Resource{Kind: "workspace", Ref: "r", Dir: s.dir, Sandbox: &extension.SandboxNeeds{Write: []string{s.dir}, ReadOnly: []string{filepath.Join(s.dir, ".git", "config")}}}
	if a.Site != nil {
		out, err := a.Site.Run(ctx, extension.SiteCommand{Argv: []string{"git", "clone", "repo", s.dir}})
		if err != nil {
			return extension.Resource{}, err
		}
		s.first = string(out.Stdout)
		res.Site = a.Site.ID()
	}
	if s.site != "" {
		res.Site = s.site
	}
	return res, nil
}

func (s *siteResources) Lookup(_ context.Context, _ extension.ResourceOwner, ref string) (extension.Resource, error) {
	return extension.Resource{Kind: "workspace", Ref: ref, Dir: s.dir, Site: s.site, Sandbox: &extension.SandboxNeeds{Write: []string{s.dir}}}, nil
}

func (s *siteResources) Reconcile(context.Context, extension.ResourceStatusFunc) (extension.ResourceReport, error) {
	return extension.ResourceReport{}, nil
}

var siteOwner = extension.ResourceOwner{ScopeID: "s", WorkID: "w", ExecutionID: "x"}

// bindPlaced binds a container provider as the host does, with prov as
// the attempt's resource provider, bound again at the placement (AtSite).
func (h *containerHost) bindPlaced(p extension.ContainerProvider, prov extension.ResourceProvider, prefer string, placed *[]extension.Placement) (*extension.BoundContainer, extension.ResourceBroker) {
	b := extension.BindContainer(extension.ContainerBinding{Provider: p, Name: "fake", Prefer: prefer, Owner: siteOwner,
		Config: containerConfig(testImage, ""), Host: "host-a", Segment: 1, Protected: []string{h.state}, Shared: []string{filepath.Dir(h.toolbelt)}, RunRoot: h.runRoot,
		Toolbelt: h.toolbelt, TokenFile: filepath.Join(filepath.Dir(h.toolbelt), "token"), Records: h.records, Home: h.home, Now: func() time.Time { return h.now },
		Pin:    func(p extension.ContainerPin) error { h.pins = append(h.pins, p); return nil },
		Placed: func(p extension.Placement) { *placed = append(*placed, p) },
		AtSite: func(site extension.Site) extension.ResourceBroker {
			return extension.BindResourcesAt(prov, siteOwner, domain.Payload{}, nil, site)
		}})
	return b, b.Resources(extension.BindResources(prov, siteOwner, domain.Payload{}))
}

// A placing provider places the execution before anything is acquired;
// the resource provider makes the workspace at the site, through it, with
// the execution's toolbelt; and the launch runs there, with no host
// directory for the workspace and the wrapper in the run directory.
func TestPlacedWorkspace(t *testing.T) {
	h := newContainerHost(t)
	f := newFakePlacer()
	var placed []extension.Placement
	// A path at the site that nothing on the host holds.
	remote := filepath.Join(h.state, "resources", "git", "workspaces", "at-site")
	var seen extension.ResourceAcquisition
	r := &siteResources{dir: remote, seen: &seen}
	b, broker := h.bindPlaced(f, r, "node-1", &placed)
	res, err := broker.Acquire(context.Background(), extension.ResourceRequest{Mode: "clone"})
	if err != nil {
		t.Fatal(err)
	}
	if seen.Site == nil || seen.Site.ID() != "fake/node-1" || !seen.Request.Sandboxed || seen.Owner != siteOwner {
		t.Fatalf("acquisition %+v, want it sandboxed, for the owner and at fake/node-1", seen)
	}
	if res.Site != "fake/node-1" || r.first != "git clone repo "+remote {
		t.Fatalf("resource %+v, first command %q", res, r.first)
	}
	token := filepath.Join(filepath.Dir(h.toolbelt), "token")
	if len(f.ran) != 1 || f.ran[0].Toolbelt != h.toolbelt || f.ran[0].TokenFile != token ||
		!slices.Equal(f.ran[0].Env, []string{"OS_ENDPOINT=unix://" + h.toolbelt, "OS_TOKEN_FILE=" + token}) {
		t.Fatalf("site commands %+v, want one with the execution's toolbelt, its token and their environment", f.ran)
	}
	if len(placed) != 1 || placed[0].ID != "node-1" || strings.Join(f.prefers, ",") != "node-1" {
		t.Fatalf("placed %+v, prefers %v, want one placement preferring node-1", placed, f.prefers)
	}
	l := h.launch()
	l.Dir = remote
	needs := extension.SandboxNeeds{Write: []string{remote}, Hosts: []string{"api.anthropic.com"}, Sockets: []string{h.toolbelt}}
	plan, err := b.Launch(context.Background(), needs, l)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.prefers) != 1 {
		t.Fatalf("placed %d times, want once", len(f.prefers))
	}
	pol := f.planned.policy
	if pol.Placement != "node-1" || pol.Paths.Site != "fake/node-1" || pol.Paths.Workspace != remote {
		t.Fatalf("policy %+v, want the placement, the site and the workspace as written", pol)
	}
	if plan.Dir != h.run {
		t.Fatalf("plan dir %q, want the run directory %q", plan.Dir, h.run)
	}
	if len(h.pins) != 1 || h.pins[0].Daemon != "daemon-node-1" {
		t.Fatalf("pins %+v, want the placement's daemon", h.pins)
	}
}

// Without a placing provider, nothing is placed and the acquisition has no
// site.
func TestUnplacedWorkspace(t *testing.T) {
	h := newContainerHost(t)
	var seen extension.ResourceAcquisition
	r := &siteResources{dir: h.ws, seen: &seen}
	b := h.bind(newFakeContainers(), containerConfig(testImage, ""), nil)
	res, err := b.Resources(extension.BindResources(r, siteOwner, domain.Payload{})).Acquire(context.Background(), extension.ResourceRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if seen.Site != nil || res.Site != "" {
		t.Fatalf("request site %v, resource site %q, want none", seen.Site, res.Site)
	}
	plan, err := b.Launch(context.Background(), h.needs(), h.launch())
	if err != nil {
		t.Fatal(err)
	}
	if plan.Dir != "" {
		t.Fatalf("plan dir %q, want none: the workspace is on the host", plan.Dir)
	}
}

// A resource that names another site than the placement, a container made
// elsewhere than placed, a placement without an ID or daemon, and a
// provider that can't place each refuse, with nothing left created.
func TestPlacedWorkspaceRefuses(t *testing.T) {
	cases := []struct {
		name  string
		setup func(f *fakePlacer, r *siteResources)
		at    string // where it fails: acquire or launch
	}{
		{"resource_at_another_site", func(_ *fakePlacer, r *siteResources) { r.site = "fake/node-2" }, "acquire"},
		{"resource_at_another_provider", func(_ *fakePlacer, r *siteResources) { r.site = "other/node-1" }, "acquire"},
		{"created_elsewhere", func(f *fakePlacer, _ *siteResources) { f.createdOn = "daemon-node-2" }, "launch"},
		{"incomplete_placement", func(f *fakePlacer, _ *siteResources) { f.node = "" }, "acquire"},
		{"placement_with_a_slash", func(f *fakePlacer, _ *siteResources) { f.node = "a/b" }, "acquire"},
		{"cannot_place", func(f *fakePlacer, _ *siteResources) { f.placeErr = errors.New("pool full") }, "acquire"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newContainerHost(t)
			f := newFakePlacer()
			var placed []extension.Placement
			remote := filepath.Join(h.state, "resources", "git", "workspaces", "at-site")
			var seen extension.ResourceAcquisition
			r := &siteResources{dir: remote, seen: &seen}
			c.setup(f, r)
			b, broker := h.bindPlaced(f, r, "", &placed)
			_, err := broker.Acquire(context.Background(), extension.ResourceRequest{})
			if c.at == "acquire" {
				if !errors.Is(err, extension.ErrSandboxUnavailable) {
					t.Fatalf("acquire: err %v, want sandbox_unavailable", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			l := h.launch()
			l.Dir = remote
			_, err = b.Launch(context.Background(), extension.SandboxNeeds{Write: []string{remote}}, l)
			if !errors.Is(err, extension.ErrSandboxUnavailable) {
				t.Fatalf("launch: err %v, want sandbox_unavailable", err)
			}
			if len(f.containers) != 0 || len(h.pins) != 0 {
				t.Fatalf("containers %v, pins %v, want none left", f.containers, h.pins)
			}
		})
	}
}

// Lookup places the execution where the lease's workspace is, whatever the
// Work last preferred, and refuses when the provider places it elsewhere.
func TestPlacedLookup(t *testing.T) {
	h := newContainerHost(t)
	f := newFakePlacer()
	f.node = "node-2"
	var placed []extension.Placement
	r := &siteResources{dir: filepath.Join(h.state, "ws"), seen: new(extension.ResourceAcquisition), site: "fake/node-2"}
	_, broker := h.bindPlaced(f, r, "node-1", &placed)
	if _, err := broker.Lookup(context.Background(), "r"); err != nil {
		t.Fatal(err)
	}
	if strings.Join(f.prefers, ",") != "node-2" {
		t.Fatalf("prefers %v, want the lease's node-2", f.prefers)
	}
	// The machine is gone: the provider places elsewhere, so the lease
	// can't be used and the executor acquires again.
	g := newFakePlacer()
	g.node = "node-3"
	_, broker = h.bindPlaced(g, r, "", &placed)
	if _, err := broker.Lookup(context.Background(), "r"); !errors.Is(err, extension.ErrSandboxUnavailable) {
		t.Fatalf("err %v, want sandbox_unavailable", err)
	}
}

func TestSiteID(t *testing.T) {
	if id := extension.SiteID("ssh", "agent@host:22"); id != "ssh/agent@host:22" {
		t.Fatal(id)
	}
	if p, at, ok := extension.SplitSiteID("ssh/agent@host:22"); !ok || p != "ssh" || at != "agent@host:22" {
		t.Fatal(p, at, ok)
	}
	for _, bad := range []string{"", "ssh", "ssh/", "/x"} {
		if _, _, ok := extension.SplitSiteID(bad); ok {
			t.Fatalf("%q split", bad)
		}
	}
}

// recordingPlacer keeps the launch it was asked to plan.
type recordingPlacer struct {
	*fakePlacer
	launch extension.SandboxLaunch
}

func (r *recordingPlacer) Plan(ctx context.Context, o extension.ResourceOwner, p extension.ContainerPolicy, n extension.SandboxNeeds, l extension.SandboxLaunch) (extension.ContainerPlan, error) {
	r.launch = l
	return r.fakePlacer.Plan(ctx, o, p, n, l)
}

// A workspace at the site is named in the launch's argv and environment as
// its provider wrote it, even where a prefix of its path is a symbolic
// link on the host (macOS's /var and /tmp), which the host would resolve
// to a path the site doesn't have.
func TestPlacedWorkspaceKeepsItsPath(t *testing.T) {
	h := newContainerHost(t)
	real := filepath.Join(filepath.Dir(h.state), "real")
	link := filepath.Join(filepath.Dir(h.state), "link")
	if err := os.MkdirAll(real, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	remote := filepath.Join(link, "ws")
	f := &recordingPlacer{fakePlacer: newFakePlacer()}
	var placed []extension.Placement
	r := &siteResources{dir: remote, seen: new(extension.ResourceAcquisition)}
	b, broker := h.bindPlaced(f, r, "", &placed)
	if _, err := broker.Acquire(context.Background(), extension.ResourceRequest{}); err != nil {
		t.Fatal(err)
	}
	l := h.launch()
	l.Dir = remote
	l.Argv = []string{"/bin/echo", remote, filepath.Join(remote, "file")}
	l.Env = []string{"WS=" + remote}
	needs := extension.SandboxNeeds{Write: []string{remote}}
	if _, err := b.Launch(context.Background(), needs, l); err != nil {
		t.Fatal(err)
	}
	got := f.launch
	if got.Dir != remote || got.Argv[1] != remote || got.Argv[2] != filepath.Join(remote, "file") || got.Env[0] != "WS="+remote {
		t.Fatalf("launch %+v, want the site's path %s as written", got, remote)
	}
}

// A placing provider without a host that binds the resource provider again
// (AtSite) makes the resource on the host: the acquisition has no site.
func TestPlacedWithoutAtSite(t *testing.T) {
	h := newContainerHost(t)
	var seen extension.ResourceAcquisition
	r := &siteResources{dir: h.ws, seen: &seen}
	b := extension.BindContainer(extension.ContainerBinding{Provider: newFakePlacer(), Name: "fake", Owner: siteOwner,
		Config: containerConfig(testImage, ""), Host: "host-a", Segment: 1, Protected: []string{h.state}, Shared: []string{filepath.Dir(h.toolbelt)}, RunRoot: h.runRoot,
		Toolbelt: h.toolbelt, Records: h.records, Home: h.home, Now: func() time.Time { return h.now },
		Pin: func(p extension.ContainerPin) error { h.pins = append(h.pins, p); return nil }})
	if _, err := b.Resources(extension.BindResources(r, siteOwner, domain.Payload{})).Acquire(context.Background(), extension.ResourceRequest{}); err != nil {
		t.Fatal(err)
	}
	if seen.Site != nil {
		t.Fatalf("acquisition site %v, want none", seen.Site.ID())
	}
}

// A placed site's commands get the run's environment layer under their
// own, git config extended rather than replaced, and the toolbelt after
// both.
func TestPlacedSiteEnv(t *testing.T) {
	f := newFakePlacer()
	layer := extension.GitTransportLayer("github.com")
	layer["GIT_AUTHOR_NAME"] = "bot"
	site := extension.PlacedSite{Provider: "fake", Placer: f, At: extension.Placement{ID: "node-1"}, Toolbelt: "/run/tb.sock", TokenFile: "/run/token", Env: layer}
	if _, err := site.Run(context.Background(), extension.SiteCommand{Argv: []string{"git"}, Env: []string{"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=core.hooksPath", "GIT_CONFIG_VALUE_0=/dev/null", "GIT_AUTHOR_NAME=own", "OS_ENDPOINT=unix:///elsewhere"}}); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{}
	for _, kv := range f.ran[0].Env {
		k, v, _ := strings.Cut(kv, "=")
		env[k] = v
	}
	if env["GIT_CONFIG_COUNT"] != "4" || env["GIT_CONFIG_KEY_0"] != "alias.remote-"+extension.GitTransport || env["GIT_CONFIG_KEY_3"] != "core.hooksPath" {
		t.Fatalf("git config %v, want the layer's then the command's", f.ran[0].Env)
	}
	if env["GIT_AUTHOR_NAME"] != "own" || env["OS_ENDPOINT"] != "unix:///run/tb.sock" || env["OS_TOKEN_FILE"] != "/run/token" {
		t.Fatalf("env %v: the command's own over the layer, the toolbelt's last", f.ran[0].Env)
	}
}
