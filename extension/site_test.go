package extension_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

// siteResources makes its workspace at the request's site when it has
// one, at a path nothing on the host holds.
type siteResources struct {
	dir   string
	seen  *extension.ResourceRequest
	site  string // overrides the site the resource reports
	first string // the output of the first command run at the site
}

func (s *siteResources) Acquire(ctx context.Context, r extension.ResourceRequest) (extension.Resource, error) {
	*s.seen = r
	res := extension.Resource{Kind: "workspace", Ref: "r", Dir: s.dir, Sandbox: &extension.SandboxNeeds{Write: []string{s.dir}, ReadOnly: []string{filepath.Join(s.dir, ".git", "config")}}}
	if r.Site != nil {
		out, err := r.Site.Run(ctx, extension.SiteCommand{Argv: []string{"git", "clone", "repo", s.dir}})
		if err != nil {
			return extension.Resource{}, err
		}
		s.first = string(out.Stdout)
		res.Site = r.Site.ID()
	}
	if s.site != "" {
		res.Site = s.site
	}
	return res, nil
}

func (s *siteResources) Lookup(_ context.Context, ref string) (extension.Resource, error) {
	return extension.Resource{Kind: "workspace", Ref: ref, Dir: s.dir, Site: s.site, Sandbox: &extension.SandboxNeeds{Write: []string{s.dir}}}, nil
}

func (h *containerHost) bindPlaced(p extension.ContainerProvider, prefer string, placed *[]extension.Placement) *extension.BoundContainer {
	return extension.BindContainer(extension.ContainerBinding{Provider: p, Name: "fake", Prefer: prefer, Owner: extension.ResourceOwner{ScopeID: "s", WorkID: "w", ExecutionID: "x"},
		Config: containerConfig(testImage, ""), Host: "host-a", Segment: 1, Protected: []string{h.state}, Shared: []string{filepath.Dir(h.toolbelt)}, RunRoot: h.runRoot,
		Toolbelt: h.toolbelt, Records: h.records, Home: h.home, Now: func() time.Time { return h.now },
		Pin:    func(p extension.ContainerPin) error { h.pins = append(h.pins, p); return nil },
		Placed: func(p extension.Placement) { *placed = append(*placed, p) }})
}

// A placing provider places the execution before anything is acquired;
// the resource provider makes the workspace at the site, through it, with
// the execution's toolbelt; and the launch runs there, with no host
// directory for the workspace and the wrapper in the run directory.
func TestPlacedWorkspace(t *testing.T) {
	h := newContainerHost(t)
	f := newFakePlacer()
	var placed []extension.Placement
	b := h.bindPlaced(f, "node-1", &placed)
	// A path at the site that nothing on the host holds.
	remote := filepath.Join(h.state, "resources", "git", "workspaces", "at-site")
	var seen extension.ResourceRequest
	r := &siteResources{dir: remote, seen: &seen}
	res, err := b.Resources(r).Acquire(context.Background(), extension.ResourceRequest{Mode: "clone"})
	if err != nil {
		t.Fatal(err)
	}
	if seen.Site == nil || seen.Site.ID() != "fake/node-1" || !seen.Sandboxed {
		t.Fatalf("request %+v, want it sandboxed and at fake/node-1", seen)
	}
	if res.Site != "fake/node-1" || r.first != "git clone repo "+remote {
		t.Fatalf("resource %+v, first command %q", res, r.first)
	}
	if len(f.ran) != 1 || f.ran[0].Toolbelt != h.toolbelt {
		t.Fatalf("site commands %+v, want one with the execution's toolbelt", f.ran)
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

// Without a placing provider, nothing is placed and an executor's own Site
// never reaches the resource provider.
func TestUnplacedWorkspace(t *testing.T) {
	h := newContainerHost(t)
	var seen extension.ResourceRequest
	r := &siteResources{dir: h.ws, seen: &seen}
	b := h.bind(newFakeContainers(), containerConfig(testImage, ""), nil)
	forged := extension.PlacedSite{Provider: "fake", Placer: newFakePlacer(), At: extension.Placement{ID: "node-1", Daemon: "d"}}
	res, err := b.Resources(r).Acquire(context.Background(), extension.ResourceRequest{Site: forged})
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
			b := h.bindPlaced(f, "", &placed)
			remote := filepath.Join(h.state, "resources", "git", "workspaces", "at-site")
			var seen extension.ResourceRequest
			r := &siteResources{dir: remote, seen: &seen}
			c.setup(f, r)
			_, err := b.Resources(r).Acquire(context.Background(), extension.ResourceRequest{})
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
	b := h.bindPlaced(f, "node-1", &placed)
	r := &siteResources{dir: filepath.Join(h.state, "ws"), seen: new(extension.ResourceRequest), site: "fake/node-2"}
	if _, err := b.Resources(r).Lookup(context.Background(), "r"); err != nil {
		t.Fatal(err)
	}
	if strings.Join(f.prefers, ",") != "node-2" {
		t.Fatalf("prefers %v, want the lease's node-2", f.prefers)
	}
	// The machine is gone: the provider places elsewhere, so the lease
	// can't be used and the executor acquires again.
	g := newFakePlacer()
	g.node = "node-3"
	b = h.bindPlaced(g, "", &placed)
	if _, err := b.Resources(r).Lookup(context.Background(), "r"); !errors.Is(err, extension.ErrSandboxUnavailable) {
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
