package extension

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/open-conduit/open-conduit/domain"
)

// ContainerBinding is the host's half of one attempt's container: what
// BindContainer needs to check an executor's needs against the grant,
// create the container and pin it, and to prove a recovered one.
type ContainerBinding struct {
	// Provider and Config are the binding's container (Config's Data is a
	// ContainerConfig). A nil Provider binds a broker that refuses every
	// launch and can't adopt.
	Provider ContainerProvider
	Owner    ResourceOwner
	Config   domain.Payload
	// Host is this daemon's ContainerLabelHost value; Segment the open
	// segment's number, for the labels.
	Host    string
	Segment int
	// Protected, Shared, RunRoot, Toolbelt, Records and MaxScan are as in
	// SandboxBinding: the shared launch checks (GuardLaunch) use them.
	// Records also holds the record of every signal and stop the host
	// sends (RecordContainerSignal).
	Protected, Shared          []string
	RunRoot, Toolbelt, Records string
	MaxScan                    int
	// Home expands ~ in the grant.
	Home string
	// WorkHome is the Work's own home (ExecutionContext.ContainerHome):
	// granted read-write and an exception to the protected set for this
	// Work's containers only. Empty gives the container none.
	WorkHome string
	// Attach makes the broker's container an operator's attach rather than
	// a segment's: labelled ContainerSegmentAttach, and never pinned to a
	// segment (Pin records it in the attempt's history instead).
	Attach bool
	// Pin records a container with the attempt's open segment, durably,
	// before its argv is returned. An error removes the container and
	// fails the launch.
	Pin func(ContainerPin) error
	// Pinned is the container a recovering segment was launched in, nil
	// when it ran in none.
	Pinned *ContainerPin
	Now    func() time.Time
}

// BoundContainer is a ContainerBroker bound to one attempt. Its Resources
// wraps the attempt's resource broker as BoundSandbox's does.
type BoundContainer struct {
	b        ContainerBinding
	mu       sync.Mutex
	resource *Resource
	launched *ContainerPin
}

// BindContainer binds a container provider to one attempt. Hosts call it;
// executors only see the broker.
func BindContainer(b ContainerBinding) *BoundContainer {
	if b.Now == nil {
		b.Now = time.Now
	}
	return &BoundContainer{b: b, launched: b.Pinned}
}

// Resources wraps the attempt's resource broker: every acquisition is
// marked Sandboxed, and the resource acquired is kept for the launch.
func (c *BoundContainer) Resources(r ResourceBroker) ResourceBroker {
	if r == nil {
		return nil
	}
	return sandboxedResources{r, func(res *Resource) {
		c.mu.Lock()
		c.resource = res
		c.mu.Unlock()
	}}
}

// ContainerDigest pins a container plan: its image and effective policy.
func ContainerDigest(image string, data domain.Payload) string {
	b, _ := json.Marshal(struct {
		Image string         `json:"image"`
		Data  domain.Payload `json:"data"`
	}{image, data})
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func (c *BoundContainer) Launch(ctx context.Context, needs SandboxNeeds, l SandboxLaunch) (SandboxPlan, error) {
	b := c.b
	if b.Provider == nil {
		return SandboxPlan{}, fmt.Errorf("%w: the binding no longer declares a container", ErrSandboxUnavailable)
	}
	if len(l.Argv) == 0 || !filepath.IsAbs(l.Dir) || !filepath.IsAbs(l.RunDir) || b.Records == "" || b.Pin == nil || b.Host == "" {
		return SandboxPlan{}, fmt.Errorf("%w: incomplete launch", ErrSandboxUnavailable)
	}
	cfg, err := ParseContainerConfig(b.Config.Data)
	if err != nil {
		return SandboxPlan{}, fmt.Errorf("%w: %v", ErrSandboxUnavailable, err)
	}
	c.mu.Lock()
	res := c.resource
	c.mu.Unlock()
	g, err := GuardLaunch(LaunchGuard{Protected: b.Protected, Shared: b.Shared, RunRoot: b.RunRoot, Toolbelt: b.Toolbelt, Records: b.Records,
		MaxScan: b.MaxScan, Dir: l.Dir, RunDir: l.RunDir, Home: b.WorkHome, Resource: res, Needs: needs})
	if err != nil {
		return SandboxPlan{}, err
	}
	if err := containerGrant(cfg, g, b, needs); err != nil {
		return SandboxPlan{}, err
	}
	// No second container: the execution's containers are listed first,
	// and a listing that fails refuses the launch.
	listed, err := b.Provider.List(ctx, b.Owner)
	if err != nil {
		return SandboxPlan{}, fmt.Errorf("%w: could not list the execution's containers: %v", ErrSandboxUnavailable, err)
	}
	// An attach container left running (its terminal went away) is over
	// once a segment launches, since the Work is no longer held; a live
	// one blocks another attach only through the hold.
	for _, st := range listed {
		if st.Running && st.Ref.Labels[ContainerLabelSegment] != ContainerSegmentAttach {
			return SandboxPlan{}, fmt.Errorf("%w: %s", ErrContainerRunning, st.Ref.ID)
		}
	}
	for _, st := range listed {
		if err := b.Provider.Remove(ctx, st.Ref); err != nil {
			return SandboxPlan{}, fmt.Errorf("%w: could not remove the stopped container %s: %v", ErrSandboxUnavailable, st.Ref.ID, err)
		}
	}
	merged := needs.Merge(g.Resource)
	// A read-only need is held by a mount over it, which a symlink would
	// redirect: the link itself stays writable.
	for _, r := range merged.ReadOnly {
		if fi, err := os.Lstat(r); err == nil && fi.Mode()&os.ModeSymlink != 0 {
			return SandboxPlan{}, fmt.Errorf("%w: %s must be held read-only but is a symlink", ErrSandboxUnavailable, r)
		}
	}
	merged.Read = append(merged.Read, g.Shared...)
	merged.Deny = append(merged.Deny, g.Protected...)
	merged = canonicalNeeds(merged)
	if err := os.MkdirAll(b.Records, 0o700); err != nil {
		return SandboxPlan{}, fmt.Errorf("%w: %v", ErrSandboxUnavailable, err)
	}
	paths := SandboxPaths{Workspace: g.Workspace, Run: g.Run, Toolbelt: b.Toolbelt, Records: b.Records, Home: g.Home}
	l.Dir, l.RunDir = g.Workspace, g.Run
	plan, err := b.Provider.Plan(ctx, b.Owner, ContainerPolicy{Config: b.Config, Paths: paths}, merged, l)
	if err != nil {
		return SandboxPlan{}, err
	}
	if plan.Image != cfg.Image || len(plan.Data.Data) == 0 {
		return SandboxPlan{}, fmt.Errorf("%w: the provider returned an incomplete plan", ErrSandboxUnavailable)
	}
	plan.Digest = ContainerDigest(plan.Image, plan.Data)
	segment := strconv.Itoa(b.Segment)
	if b.Attach {
		segment = ContainerSegmentAttach
	}
	labels := map[string]string{
		ContainerLabelHost:      b.Host,
		ContainerLabelExecution: string(b.Owner.ExecutionID),
		ContainerLabelSegment:   segment,
		ContainerLabelDigest:    plan.Digest,
	}
	created, err := b.Provider.Create(ctx, plan, labels)
	if err != nil {
		return SandboxPlan{}, fmt.Errorf("%w: could not create the container: %v", ErrSandboxUnavailable, err)
	}
	if created.Ref.ID == "" || created.Daemon == "" || len(created.Argv) == 0 {
		_ = b.Provider.Remove(ctx, created.Ref)
		return SandboxPlan{}, fmt.Errorf("%w: the provider returned an incomplete container", ErrSandboxUnavailable)
	}
	pin := ContainerPin{Digest: plan.Digest, Summary: plan.Summary, Image: plan.Image, Daemon: created.Daemon, ID: created.Ref.ID, Run: g.Run, At: b.Now()}
	// A marker left by an earlier container would read as this one's.
	_ = os.Remove(filepath.Join(g.Run, ContainerTermMarker))
	if err := b.Pin(pin); err != nil {
		_ = b.Provider.Remove(ctx, created.Ref)
		return SandboxPlan{}, fmt.Errorf("%w: could not pin the container: %v", ErrSandboxUnavailable, err)
	}
	c.mu.Lock()
	c.launched = &pin
	c.mu.Unlock()
	return SandboxPlan{Argv: created.Argv, Env: created.Env, Digest: plan.Digest, Signals: SandboxTerm, Summary: plan.Summary, Data: plan.Data}, nil
}

// containerGrant checks the grant and the executor's needs against each
// other: the grant never reaches into the protected set except at the
// launch's own directories and what the host shares, and every need falls
// inside the grant. Nothing is visible in a container unless granted.
func containerGrant(cfg ContainerConfig, g GuardedLaunch, b ContainerBinding, needs SandboxNeeds) error {
	expand := func(list []string) []string {
		var out []string
		for _, s := range list {
			s = strings.NewReplacer("{workspace}", g.Workspace, "{run}", g.Run).Replace(s)
			if (s == "~" || strings.HasPrefix(s, "~/")) && b.Home != "" {
				s = b.Home + s[1:]
			}
			if filepath.IsAbs(s) {
				out = append(out, CanonicalPath(s))
			}
		}
		return out
	}
	write := append(expand(cfg.Write), g.Run)
	if g.Home != "" {
		write = append(write, g.Home)
	}
	read := append(append(append([]string{}, write...), expand(cfg.Read)...), g.Shared...)
	exceptions := append([]string{g.Workspace, g.Run}, g.Shared...)
	var refused []string
	for _, p := range append(expand(cfg.Write), expand(cfg.Read)...) {
		for _, d := range g.Protected {
			if (Within(p, []string{d}) || Within(d, []string{p})) && !Within(p, exceptions) {
				refused = append(refused, p+" reaches "+d)
			}
		}
	}
	if len(refused) > 0 {
		sort.Strings(refused)
		return fmt.Errorf("%w: the grant reaches into what Open Conduit protects: %s", ErrSandboxDenied, strings.Join(refused, ", "))
	}
	var denied []string
	for _, w := range needs.Write {
		if !Within(CanonicalPath(w), write) {
			denied = append(denied, "write "+w)
		}
	}
	for _, r := range append(append([]string{}, needs.Read...), needs.ReadOnly...) {
		if !Within(CanonicalPath(r), read) {
			denied = append(denied, "read "+r)
		}
	}
	for _, h := range needs.Hosts {
		if !HostAllowed(h, cfg.Network.Allow) {
			denied = append(denied, "host "+h)
		}
	}
	for _, s := range needs.Sockets {
		if b.Toolbelt == "" || CanonicalPath(s) != CanonicalPath(b.Toolbelt) {
			denied = append(denied, "socket "+s)
		}
	}
	if len(denied) > 0 {
		sort.Strings(denied)
		return fmt.Errorf("%w: %s", ErrSandboxDenied, strings.Join(denied, ", "))
	}
	return nil
}

// HostAllowed reports whether host (host or host:port) is in allow: the
// same host, or beneath a *.domain entry. An entry with a port allows only
// that port.
func HostAllowed(host string, allow []string) bool {
	name, port, hasPort := strings.Cut(host, ":")
	for _, a := range allow {
		an, ap, aPort := strings.Cut(a, ":")
		if aPort && (!hasPort || ap != port) {
			continue
		}
		if strings.EqualFold(an, name) {
			return true
		}
		if suffix, ok := strings.CutPrefix(an, "*."); ok && strings.HasSuffix(strings.ToLower(name), "."+strings.ToLower(suffix)) {
			return true
		}
	}
	return false
}

func (c *BoundContainer) Adopt(ctx context.Context, alive bool) (ContainerExit, error) {
	b := c.b
	pin := b.Pinned
	if pin == nil {
		return ContainerExit{}, fmt.Errorf("%w: the segment was launched without a container", ErrNotAdoptable)
	}
	if b.Provider == nil {
		return ContainerExit{}, fmt.Errorf("%w: the container provider is not available to prove the container", ErrNotAdoptable)
	}
	st, err := b.Provider.Inspect(ctx, ContainerRef{ID: pin.ID})
	if err != nil {
		return ContainerExit{}, errors.Join(ErrNotAdoptable, ErrContainerUnconfirmed, err)
	}
	if err := c.provePinned(st); err != nil {
		return ContainerExit{}, errors.Join(ErrNotAdoptable, err)
	}
	switch {
	case alive:
		return ContainerExit{}, nil
	case st.Running:
		// Never adopted without its wrapper: the host's Stop stops it
		// before a retry.
		return ContainerExit{}, fmt.Errorf("%w: the container runs without its wrapper", ErrNotAdoptable)
	}
	return ContainerExit{Exited: true, ExitCode: st.ExitCode, Interrupted: c.interrupted(*pin)}, nil
}

// provePinned proves st is the pinned container: on the pinned daemon, the
// pinned ID, this host's and execution's labels, the pinned plan and image,
// and started after it was pinned.
func (c *BoundContainer) provePinned(st ContainerState) error {
	pin := c.b.Pinned
	switch {
	case st.Daemon != pin.Daemon:
		return fmt.Errorf("%w: the container daemon is %q, not the pinned %q", ErrContainerUnconfirmed, st.Daemon, pin.Daemon)
	case st.Gone:
		return errors.New("the pinned container is gone")
	case st.Ref.ID != pin.ID:
		return errors.New("the container is not the pinned one")
	case st.Ref.Labels[ContainerLabelHost] != c.b.Host || st.Ref.Labels[ContainerLabelExecution] != string(c.b.Owner.ExecutionID) || st.Ref.Labels[ContainerLabelDigest] != pin.Digest:
		return errors.New("the container's labels are not the pinned ones")
	case st.Image != pin.Image:
		return errors.New("the container does not run the pinned image")
	case st.StartedAt.IsZero() || !st.StartedAt.After(pin.At):
		return errors.New("the container did not start after it was pinned")
	}
	return nil
}

// interrupted reports whether the pinned container's entry stub saw a TERM
// the host never sent.
func (c *BoundContainer) interrupted(pin ContainerPin) bool {
	if _, err := os.Stat(filepath.Join(pin.Run, ContainerTermMarker)); err != nil {
		return false
	}
	return !ContainerSignalled(c.b.Records, pin.ID)
}

// Settle is the host's last word on a segment's result: an exit after a
// TERM the host didn't send is interrupted, whatever its code. The
// executor's data and artifacts are kept.
func (c *BoundContainer) Settle(r Result) Result {
	c.mu.Lock()
	pin := c.launched
	c.mu.Unlock()
	if pin == nil || !c.interrupted(*pin) {
		return r
	}
	r.Outcome.Kind = domain.OutcomeFailed
	r.Outcome.Summary = "interrupted: the container was sent a TERM Open Conduit didn't send"
	r.RetryHint = nil
	return r
}

// RecordContainerSignal records, in the execution's container records, a
// signal or stop the host sends a container (term, kill, stop). It is a
// no-op where no container was launched. Hosts call it before sending.
func RecordContainerSignal(records, id, kind string) error {
	if records == "" || id == "" {
		return nil
	}
	if _, err := os.Stat(records); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	f, err := os.OpenFile(filepath.Join(records, signalFile(id)), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(f, kind)
	return errors.Join(err, f.Close())
}

// ContainerSignalled reports whether the host recorded sending the
// container a signal or a stop.
func ContainerSignalled(records, id string) bool {
	raw, err := os.ReadFile(filepath.Join(records, signalFile(id)))
	return err == nil && strings.TrimSpace(string(raw)) != ""
}

func signalFile(id string) string {
	sum := sha256.Sum256([]byte(id))
	return "signals-" + hex.EncodeToString(sum[:8])
}

// StopContainer is the host's stop for a pinned container, after the
// executor's own Stop: it stops the container and returns nil only once
// Inspect shows it isn't running. A container gone from the pinned daemon
// is confirmed not running. An unreachable daemon, or a different one,
// confirms nothing (ErrContainerUnconfirmed), so replacement stays blocked.
func StopContainer(ctx context.Context, p ContainerProvider, pin *ContainerPin, records string, grace time.Duration) error {
	if pin == nil || pin.ID == "" {
		return nil
	}
	if p == nil {
		return fmt.Errorf("%w: no container provider", ErrContainerUnconfirmed)
	}
	ref := ContainerRef{ID: pin.ID}
	running := func() (bool, error) {
		st, err := p.Inspect(ctx, ref)
		switch {
		case err != nil:
			return false, errors.Join(ErrContainerUnconfirmed, err)
		case st.Daemon != pin.Daemon:
			return false, fmt.Errorf("%w: the container daemon is %q, not the pinned %q", ErrContainerUnconfirmed, st.Daemon, pin.Daemon)
		}
		return !st.Gone && st.Running, nil
	}
	live, err := running()
	if err != nil || !live {
		return err
	}
	if err := RecordContainerSignal(records, pin.ID, "stop"); err != nil {
		return err
	}
	if err := p.Stop(ctx, ref, grace); err != nil {
		return err
	}
	if live, err = running(); err != nil {
		return err
	} else if live {
		return fmt.Errorf("%w: the container still runs after its stop", ErrContainerUnconfirmed)
	}
	return nil
}

// RemoveContainers removes every container of owner. The host calls it
// once the execution is confirmed stopped and before its workspace may be
// released; an error keeps the workspace.
func RemoveContainers(ctx context.Context, p ContainerProvider, owner ResourceOwner) error {
	listed, err := p.List(ctx, owner)
	if err != nil {
		return err
	}
	var errs []error
	for _, st := range listed {
		if st.Running {
			errs = append(errs, fmt.Errorf("%w: %s", ErrContainerRunning, st.Ref.ID))
			continue
		}
		errs = append(errs, p.Remove(ctx, st.Ref))
	}
	return errors.Join(errs...)
}

// ContainerMounts are the host paths live containers of owner mount, for a
// resource sweep's in-use check. A listing that fails is an error.
func ContainerMounts(ctx context.Context, p ContainerProvider, owner ResourceOwner) ([]string, error) {
	listed, err := p.List(ctx, owner)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, st := range listed {
		if st.Running {
			out = append(out, st.Mounts...)
		}
	}
	return out, nil
}

// ReconcileContainers removes this host's containers (and what the
// provider made for them) whose execution is terminal or gone. live
// reports, for the executions found, which are still live; one it leaves
// out is gone.
func ReconcileContainers(ctx context.Context, p ContainerProvider, host string, live func([]domain.ID) (map[domain.ID]bool, error)) error {
	listed, err := p.List(ctx, ResourceOwner{})
	if err != nil {
		return err
	}
	var ids []domain.ID
	seen := map[domain.ID]bool{}
	for _, st := range listed {
		id := domain.ID(st.Ref.Labels[ContainerLabelExecution])
		if st.Ref.Labels[ContainerLabelHost] == host && id != "" && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	alive, err := live(ids)
	if err != nil {
		return err
	}
	var errs []error
	for _, st := range listed {
		id := domain.ID(st.Ref.Labels[ContainerLabelExecution])
		if st.Ref.Labels[ContainerLabelHost] != host || id == "" || alive[id] {
			continue
		}
		// An operator's attach runs beside a finished execution until they
		// exit; their attach removes it then.
		if st.Running && st.Ref.Labels[ContainerLabelSegment] == ContainerSegmentAttach {
			continue
		}
		errs = append(errs, p.Remove(ctx, st.Ref))
	}
	return errors.Join(errs...)
}
