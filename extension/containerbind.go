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
	"slices"
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
	// FormerHosts are labels this daemon made containers under before
	// (ContainerHosts.Former): a recovering segment's pinned container
	// may carry one.
	FormerHosts []string
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
	// Workspace, for an attach, is the workspace the attempt's last
	// segment had at its site, from that segment's pin (Workspace and
	// Site): an attach acquires nothing, so the host names it. The launch
	// must be placed at that site and be in that workspace, as a
	// segment's must be in the one it acquired. Nil for a workspace on
	// the host.
	Workspace *Resource
	// Pin records a container with the attempt's open segment, durably,
	// before its argv is returned. An error removes the container and
	// fails the launch.
	Pin func(ContainerPin) error
	// Pinned is the container a recovering segment was launched in, nil
	// when it ran in none.
	Pinned *ContainerPin
	Now    func() time.Time
	// Phase, when set, records a step of the launch (the container's plan
	// and its creation) for the attempt's history, as SandboxBinding's does.
	Phase func(name string) func(error)
	// Name is the binding's container provider's name, for the IDs of the
	// sites it places at (SiteID).
	Name string
	// Prefer is the placement the Work last ran at, "" for none: passed to
	// a ContainerPlacer's Place, so a Work goes back to its workspace.
	Prefer string
	// Placed, when set, is told each placement Place gives, before
	// anything is acquired there, so the host can prefer it next time.
	Placed func(Placement)
	// TokenFile is the execution's toolbelt token file, given with
	// Toolbelt to commands run at its site (PlacedSite).
	TokenFile string
	// Env is the run's environment layer from the extensions
	// (ExecutionContext.Env), given to commands run at its site
	// (PlacedSite.Env), so git there reaches the repository as the run's
	// does.
	Env map[string]string
	// Withheld, when set, gives the credentials of the connection the
	// container's config names (ContainerConfig.Connection), which the host
	// resolves for the launch. A
	// placement, plan or created container that carries one is refused
	// as ErrSandboxDenied, and a created one removed: they reach the run,
	// its history and the container's labels (ContainerCreated).
	Withheld func() []string
	// AtSite binds the attempt's resource provider again with the site
	// the execution was placed at (BindResourcesAt), so its acquisition
	// can make the workspace there. Nil: the resource is always made on
	// the host.
	AtSite func(Site) ResourceBroker
}

// BoundContainer is a ContainerBroker bound to one attempt. Its Resources
// wraps the attempt's resource broker as BoundSandbox's does.
type BoundContainer struct {
	b        ContainerBinding
	mu       sync.Mutex
	resource *Resource
	launched *ContainerPin
	placed   *Placement
	refused  *NoCapacityError
}

// noCapacity records a provider's ErrNoCapacity refusal, so the host can
// return the claim whatever the executor makes of the error, and returns
// the error to give the executor. A refusal whose reason carries a
// credential the provider resolved is refused as any other output would
// be. The caller holds c.mu when locked is true.
func (c *BoundContainer) noCapacity(err error, locked bool) error {
	refusal := &NoCapacityError{}
	var typed *NoCapacityError
	if errors.As(err, &typed) {
		*refusal = *typed
	}
	if denied := c.withheld("refusal", refusal.Reason); denied != nil {
		return denied
	}
	if !locked {
		c.mu.Lock()
		defer c.mu.Unlock()
	}
	c.refused = refusal
	return err
}

// Refusal is the provider's ErrNoCapacity refusal of this attempt's launch,
// nil when there was none or a later launch of the attempt got a container:
// a refusal means nothing was started.
func (c *BoundContainer) Refusal() *NoCapacityError {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.launched != nil {
		return nil
	}
	return c.refused
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
// marked Sandboxed, and the resource acquired is kept for the launch. When
// the provider places launches (ContainerPlacer), the execution is placed
// first and every acquisition is given the placement as its Site, so the
// resource provider can make the workspace where the launch will run.
func (c *BoundContainer) Resources(r ResourceBroker) ResourceBroker {
	if r == nil {
		return nil
	}
	return placedResources{c: c, r: r}
}

// placedResources places the execution before the resource broker it
// wraps is called, and marks every request Sandboxed.
type placedResources struct {
	c *BoundContainer
	r ResourceBroker
}

func (p placedResources) sandboxed(r ResourceBroker) ResourceBroker {
	return sandboxedResources{r, func(res *Resource) {
		p.c.mu.Lock()
		p.c.resource = res
		p.c.mu.Unlock()
	}}
}

func (p placedResources) Acquire(ctx context.Context, req ResourceRequest) (Resource, error) {
	site, err := p.c.site(ctx, p.c.b.Prefer)
	if err != nil {
		return Resource{}, err
	}
	broker := p.r
	if site != nil && p.c.b.AtSite != nil {
		broker = p.c.b.AtSite(site)
	}
	res, err := p.sandboxed(broker).Acquire(ctx, req)
	if err == nil {
		err = p.c.sameSite(res)
	}
	return res, err
}

// Lookup finds the lease, then places the execution where its resource
// is: a resource at a site the provider no longer places at can't be
// used, and the executor acquires again.
func (p placedResources) Lookup(ctx context.Context, ref string) (Resource, error) {
	res, err := p.sandboxed(p.r).Lookup(ctx, ref)
	if err != nil {
		return res, err
	}
	prefer := p.c.b.Prefer
	if _, placement, ok := SplitSiteID(res.Site); ok {
		prefer = placement
	}
	if _, err := p.c.site(ctx, prefer); err != nil {
		return Resource{}, err
	}
	return res, p.c.sameSite(res)
}

// sameSite refuses a resource made at another site than the execution is
// placed at, or at a site when it is placed at none.
func (c *BoundContainer) sameSite(res Resource) error {
	if res.Site == "" {
		return nil
	}
	c.mu.Lock()
	at := c.placed
	c.mu.Unlock()
	if at == nil || SiteID(c.b.Name, at.ID) != res.Site {
		return fmt.Errorf("%w: the workspace is at %s, where this launch isn't placed", ErrSandboxUnavailable, res.Site)
	}
	return nil
}

// site places the execution once, preferring prefer, and returns the
// placement as a Site; nil when the provider doesn't place.
func (c *BoundContainer) site(ctx context.Context, prefer string) (Site, error) {
	placer, ok := c.b.Provider.(ContainerPlacer)
	if !ok || c.b.Provider == nil {
		return nil, nil
	}
	at, err := c.place(ctx, placer, prefer)
	if err != nil {
		return nil, err
	}
	site := PlacedSite{Provider: c.b.Name, Placer: placer, At: at, Env: c.b.Env}
	if c.b.Toolbelt != "" && c.b.TokenFile != "" {
		site.Toolbelt, site.TokenFile = c.b.Toolbelt, c.b.TokenFile
	}
	return site, nil
}

func (c *BoundContainer) place(ctx context.Context, placer ContainerPlacer, prefer string) (Placement, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.placed != nil {
		return *c.placed, nil
	}
	if c.b.Name == "" {
		return Placement{}, fmt.Errorf("%w: the container binding has no provider name", ErrSandboxUnavailable)
	}
	end := phase(c.b.Phase, "container place")
	at, err := placer.Place(ctx, c.b.Owner, ContainerPolicy{Config: c.b.Config}, prefer)
	if err == nil && (at.ID == "" || at.Daemon == "" || strings.Contains(at.ID, "/")) {
		err = fmt.Errorf("%w: the provider returned an incomplete placement", ErrSandboxUnavailable)
	}
	if err == nil {
		err = c.withheld("placement", at.ID, at.Daemon, at.Summary)
	}
	end(err)
	if err != nil {
		if errors.Is(err, ErrNoCapacity) {
			return Placement{}, c.noCapacity(err, true)
		}
		if errors.Is(err, ErrSandboxUnavailable) || errors.Is(err, ErrSandboxDenied) {
			return Placement{}, err
		}
		return Placement{}, fmt.Errorf("%w: could not place the launch: %v", ErrSandboxUnavailable, err)
	}
	c.placed = &at
	if c.b.Placed != nil {
		c.b.Placed(at)
	}
	return at, nil
}

// minWithheld is the shortest credential withheld checks for: a shorter
// value would match ordinary text, refusing launches that carry none.
const minWithheld = 8

// withheld refuses what the provider returned when any of texts carries a
// credential its package resolved (ContainerBinding.Withheld). The error
// names what carried it, never the credential.
func (c *BoundContainer) withheld(what string, texts ...string) error {
	if c.b.Withheld == nil {
		return nil
	}
	for _, v := range c.b.Withheld() {
		if len(v) < minWithheld {
			continue
		}
		// The plan's data is JSON, where the credential may be escaped.
		forms := []string{v}
		if raw, err := json.Marshal(v); err == nil && string(raw[1:len(raw)-1]) != v {
			forms = append(forms, string(raw[1:len(raw)-1]))
		}
		for _, t := range texts {
			if slices.ContainsFunc(forms, func(f string) bool { return strings.Contains(t, f) }) {
				return fmt.Errorf("%w: the container provider put a credential it resolved for its connection in the %s; a provider keeps its credentials out of what reaches the run", ErrSandboxDenied, what)
			}
		}
	}
	return nil
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
	// A provider that places launches places this one before it plans,
	// if acquiring a workspace didn't already.
	var placement *Placement
	if placer, ok := b.Provider.(ContainerPlacer); ok {
		at, err := c.place(ctx, placer, b.Prefer)
		if err != nil {
			return SandboxPlan{}, err
		}
		placement = &at
	}
	c.mu.Lock()
	res := c.resource
	c.mu.Unlock()
	if res == nil && b.Attach && b.Workspace != nil {
		res = b.Workspace
	}
	atSite := res != nil && res.Site != ""
	if atSite {
		if err := c.sameSite(*res); err != nil {
			return SandboxPlan{}, err
		}
	}
	g, err := GuardLaunch(LaunchGuard{Protected: b.Protected, Shared: b.Shared, RunRoot: b.RunRoot, Toolbelt: b.Toolbelt, Records: b.Records,
		MaxScan: b.MaxScan, Dir: l.Dir, RunDir: l.RunDir, Home: b.WorkHome, Resource: res, Needs: needs, AtSite: atSite})
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
	// What another execution of the Work left (an attempt whose recovery
	// failed, before its retry) comes back to the workspace before this
	// one runs there; while any of it can't be collected, nothing launches.
	if err := c.collectWork(ctx); err != nil {
		return SandboxPlan{}, err
	}
	// What a stopped container wrote comes back before it goes; one that
	// can't be collected is kept, and nothing launches beside it.
	for _, st := range listed {
		if err := RemoveContainer(ctx, b.Provider, st.Ref); err != nil {
			return SandboxPlan{}, fmt.Errorf("%w: could not remove the stopped container %s: %w", ErrSandboxUnavailable, st.Ref.ID, err)
		}
	}
	merged := needs.Merge(g.Resource)
	// A read-only need is held by a mount over it, which a symlink would
	// redirect: the link itself stays writable. One in a workspace at the
	// site is the site's to hold.
	for _, r := range merged.ReadOnly {
		if atSite && Within(r, []string{g.Workspace}) {
			continue
		}
		if fi, err := os.Lstat(r); err == nil && fi.Mode()&os.ModeSymlink != 0 {
			return SandboxPlan{}, fmt.Errorf("%w: %s must be held read-only but is a symlink", ErrSandboxUnavailable, r)
		}
	}
	merged.Read = append(merged.Read, g.Shared...)
	merged.Deny = append(merged.Deny, g.Protected...)
	merged = canonicalNeedsAt(merged, siteRoot(atSite, g.Workspace))
	if err := os.MkdirAll(b.Records, 0o700); err != nil {
		return SandboxPlan{}, fmt.Errorf("%w: %v", ErrSandboxUnavailable, err)
	}
	paths := SandboxPaths{Workspace: g.Workspace, Run: g.Run, Toolbelt: b.Toolbelt, Records: b.Records, Home: g.Home}
	if atSite {
		paths.Site = res.Site
	}
	// The container mounts canonical paths, so the launch names them too:
	// a path through a symbolic link (macOS's /tmp) isn't there inside. A
	// workspace at the site is named as its provider wrote it.
	roots := append([]string{l.RunDir, b.WorkHome, b.Toolbelt}, b.Shared...)
	if !atSite {
		roots = append(roots, l.Dir)
	}
	for _, list := range [][]string{needs.Read, needs.Write, needs.ReadOnly} {
		for _, p := range list {
			// A need in a workspace at the site names a path there,
			// which nothing on the host resolves.
			if atSite && Within(filepath.Clean(p), []string{g.Workspace}) {
				continue
			}
			roots = append(roots, p)
		}
	}
	l = canonicalLaunch(l, roots)
	l.Dir, l.RunDir = g.Workspace, g.Run
	end := phase(b.Phase, "container plan")
	policy := ContainerPolicy{Config: b.Config, Paths: paths}
	if placement != nil {
		policy.Placement = placement.ID
	}
	plan, err := b.Provider.Plan(ctx, b.Owner, policy, merged, l)
	end(err)
	if errors.Is(err, ErrNoCapacity) {
		return SandboxPlan{}, c.noCapacity(err, false)
	}
	if err != nil {
		return SandboxPlan{}, err
	}
	if plan.Image != cfg.Image || len(plan.Data.Data) == 0 {
		return SandboxPlan{}, fmt.Errorf("%w: the provider returned an incomplete plan", ErrSandboxUnavailable)
	}
	if err := c.withheld("plan", plan.Image, plan.Summary, string(plan.Data.Data)); err != nil {
		return SandboxPlan{}, err
	}
	plan.Digest = ContainerDigest(plan.Image, plan.Data)
	segment := strconv.Itoa(b.Segment)
	if b.Attach {
		segment = ContainerSegmentAttach
	}
	labels := map[string]string{
		ContainerLabelHost:      b.Host,
		ContainerLabelExecution: string(b.Owner.ExecutionID),
		ContainerLabelWork:      string(b.Owner.WorkID),
		ContainerLabelSegment:   segment,
		ContainerLabelDigest:    plan.Digest,
	}
	end = phase(b.Phase, "container create")
	created, err := b.Provider.Create(ctx, plan, labels)
	end(err)
	if errors.Is(err, ErrNoCapacity) {
		return SandboxPlan{}, c.noCapacity(err, false)
	}
	if err != nil {
		return SandboxPlan{}, fmt.Errorf("%w: could not create the container: %v", ErrSandboxUnavailable, err)
	}
	if created.Ref.ID == "" || created.Daemon == "" || len(created.Argv) == 0 {
		_ = b.Provider.Remove(ctx, created.Ref)
		return SandboxPlan{}, fmt.Errorf("%w: the provider returned an incomplete container", ErrSandboxUnavailable)
	}
	carried := append([]string{created.Ref.ID, created.Daemon}, created.Argv...)
	carried = append(carried, created.Env...)
	for k, v := range created.Ref.Labels {
		carried = append(carried, k, v)
	}
	if err := c.withheld("container", carried...); err != nil {
		_ = b.Provider.Remove(ctx, created.Ref)
		return SandboxPlan{}, err
	}
	// The container must be where the execution was placed, which is
	// where its workspace was made.
	if placement != nil && created.Daemon != placement.Daemon {
		_ = b.Provider.Remove(ctx, created.Ref)
		return SandboxPlan{}, fmt.Errorf("%w: the container was created on %s, not at its placement %s", ErrSandboxUnavailable, created.Daemon, placement.Daemon)
	}
	pin := ContainerPin{Digest: plan.Digest, Summary: plan.Summary, Image: plan.Image, Daemon: created.Daemon, ID: created.Ref.ID, Run: g.Run, At: b.Now()}
	if atSite {
		pin.Workspace, pin.Site = g.Workspace, res.Site
	}
	// A marker left by an earlier container would read as this one's.
	_ = os.Remove(filepath.Join(g.Run, ContainerTermMarker))
	if err := b.Pin(pin); err != nil {
		_ = b.Provider.Remove(ctx, created.Ref)
		return SandboxPlan{}, fmt.Errorf("%w: could not pin the container: %v", ErrSandboxUnavailable, err)
	}
	c.mu.Lock()
	c.launched = &pin
	c.mu.Unlock()
	out := SandboxPlan{Argv: created.Argv, Env: created.Env, Digest: plan.Digest, Signals: SandboxTerm, Summary: plan.Summary, Data: plan.Data}
	// The workspace isn't on the host, so the wrapper runs in the run
	// directory, which is.
	if atSite {
		out.Dir = g.Run
	}
	return out, nil
}

// collectWork collects every stopped container of the Work's other
// executions (ContainerLabelWork) that this host made, when the provider
// collects: they write to the Work's workspace, which this launch's
// execution shares, so what they wrote must be back before it runs, and
// never arrive over what it writes. A collected container changes
// nothing, so this costs a listing once each is back; its removal stays
// with the removal paths. One that can't be collected refuses the launch,
// so a retry waits behind it.
func (c *BoundContainer) collectWork(ctx context.Context) error {
	b := c.b
	if !Collects(b.Provider) || b.Owner.WorkID == "" {
		return nil
	}
	listed, err := b.Provider.List(ctx, ResourceOwner{})
	if err != nil {
		return fmt.Errorf("%w: could not list the Work's containers: %v", ErrSandboxUnavailable, err)
	}
	hosts := ContainerHosts{Host: b.Host, Former: b.FormerHosts}
	for _, st := range listed {
		l := st.Ref.Labels
		if st.Running || !hosts.Owns(l[ContainerLabelHost]) || l[ContainerLabelWork] != string(b.Owner.WorkID) || l[ContainerLabelExecution] == string(b.Owner.ExecutionID) {
			continue
		}
		if err := CollectContainer(ctx, b.Provider, st.Ref); err != nil {
			return fmt.Errorf("%w: another run of the Work left %s uncollected: %w", ErrSandboxUnavailable, st.Ref.ID, err)
		}
	}
	return nil
}

// AttachWorkspace is where an attach to the Work's session goes under
// container provider provider, from pin, the attempt's last segment's
// container: when that segment's workspace was at a site, the placement
// to prefer and the workspace there (ContainerBinding.Workspace), with
// no needs of its own, as an attach on the host has none. Otherwise it is
// fallback (the Work's last placement) and no workspace. A workspace at
// another provider's site is refused as ErrSandboxUnavailable: the
// binding's provider can't place the attach there.
func AttachWorkspace(pin *ContainerPin, provider, fallback string) (string, *Resource, error) {
	if pin == nil || pin.Site == "" {
		return fallback, nil, nil
	}
	p, at, ok := SplitSiteID(pin.Site)
	if !ok || p != provider || pin.Workspace == "" {
		return "", nil, fmt.Errorf("%w: the Work's workspace is at %s, which the binding's container provider no longer places at", ErrSandboxUnavailable, pin.Site)
	}
	return at, &Resource{Kind: "workspace", Dir: pin.Workspace, Site: pin.Site, Sandbox: &SandboxNeeds{}}, nil
}

// siteRoot is the workspace at a site, whose paths canonicalNeeds leaves
// as written; "" when the workspace is on the host.
func siteRoot(atSite bool, workspace string) string {
	if atSite {
		return workspace
	}
	return ""
}

// canonicalLaunch rewrites each argv element and environment value that is
// one of roots, or beneath one, to its canonical path.
func canonicalLaunch(l SandboxLaunch, roots []string) SandboxLaunch {
	type alias struct{ from, to string }
	var aliases []alias
	for _, r := range roots {
		if r == "" || !filepath.IsAbs(r) {
			continue
		}
		r = filepath.Clean(r)
		if c := CanonicalPath(r); c != r {
			aliases = append(aliases, alias{r, c})
		}
	}
	if len(aliases) == 0 {
		return l
	}
	// The longest first, so a path is rewritten by its nearest root.
	sort.Slice(aliases, func(i, j int) bool { return len(aliases[i].from) > len(aliases[j].from) })
	rewrite := func(s string) string {
		prefix := ""
		if rest, ok := strings.CutPrefix(s, "unix://"); ok {
			prefix, s = "unix://", rest
		}
		for _, a := range aliases {
			if s == a.from || strings.HasPrefix(s, a.from+string(filepath.Separator)) {
				return prefix + a.to + s[len(a.from):]
			}
		}
		return prefix + s
	}
	argv := make([]string, len(l.Argv))
	for i, a := range l.Argv {
		argv[i] = rewrite(a)
	}
	env := make([]string, len(l.Env))
	for i, kv := range l.Env {
		k, v, _ := strings.Cut(kv, "=")
		env[i] = k + "=" + rewrite(v)
	}
	l.Argv, l.Env = argv, env
	return l
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
	// The run's output comes back before its exit stands for the
	// segment's: one that can't be collected is not adopted, so the
	// attempt is never settled as if the run had ended cleanly with its
	// work left behind, and the container stays to be collected again.
	if err := CollectContainer(ctx, b.Provider, st.Ref); err != nil {
		return ContainerExit{}, errors.Join(ErrNotAdoptable, err)
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
	case !(ContainerHosts{Host: c.b.Host, Former: c.b.FormerHosts}).Owns(st.Ref.Labels[ContainerLabelHost]) || st.Ref.Labels[ContainerLabelExecution] != string(c.b.Owner.ExecutionID) || st.Ref.Labels[ContainerLabelDigest] != pin.Digest:
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

// RemoveContainers removes every container of owner, each collected first
// (RemoveContainer). The host calls it once the execution is confirmed
// stopped and before its workspace may be released; an error keeps the
// workspace, and a container that can't be collected stays.
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
		errs = append(errs, RemoveContainer(ctx, p, st.Ref))
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

// ContainerKeep is how much of an execution's containers (and what the
// provider made beside them) reconcile keeps.
type ContainerKeep int

const (
	// ContainerKeepNone removes them all, but an attach still running: the
	// execution is over or gone.
	ContainerKeepNone ContainerKeep = iota
	// ContainerKeepAttach removes all but its attach's: the execution runs
	// no segment (it is parked), but an operator may be attaching to it,
	// and an attach being launched has nothing running yet. The attach's
	// own detach, or the wake's launch, removes those.
	ContainerKeepAttach
	// ContainerKeepAll keeps them all: the execution may be running a
	// segment, or about to, and recovery may adopt a stopped container.
	ContainerKeepAll
)

// ReconcileContainers removes this host's containers (and what the
// provider made for them, which its host-wide listing reports after them)
// whose execution is not live, and returns how many entries it removed.
// live reports, for the executions found, which are still live; one it
// leaves out is gone. It is ReconcileContainersKeeping with live as
// ContainerKeepAll and anything else as ContainerKeepNone.
func ReconcileContainers(ctx context.Context, p ContainerProvider, host string, live func([]domain.ID) (map[domain.ID]bool, error)) (int, error) {
	return ReconcileContainersKeeping(ctx, p, host, func(ids []domain.ID) (map[domain.ID]ContainerKeep, error) {
		alive, err := live(ids)
		keep := map[domain.ID]ContainerKeep{}
		for id, ok := range alive {
			if ok {
				keep[id] = ContainerKeepAll
			}
		}
		return keep, err
	})
}

// ReconcileContainersKeeping removes this host's containers, and what the
// provider made for them, as keep says for each execution found; one it
// leaves out is gone (ContainerKeepNone). It returns how many entries it
// removed. A running attach is always kept: an operator's attach runs
// beside a finished execution until they exit, and their detach removes
// it then. It is ReconcileHostContainers with no former labels.
func ReconcileContainersKeeping(ctx context.Context, p ContainerProvider, host string, keep func([]domain.ID) (map[domain.ID]ContainerKeep, error)) (int, error) {
	return ReconcileHostContainers(ctx, p, ContainerHosts{Host: host}, keep)
}

// ContainerHosts are the host labels (ContainerLabelHost) a daemon owns:
// Host, its installation's, which it labels every new container with, and
// Former, labels it made containers under before (an upgrade's path hash),
// still recognised but never given to a new container.
type ContainerHosts struct {
	Host   string
	Former []string
}

// Owns reports whether label is one of the daemon's.
func (h ContainerHosts) Owns(label string) bool {
	return label != "" && (label == h.Host || h.former(label))
}

func (h ContainerHosts) former(label string) bool {
	for _, f := range h.Former {
		if f != "" && f == label {
			return true
		}
	}
	return false
}

// reconcileStopGrace is how long reconcile gives a running container of an
// execution that is over before it is killed, when its provider collects.
const reconcileStopGrace = 10 * time.Second

// ReconcileHostContainers is ReconcileContainersKeeping over every label
// the daemon owns. Under its own label, an execution keep leaves out is
// gone. Under a former label only an execution keep reports is the
// daemon's: another installation once shared that label (a state
// directory at the same path), so an execution it doesn't know is left
// alone. Each container is collected before it is removed
// (RemoveContainer); one that can't be collected stays, and is an error.
// It returns how many entries it removed.
func ReconcileHostContainers(ctx context.Context, p ContainerProvider, hosts ContainerHosts, keep func([]domain.ID) (map[domain.ID]ContainerKeep, error)) (int, error) {
	listed, err := p.List(ctx, ResourceOwner{})
	if err != nil {
		return 0, err
	}
	var ids []domain.ID
	seen := map[domain.ID]bool{}
	for _, st := range listed {
		id := domain.ID(st.Ref.Labels[ContainerLabelExecution])
		if hosts.Owns(st.Ref.Labels[ContainerLabelHost]) && id != "" && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return 0, nil
	}
	kept, err := keep(ids)
	if err != nil {
		return 0, err
	}
	removed := 0
	var errs []error
	for _, st := range listed {
		id := domain.ID(st.Ref.Labels[ContainerLabelExecution])
		label := st.Ref.Labels[ContainerLabelHost]
		if !hosts.Owns(label) || id == "" {
			continue
		}
		former := label != hosts.Host
		attach := st.Ref.Labels[ContainerLabelSegment] == ContainerSegmentAttach
		k, known := kept[id]
		switch {
		case former && !known, k == ContainerKeepAll, k == ContainerKeepAttach && attach, st.Running && attach:
			continue
		}
		// A running one is stopped before it is collected, so what it
		// wrote comes back as it would from a stop.
		if Collects(p) && st.Running {
			if err := p.Stop(ctx, st.Ref, reconcileStopGrace); err != nil {
				errs = append(errs, err)
				continue
			}
		}
		if err := RemoveContainer(ctx, p, st.Ref); err != nil {
			errs = append(errs, err)
			continue
		}
		removed++
	}
	return removed, errors.Join(errs...)
}
