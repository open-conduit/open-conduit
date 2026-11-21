package conformance

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
)

// ContainerFixture is a container provider to check, with a launch it can
// plan and run to an exit, and the means to run a created container's
// argv as an executor's wrapper would.
type ContainerFixture struct {
	Provider extension.ContainerProvider
	// Host is the ContainerLabelHost value the provider lists for.
	Host string
	// Policy, Needs and Launch are a launch the provider can plan. Once
	// started, it exits with Code.
	Policy extension.ContainerPolicy
	Needs  extension.SandboxNeeds
	Launch extension.SandboxLaunch
	Code   int
	// Start runs a created container's argv and environment to its end and
	// returns its exit status.
	Start func(extension.ContainerCreated) int
}

// CheckContainerProvider checks the container contract the host's
// lifecycle relies on: a created container isn't running and hasn't
// started; it is listed for its execution and the host, and not for
// another execution; once started it reports its exit and when it
// started; it can be stopped once exited; removed, it is gone from the
// daemon that still answers, from listings and from a second removal.
func CheckContainerProvider(f ContainerFixture) error {
	ctx := context.Background()
	p := f.Provider
	const execution = "conformance-execution"
	owner := extension.ResourceOwner{ScopeID: "conformance-scope", WorkID: "conformance-work", ExecutionID: execution}
	plan, err := p.Plan(ctx, owner, f.Policy, f.Needs, f.Launch)
	if err != nil {
		return fmt.Errorf("plan: %v", err)
	}
	if plan.Image == "" || len(plan.Data.Data) == 0 {
		return fmt.Errorf("plan: no image or data")
	}
	plan.Digest = extension.ContainerDigest(plan.Image, plan.Data)
	labels := map[string]string{
		extension.ContainerLabelHost:      f.Host,
		extension.ContainerLabelExecution: execution,
		extension.ContainerLabelSegment:   "1",
		extension.ContainerLabelDigest:    plan.Digest,
	}
	before := time.Now()
	created, err := p.Create(ctx, plan, labels)
	if err != nil {
		return fmt.Errorf("create: %v", err)
	}
	if created.Ref.ID == "" || created.Daemon == "" || len(created.Argv) == 0 {
		return fmt.Errorf("create: incomplete container %+v", created)
	}
	ref := extension.ContainerRef{ID: created.Ref.ID}
	st, err := p.Inspect(ctx, ref)
	switch {
	case err != nil:
		return fmt.Errorf("inspect a created container: %v", err)
	case st.Gone || st.Running || !st.StartedAt.IsZero():
		return fmt.Errorf("a created container must exist, unstarted: %+v", st)
	case st.Daemon != created.Daemon:
		return fmt.Errorf("inspect names daemon %q, create %q", st.Daemon, created.Daemon)
	case st.Ref.ID != created.Ref.ID:
		return fmt.Errorf("inspect names %q for %q", st.Ref.ID, created.Ref.ID)
	case st.Image != plan.Image:
		return fmt.Errorf("inspect reports image %q, not the plan's %q", st.Image, plan.Image)
	}
	for k, v := range labels {
		if st.Ref.Labels[k] != v {
			return fmt.Errorf("label %s is %q, not %q", k, st.Ref.Labels[k], v)
		}
	}
	if err := listed(ctx, p, owner, created.Ref.ID, true); err != nil {
		return err
	}
	if err := listed(ctx, p, extension.ResourceOwner{}, created.Ref.ID, true); err != nil {
		return err
	}
	if err := listed(ctx, p, extension.ResourceOwner{ExecutionID: domain.ID("another-" + execution)}, created.Ref.ID, false); err != nil {
		return err
	}
	if code := f.Start(created); code != f.Code {
		return fmt.Errorf("the container's wrapper exited %d, not %d", code, f.Code)
	}
	st, err = p.Inspect(ctx, ref)
	switch {
	case err != nil:
		return fmt.Errorf("inspect an exited container: %v", err)
	case st.Gone || st.Running:
		return fmt.Errorf("an exited container must exist, not running: %+v", st)
	case st.ExitCode != f.Code:
		return fmt.Errorf("inspect reports exit %d, not %d", st.ExitCode, f.Code)
	case !st.StartedAt.After(before):
		return fmt.Errorf("inspect reports a start (%v) before the container was created (%v)", st.StartedAt, before)
	}
	if err := p.Stop(ctx, ref, 0); err != nil {
		return fmt.Errorf("stop an exited container: %v", err)
	}
	if err := p.Remove(ctx, ref); err != nil {
		return fmt.Errorf("remove: %v", err)
	}
	st, err = p.Inspect(ctx, ref)
	switch {
	case err != nil:
		return fmt.Errorf("inspect a removed container on a daemon that answers must be Gone, not an error: %v", err)
	case !st.Gone:
		return fmt.Errorf("a removed container must be gone: %+v", st)
	case st.Daemon != created.Daemon:
		return fmt.Errorf("a gone container's daemon is %q, not %q", st.Daemon, created.Daemon)
	}
	if err := p.Remove(ctx, ref); err != nil {
		return fmt.Errorf("removing a gone container is not an error: %v", err)
	}
	return listed(ctx, p, owner, created.Ref.ID, false)
}

// listed checks whether the owner's listing has the container.
func listed(ctx context.Context, p extension.ContainerProvider, owner extension.ResourceOwner, id string, want bool) error {
	states, err := p.List(ctx, owner)
	if err != nil {
		return fmt.Errorf("list: %v", err)
	}
	got := false
	for _, st := range states {
		got = got || st.Ref.ID == id
	}
	if got != want {
		return fmt.Errorf("the listing for execution %q has the container: %v, want %v", owner.ExecutionID, got, want)
	}
	return nil
}

// PlacerFixture is a container provider that places launches
// (extension.ContainerPlacer) to check, with a launch it can plan at a
// placement and a directory that exists at every site it places at. Its
// sites must have a POSIX sh.
type PlacerFixture struct {
	Provider interface {
		extension.ContainerProvider
		extension.ContainerPlacer
	}
	Policy extension.ContainerPolicy
	Needs  extension.SandboxNeeds
	Launch extension.SandboxLaunch
	Dir    string
	// Pwd is what a command run in Dir reports as its directory there; ""
	// for Dir itself (a fake site that prefixes the host's paths differs).
	Pwd string
}

// CheckContainerPlacer checks the placement contract the host relies on to
// make a workspace where a launch runs: a placement has an ID (without a
// slash, which SiteID uses) and a daemon; placing again preferring it
// gives it back; a command runs there in its directory, with its
// environment and stdin, and one that fails is its exit code, not an
// error; a placement the provider can't name is ErrSiteUnavailable; and a
// container planned at the placement is created on its daemon.
func CheckContainerPlacer(f PlacerFixture) error {
	ctx := context.Background()
	p := f.Provider
	owner := extension.ResourceOwner{ScopeID: "conformance-scope", WorkID: "conformance-work", ExecutionID: "conformance-placed"}
	at, err := p.Place(ctx, owner, extension.ContainerPolicy{Config: f.Policy.Config}, "")
	if err != nil {
		return fmt.Errorf("place: %v", err)
	}
	if at.ID == "" || at.Daemon == "" || strings.Contains(at.ID, "/") {
		return fmt.Errorf("place: incomplete placement %+v", at)
	}
	again, err := p.Place(ctx, owner, extension.ContainerPolicy{Config: f.Policy.Config}, at.ID)
	if err != nil {
		return fmt.Errorf("place again: %v", err)
	}
	if again.ID != at.ID || again.Daemon != at.Daemon {
		return fmt.Errorf("placing again preferring %q gave %+v", at.ID, again)
	}
	res, err := p.SiteRun(ctx, at.ID, extension.SiteCommand{Argv: []string{"/bin/sh", "-c", `printf '%s|%s|' "$(pwd)" "$CONFORMANCE"; cat; exit 3`}, Dir: f.Dir, Env: []string{"CONFORMANCE=site"}, Stdin: []byte("in")})
	if err != nil {
		return fmt.Errorf("site run: a command that ran and failed is an error: %v", err)
	}
	pwd := f.Pwd
	if pwd == "" {
		pwd = f.Dir
	}
	if want := pwd + "|site|in"; res.ExitCode != 3 || string(res.Stdout) != want {
		return fmt.Errorf("site run: exit %d, output %q; want 3 and %q", res.ExitCode, res.Stdout, want)
	}
	if _, err := p.SiteRun(ctx, at.ID+"-unknown", extension.SiteCommand{Argv: []string{"/bin/sh", "-c", "true"}}); !errors.Is(err, extension.ErrSiteUnavailable) {
		return fmt.Errorf("site run at a placement the provider never gave: %v, want ErrSiteUnavailable", err)
	}
	policy := f.Policy
	policy.Placement = at.ID
	plan, err := p.Plan(ctx, owner, policy, f.Needs, f.Launch)
	if err != nil {
		return fmt.Errorf("plan at the placement: %v", err)
	}
	plan.Digest = extension.ContainerDigest(plan.Image, plan.Data)
	created, err := p.Create(ctx, plan, map[string]string{extension.ContainerLabelExecution: string(owner.ExecutionID), extension.ContainerLabelSegment: "1", extension.ContainerLabelDigest: plan.Digest})
	if err != nil {
		return fmt.Errorf("create at the placement: %v", err)
	}
	defer func() { _ = p.Remove(ctx, created.Ref) }()
	if created.Daemon != at.Daemon {
		return fmt.Errorf("created on %q, not at the placement's %q", created.Daemon, at.Daemon)
	}
	return nil
}

// CapacityFixture is a pool container provider to check: Fill leaves it no
// room and Free gives it room again.
type CapacityFixture struct {
	Provider interface {
		extension.ContainerProvider
		extension.ContainerPlacer
	}
	Policy     extension.ContainerPolicy
	Needs      extension.SandboxNeeds
	Launch     extension.SandboxLaunch
	Fill, Free func()
}

// CheckContainerCapacity checks the "not now" a full pool gives (#702):
// with no room, Place is refused with extension.ErrNoCapacity, not a
// failure of the launch, and leaves no container; with room again, the
// same launch places and creates.
func CheckContainerCapacity(f CapacityFixture) error {
	ctx := context.Background()
	p := f.Provider
	owner := extension.ResourceOwner{ScopeID: "conformance-scope", WorkID: "conformance-work", ExecutionID: "conformance-pool"}
	config := extension.ContainerPolicy{Config: f.Policy.Config}
	f.Fill()
	if _, err := p.Place(ctx, owner, config, ""); !errors.Is(err, extension.ErrNoCapacity) {
		return fmt.Errorf("place on a full pool: %v, want ErrNoCapacity", err)
	}
	states, err := p.List(ctx, owner)
	if err != nil {
		return fmt.Errorf("list after a refusal: %v", err)
	}
	if len(states) != 0 {
		return fmt.Errorf("a refused launch left %d containers", len(states))
	}
	f.Free()
	at, err := p.Place(ctx, owner, config, "")
	if err != nil {
		return fmt.Errorf("place once there is room: %v", err)
	}
	policy := f.Policy
	policy.Placement = at.ID
	plan, err := p.Plan(ctx, owner, policy, f.Needs, f.Launch)
	if err != nil {
		return fmt.Errorf("plan once there is room: %v", err)
	}
	plan.Digest = extension.ContainerDigest(plan.Image, plan.Data)
	created, err := p.Create(ctx, plan, map[string]string{extension.ContainerLabelExecution: string(owner.ExecutionID), extension.ContainerLabelSegment: "1", extension.ContainerLabelDigest: plan.Digest})
	if err != nil {
		return fmt.Errorf("create once there is room: %v", err)
	}
	return p.Remove(ctx, created.Ref)
}

// CollectorFixture is a container provider that collects
// (extension.ContainerCollector) to check, with a launch that, once
// started, writes Content to File in its write grant and exits with Code,
// and the means to start a created container and lose its wrapper while
// the run still runs.
type CollectorFixture struct {
	Provider interface {
		extension.ContainerProvider
		extension.ContainerCollector
	}
	// Host is the ContainerLabelHost value the provider lists for.
	Host string
	// Policy, Needs and Launch are a launch the provider can plan;
	// Policy.Paths names its run and records directories.
	Policy extension.ContainerPolicy
	Needs  extension.SandboxNeeds
	Launch extension.SandboxLaunch
	Code   int
	// File is a host path in the launch's write grant, and Content what
	// the run writes there.
	File, Content string
	// Orphan starts a created container's argv as its wrapper, then kills
	// the wrapper while the run still runs (as a daemon's machine
	// restarting takes it), and returns once the run has gone on to its
	// exit without it.
	Orphan func(extension.ContainerCreated) error
}

// CheckContainerCollector checks that a run whose wrapper died still
// brings its work back to the host (#725): with the wrapper killed
// mid-run and the container left to exit, the run's file is on the host
// once Adopt returns the container's exit, and once the host removes a
// container it never adopted. Collecting again changes nothing, so a
// change the host made since stays, and a removed container collects as
// nothing.
func CheckContainerCollector(f CollectorFixture) error {
	ctx := context.Background()
	orphaned := func(execution string) (extension.ResourceOwner, extension.ContainerPin, error) {
		owner := extension.ResourceOwner{ScopeID: "conformance-scope", WorkID: "conformance-work", ExecutionID: domain.ID(execution)}
		if err := os.Remove(f.File); err != nil && !errors.Is(err, os.ErrNotExist) {
			return owner, extension.ContainerPin{}, err
		}
		plan, err := f.Provider.Plan(ctx, owner, f.Policy, f.Needs, f.Launch)
		if err != nil {
			return owner, extension.ContainerPin{}, fmt.Errorf("plan: %v", err)
		}
		plan.Digest = extension.ContainerDigest(plan.Image, plan.Data)
		before := time.Now()
		created, err := f.Provider.Create(ctx, plan, map[string]string{
			extension.ContainerLabelHost:      f.Host,
			extension.ContainerLabelExecution: execution,
			extension.ContainerLabelSegment:   "1",
			extension.ContainerLabelDigest:    plan.Digest,
		})
		if err != nil {
			return owner, extension.ContainerPin{}, fmt.Errorf("create: %v", err)
		}
		pin := extension.ContainerPin{Digest: plan.Digest, Image: plan.Image, Daemon: created.Daemon, ID: created.Ref.ID, Run: f.Policy.Paths.Run, At: before}
		if err := f.Orphan(created); err != nil {
			return owner, pin, fmt.Errorf("start the run and kill its wrapper: %v", err)
		}
		st, err := f.Provider.Inspect(ctx, extension.ContainerRef{ID: pin.ID})
		switch {
		case err != nil:
			return owner, pin, fmt.Errorf("inspect the orphaned run: %v", err)
		case st.Gone || st.Running || st.StartedAt.IsZero():
			return owner, pin, fmt.Errorf("the orphaned run must have run and exited, there to collect: %+v", st)
		case st.ExitCode != f.Code:
			return owner, pin, fmt.Errorf("the orphaned run exited %d, not %d", st.ExitCode, f.Code)
		}
		return owner, pin, nil
	}
	has := func(when string) error {
		raw, err := os.ReadFile(f.File)
		if err != nil || string(raw) != f.Content {
			return fmt.Errorf("%s, the run's file is %q (%v), want %q: its work was not collected", when, raw, err, f.Content)
		}
		return nil
	}

	// Adopted: the exit stands for the segment's only once its work is
	// back.
	owner, pin, err := orphaned("conformance-collect-adopt")
	if err != nil {
		return err
	}
	bound := extension.BindContainer(extension.ContainerBinding{Provider: f.Provider, Owner: owner, Config: f.Policy.Config, Host: f.Host, Records: f.Policy.Paths.Records, Pinned: &pin})
	exit, err := bound.Adopt(ctx, false)
	if err != nil || !exit.Exited || exit.ExitCode != f.Code {
		return fmt.Errorf("adopt the exited run: %+v %v, want its exit %d", exit, err, f.Code)
	}
	if err := has("once Adopt returned its exit"); err != nil {
		return err
	}
	// A second collect changes nothing: what the host did since stays.
	const host = "changed on the host since"
	if err := os.WriteFile(f.File, []byte(host), 0o644); err != nil {
		return err
	}
	if err := f.Provider.Collect(ctx, extension.ContainerRef{ID: pin.ID}); err != nil {
		return fmt.Errorf("collect a collected run again: %v", err)
	}
	if raw, _ := os.ReadFile(f.File); string(raw) != host {
		return fmt.Errorf("collecting again undid the host's change: the file is %q", raw)
	}
	if err := extension.RemoveContainers(ctx, f.Provider, owner); err != nil {
		return fmt.Errorf("remove the adopted run: %v", err)
	}
	if raw, _ := os.ReadFile(f.File); string(raw) != host {
		return fmt.Errorf("removing a collected run undid the host's change: the file is %q", raw)
	}

	// Removed without being adopted: its work comes back first.
	owner, pin, err = orphaned("conformance-collect-remove")
	if err != nil {
		return err
	}
	if err := extension.RemoveContainers(ctx, f.Provider, owner); err != nil {
		return fmt.Errorf("remove the exited run: %v", err)
	}
	if err := has("once the host removed it"); err != nil {
		return err
	}
	if st, err := f.Provider.Inspect(ctx, extension.ContainerRef{ID: pin.ID}); err != nil || !st.Gone {
		return fmt.Errorf("the removed run is %+v, %v, want gone", st, err)
	}
	if err := f.Provider.Collect(ctx, extension.ContainerRef{ID: pin.ID}); err != nil {
		return fmt.Errorf("collect a removed run: %v, want nothing to collect", err)
	}
	return nil
}
