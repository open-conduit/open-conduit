package ssh

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/open-conduit/open-conduit/conformance"
	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
)

// TestPlacerConformance: the remote is a placement that keeps the
// placement contract the host relies on.
func TestPlacerConformance(t *testing.T) {
	f := newFakeRemote(t)
	if err := os.MkdirAll(f.root+f.ws, 0o700); err != nil {
		t.Fatal(err)
	}
	err := conformance.CheckContainerPlacer(conformance.PlacerFixture{
		Provider: f.p,
		Policy:   extension.ContainerPolicy{Config: f.config, Paths: extension.SandboxPaths{Workspace: f.ws, Run: f.run, Records: f.records}},
		Needs:    extension.SandboxNeeds{Write: []string{f.ws}},
		Launch:   extension.SandboxLaunch{Argv: []string{"/bin/sh", "-c", "exit 0"}, Dir: f.ws, RunDir: f.run, Env: []string{"PATH=/usr/bin:/bin"}},
		Dir:      f.ws,
		Pwd:      f.root + f.ws,
	})
	if err != nil {
		t.Fatal(err)
	}
}

// siteMaker is a resource provider that makes its workspace at the
// acquisition's site, through the site, as a git provider would clone.
type siteMaker struct {
	dir string
	env string
}

func (m *siteMaker) Acquire(ctx context.Context, a extension.ResourceAcquisition) (extension.Resource, error) {
	if a.Site == nil {
		return extension.Resource{}, errors.New("no site")
	}
	res, err := a.Site.Run(ctx, extension.SiteCommand{Argv: []string{"/bin/sh", "-c", `mkdir -p "$1" && echo made > "$1/made.txt" && printf '%s|%s' "$OS_ENDPOINT" "$OS_TOKEN_FILE"`, "make", os.Getenv("SITE_ROOT") + m.dir}})
	if err != nil {
		return extension.Resource{}, err
	}
	if res.ExitCode != 0 {
		return extension.Resource{}, errors.New(string(res.Stderr))
	}
	m.env = string(res.Stdout)
	return extension.Resource{Kind: "workspace", Ref: "r", Dir: m.dir, Site: a.Site.ID(), Sandbox: &extension.SandboxNeeds{Write: []string{m.dir}}}, nil
}

func (m *siteMaker) Lookup(context.Context, extension.ResourceOwner, string) (extension.Resource, error) {
	return extension.Resource{}, extension.ErrResourceNotFound
}

func (m *siteMaker) Reconcile(context.Context, extension.ResourceStatusFunc) (extension.ResourceReport, error) {
	return extension.ResourceReport{}, nil
}

// TestWorkspaceAtSite: the binding's remote is the placement; a resource
// provider makes the workspace there through SiteRun, with the
// execution's toolbelt token put at its path there and its socket
// forwarded; and the launch neither uploads the workspace nor copies it
// back, so nothing of it is on the host, what the provider made there
// survives the run's upload, and removing the run leaves it.
func TestWorkspaceAtSite(t *testing.T) {
	f := newFakeRemote(t)
	ctx := context.Background()
	t.Setenv("SITE_ROOT", f.root)
	token := filepath.Join(filepath.Dir(f.tb), "token")
	remote := filepath.Join(f.state, "resources", "git", "workspaces", "at-site")
	maker := &siteMaker{dir: remote}
	owner := extension.ResourceOwner{ScopeID: "s", WorkID: "w", ExecutionID: "x"}
	b := extension.BindContainer(extension.ContainerBinding{Provider: f.p, Name: Type, Owner: owner,
		Config: f.config, Host: "host-a", Segment: 1, Protected: []string{f.state}, Shared: []string{filepath.Dir(f.tb)}, RunRoot: f.runRoot,
		Toolbelt: f.tb, TokenFile: token, Records: f.records,
		Pin: func(p extension.ContainerPin) error { f.pins = append(f.pins, p); return nil },
		AtSite: func(site extension.Site) extension.ResourceBroker {
			return extension.BindResourcesAt(maker, owner, domain.Payload{}, nil, site)
		}})
	res, err := b.Resources(extension.BindResources(maker, owner, domain.Payload{})).Acquire(ctx, extension.ResourceRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Site != Type+"/"+f.settings.Endpoint() {
		t.Fatalf("site %q, want the binding's remote", res.Site)
	}
	if maker.env != "unix://"+f.tb+"|"+token {
		t.Errorf("the site command's toolbelt environment is %q", maker.env)
	}
	if got := read(t, f.root+token); got != "toolbelt-token" {
		t.Errorf("the remote's toolbelt token is %q", got)
	}
	if fi, err := os.Stat(f.root + token); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("the remote's token %v, %v, want owner-only", fi, err)
	}
	if !strings.Contains(read(t, f.log), "-R "+f.root+f.tb+":"+f.tb) {
		t.Error("the toolbelt socket was not forwarded for the site command")
	}
	if _, err := os.Stat(remote); !os.IsNotExist(err) {
		t.Fatalf("the workspace was made on the host: %v", err)
	}
	l := extension.SandboxLaunch{Argv: []string{"/bin/sh", "-c", "cat made.txt > seen.txt; pwd > pwd.txt"}, Dir: remote, RunDir: f.run, Env: []string{"PATH=/usr/bin:/bin"}}
	plan, err := b.Launch(ctx, extension.SandboxNeeds{Write: []string{remote}, Sockets: []string{f.tb}}, l)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Dir != f.run {
		t.Errorf("the wrapper's directory is %q, want the run directory", plan.Dir)
	}
	if code, out := f.helper(plan.Argv, nil); code != 0 {
		t.Fatalf("the helper exited %d: %s", code, out)
	}
	if got := read(t, f.root+remote+"/seen.txt"); got != "made\n" {
		t.Errorf("the run saw %q in the workspace the provider made", got)
	}
	if got := strings.TrimSpace(read(t, f.root+remote+"/pwd.txt")); got != f.root+remote {
		t.Errorf("the run ran in %q", got)
	}
	if _, err := os.Stat(remote); !os.IsNotExist(err) {
		t.Errorf("the workspace was copied back to the host: %v", err)
	}
	if err := extension.RemoveContainers(ctx, f.p, extension.ResourceOwner{ExecutionID: "x"}); err != nil {
		t.Fatal(err)
	}
	if got := read(t, f.root+remote+"/made.txt"); got != "made\n" {
		t.Errorf("removing the run took the provider's workspace: %q", got)
	}
}

// TestSiteRefusals: a placement no binding uses is unavailable; a plan at
// another placement than the binding's remote is refused; a site command
// with a toolbelt needs its token, and one with an environment entry a
// remote can't take is refused before ssh runs.
func TestSiteRefusals(t *testing.T) {
	f := newFakeRemote(t)
	ctx := context.Background()
	if _, err := f.p.SiteRun(ctx, "oc@elsewhere:22", extension.SiteCommand{Argv: []string{"true"}}); !errors.Is(err, extension.ErrSiteUnavailable) {
		t.Errorf("unknown placement: %v, want ErrSiteUnavailable", err)
	}
	at := f.settings.Endpoint()
	if _, err := f.p.SiteRun(ctx, at, extension.SiteCommand{Argv: []string{"true"}, Toolbelt: f.tb}); err == nil {
		t.Error("a toolbelt without its token ran")
	}
	if _, err := f.p.SiteRun(ctx, at, extension.SiteCommand{Argv: []string{"true"}, Env: []string{"BAD KEY=1"}}); err == nil {
		t.Error("an environment entry a remote can't take ran")
	}
	if _, err := f.p.SiteRun(ctx, at, extension.SiteCommand{Argv: []string{"A=1"}}); err == nil {
		t.Error("an argv env(1) would take as a variable ran")
	}
	policy := extension.ContainerPolicy{Config: f.config, Paths: extension.SandboxPaths{Workspace: f.ws, Run: f.run, Records: f.records}, Placement: "oc@elsewhere:22"}
	if _, err := f.p.Plan(ctx, extension.ResourceOwner{ExecutionID: "x"}, policy, extension.SandboxNeeds{Write: []string{f.ws}}, f.launch("true")); !errors.Is(err, extension.ErrSandboxUnavailable) {
		t.Errorf("a plan at another placement: %v, want sandbox_unavailable", err)
	}
	// A placement is the binding's remote, whatever was preferred.
	p, err := f.p.Place(ctx, extension.ResourceOwner{ExecutionID: "x"}, extension.ContainerPolicy{Config: f.config}, "oc@other:22")
	if err != nil || p.ID != at || p.Daemon != f.settings.Daemon() {
		t.Errorf("placement %+v, %v, want the remote", p, err)
	}
	// Bindings that pin the remote to different keys leave it no one pin.
	rekeyed := f.settings
	rekeyed.HostKey += "x"
	f.p.o.Remotes = func() []Settings { return []Settings{f.settings, rekeyed} }
	if _, err := f.p.SiteRun(ctx, at, extension.SiteCommand{Argv: []string{"true"}}); !errors.Is(err, extension.ErrSiteUnavailable) {
		t.Errorf("a remote with two pins: %v, want ErrSiteUnavailable", err)
	}
	if _, err := f.p.Place(ctx, extension.ResourceOwner{ExecutionID: "x"}, extension.ContainerPolicy{Config: f.config}, ""); !errors.Is(err, extension.ErrSandboxUnavailable) {
		t.Errorf("placing on a remote with two pins: %v, want sandbox_unavailable", err)
	}
}
