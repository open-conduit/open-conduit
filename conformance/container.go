package conformance

import (
	"context"
	"fmt"
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
