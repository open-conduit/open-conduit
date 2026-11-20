package bootstrap

import (
	"context"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/internal/config"
	"github.com/open-conduit/open-conduit/internal/extproc"
	"github.com/open-conduit/open-conduit/internal/management"
	"github.com/open-conduit/open-conduit/internal/runtime"
	"github.com/open-conduit/open-conduit/protocol"
	"github.com/open-conduit/open-conduit/storage"
)

// Packages on demand (#734). The daemon discovers every package, bundled
// and installed, into its catalogue, but runs a package's process only
// while something needs it:
//
//   - the configuration uses it (packageUsed): an entry of one of its
//     types, a route to one of its handlers, a binding of one of its
//     executors, resource, sandbox or container providers, a binding
//     granted one of its operations, or a binding naming a connection of
//     one of its types. Disabled scopes count, since a package keeps what
//     it holds for their entries;
//   - the host needs it: the webhook tunnel it runs, the first-run setup
//     while there is no config file, the verifier of a trusted issuer, or
//     a surface asked for on demand (a verifier, started on first use);
//   - it is a resource or container provider whose state directory holds
//     something: the workspaces and containers it made are still to be
//     reconciled and swept, whether or not a binding names it now;
//   - an execution may still need it (liveHolds): one live, parked or
//     finished with its resource exclusion unconfirmed, that ran on one of
//     its executors or whose binding used it when the configuration
//     stopped using it. It stops once none is left.
//
// Every apply reconciles the running set with the new generation before
// it is built, so the generation composes the packages that run with it;
// a package that drains later is stopped and the generation rebuilt
// without it. A package that isn't running still validates its config
// objects: its catalogue entry asks a validation process, started for
// validate alone and stopped once idle.

// validationIdle is how long a validation process may go unused before the
// host loop stops it.
const validationIdle = time.Minute

// providerSet is a copy-on-write set of providers by type: published
// whole, so a reader holding one never sees it change.
type providerSet[P any] struct{ p atomic.Pointer[map[string]P] }

func (s *providerSet[P]) all() map[string]P {
	if m := s.p.Load(); m != nil {
		return *m
	}
	return nil
}

func (s *providerSet[P]) set(m map[string]P) { s.p.Store(&m) }

// retiring is a running package the configuration no longer uses, kept for
// the live executions that may still need it: those the from generation
// gave it (ids, worked out on the next drain when nil).
type retiring struct {
	from *config.Snapshot
	ids  map[domain.ID]bool
}

// catalogued are the discovered, admitted packages in discovery order:
// each a stand-in with no process of its own, through which the config
// registry validates the package's types (its running process when it has
// one, else a validation process).
func (d *daemon) catalogued() []*installed {
	d.external.mu.Lock()
	defer d.external.mu.Unlock()
	return append([]*installed(nil), d.external.catalogue...)
}

// runningRemote is the running process of a package, or nil.
func (d *daemon) runningRemote(name string) *extproc.Remote {
	d.external.mu.Lock()
	defer d.external.mu.Unlock()
	if x := d.external.byName[name]; x != nil {
		return x.r
	}
	return nil
}

// executorMap is the running packages' executors by name. It is never
// changed once published, so a scheduler built with it keeps a fixed set.
func (d *daemon) executorMap() map[string]extension.Executor {
	d.external.mu.Lock()
	defer d.external.mu.Unlock()
	return d.external.executors
}

// poolMap is the capacity pool of each running executor that names one.
func (d *daemon) poolMap() map[string]string {
	d.external.mu.Lock()
	defer d.external.mu.Unlock()
	return d.external.pools
}

// publishRunning recomputes, from the running packages, the executors,
// capacity pools and resource, sandbox, container and tunnel providers the
// host composes, each as a new map. It is called with change held.
func (d *daemon) publishRunning() {
	pkgs := d.extensionPackages()
	executors, pools := map[string]extension.Executor{}, map[string]string{}
	for _, refused := range packageExecutors(pkgs, executors, pools) {
		d.report("executor_pool", domain.ID(refused), "unknown_pool")
	}
	resources := map[string]extension.ResourceProvider{}
	sandboxes := map[string]extension.SandboxProvider{}
	containers := map[string]extension.ContainerProvider{}
	tunnels := map[string]extension.TunnelProvider{}
	for _, x := range pkgs {
		p := x.m.Provides
		for _, res := range p.Resources {
			if v, ok := x.r.Provider(res.Type); ok {
				resources[res.Type] = v
			}
		}
		for _, s := range p.Sandboxes {
			if v, ok := x.r.Sandbox(s.Type); ok {
				sandboxes[s.Type] = v
			}
		}
		for _, c := range p.Containers {
			if v, ok := x.r.Container(c.Type); ok {
				containers[c.Type] = v
			}
		}
		for _, t := range p.Tunnels {
			if v, ok := x.r.Tunnel(t.Type); ok {
				tunnels[t.Type] = v
			}
		}
	}
	d.external.mu.Lock()
	d.external.executors, d.external.pools = executors, pools
	d.external.mu.Unlock()
	d.resources.providers.set(resources)
	d.sandboxes.providers.set(sandboxes)
	d.containers.providers.set(containers)
	d.hooks.setTunnels(tunnels)
}

// startPackages starts the named catalogued packages that aren't running,
// all at once, so a start takes the slowest handshake, not their sum. Each
// joins the running packages in discovery order; one that fails to start
// is reported in doctor. It is called with change held.
func (d *daemon) startPackages(ctx context.Context, names []string) {
	x := &d.external
	var found []extproc.Found
	x.mu.Lock()
	for _, name := range names {
		if f, ok := x.found[name]; ok && x.byName[name] == nil {
			found = append(found, f)
		}
	}
	x.mu.Unlock()
	if len(found) == 0 {
		return
	}
	loaded := make([]*extproc.Remote, len(found))
	failures := make([]error, len(found))
	var wg sync.WaitGroup
	for i, f := range found {
		deps, opts := d.extensionDeps(f.Manifest), d.extensionOptions(f, extproc.StatePaths(d.o.StateDir, f.Manifest.Name))
		wg.Add(1)
		go func() {
			defer wg.Done()
			loaded[i], failures[i] = extproc.Load(ctx, f, deps, opts)
		}()
	}
	wg.Wait()
	for i, f := range found {
		name := f.Manifest.Name
		x.mu.Lock()
		x.failed = slices.DeleteFunc(x.failed, func(diag extproc.Diagnostic) bool { return diag.Extension == name })
		x.mu.Unlock()
		if err := failures[i]; err != nil {
			diag := extproc.Diagnose(name, err, extproc.StatePaths(d.o.StateDir, name))
			x.mu.Lock()
			x.failed = append(x.failed, diag)
			x.mu.Unlock()
			d.log.Error("extension not started", "extension", name, "code", diag.Code, "hint", diag.Hint, "docs", diag.Docs)
			continue
		}
		r := loaded[i]
		pkg := &installed{m: f.Manifest, r: r, d: d, dir: f.Dir}
		x.mu.Lock()
		x.packages = append(x.packages, pkg)
		sort.SliceStable(x.packages, func(a, b int) bool { return x.order[x.packages[a].Name()] < x.order[x.packages[b].Name()] })
		x.byName[name] = pkg
		delete(x.retiring, name)
		x.mu.Unlock()
		// Its validation process has nothing left to do.
		if c := d.catalogueEntry(name); c != nil && c.offline != nil {
			c.offline.close()
		}
		d.log.Info("extension started", "extension", name, "version", f.Manifest.Version, "protocol", protocol.Version, "pass_env", strings.Join(r.Process().PassedEnv(), ","))
	}
	d.publishRunning()
}

// stopPackages stops the named running packages, all at once: they leave
// the running packages first, so nothing composes them while they go, and
// the ingress sources they declared are unregistered (what the store holds
// for them stays, for the next start). Executor jobs a package detached
// keep running. It is called with change held.
func (d *daemon) stopPackages(names []string) {
	d.finishStopping(d.detachPackages(names))
}

// detachPackages takes the named packages out of the running set, so
// nothing composes them from now on (a generation built next leaves them
// out), and unregisters their ingress sources. Their processes keep
// running until finishStopping, so a generation still active that composes
// them isn't left calling dead processes until it is rebuilt. It is called
// with change held.
func (d *daemon) detachPackages(names []string) []*installed {
	x := &d.external
	var stopping []*installed
	x.mu.Lock()
	for _, name := range names {
		pkg := x.byName[name]
		if pkg == nil {
			continue
		}
		stopping = append(stopping, pkg)
		delete(x.byName, name)
		delete(x.retiring, name)
		x.packages = slices.DeleteFunc(x.packages, func(p *installed) bool { return p == pkg })
	}
	x.mu.Unlock()
	if len(stopping) == 0 {
		return nil
	}
	d.publishRunning()
	for _, pkg := range stopping {
		for _, s := range pkg.Sources() {
			if d.ingress != nil {
				d.ingress.Unregister(s.Name)
			}
			x.mu.Lock()
			delete(x.sources, s.Name)
			x.mu.Unlock()
		}
	}
	return stopping
}

// finishStopping stops detached packages' processes.
func (d *daemon) finishStopping(stopping []*installed) {
	stopAll(stopping)
	for _, pkg := range stopping {
		d.log.Info("extension stopped: nothing uses it", "extension", pkg.Name())
	}
}

// catalogueEntry is the catalogued stand-in of a package, or nil.
func (d *daemon) catalogueEntry(name string) *installed {
	for _, c := range d.catalogued() {
		if c.Name() == name {
			return c
		}
	}
	return nil
}

// packagesWanted are the catalogued packages that should run under s: those
// it uses, those the host needs, and the providers with state to keep.
func (d *daemon) packagesWanted(ctx context.Context, s *config.Snapshot) map[string]bool {
	want := map[string]bool{}
	var tunnels []string
	if !d.o.NoTunnel {
		var stored config.Webhooks
		if s != nil {
			stored = s.Settings.Webhooks
		}
		if _, _, t, _ := exposure(d.o, stored); t.Kind != "" {
			tunnels = append(tunnels, t.Kind)
		}
	}
	d.hooks.mu.Lock()
	if k := d.hooks.options.Kind; k != "" {
		tunnels = append(tunnels, k)
	}
	d.hooks.mu.Unlock()
	_, noFile := os.Stat(d.o.ConfigPath)
	firstRun := errors.Is(noFile, os.ErrNotExist)
	issuers, _ := management.IssuerTypes(ctx, d.coord, d.now)
	var armed map[string]bool
	_ = d.coord.Transact(ctx, storage.Guard{Now: d.now()}, func(tx storage.Tx) error {
		var err error
		armed, err = runtime.ArmedRecurrencesInTx(tx)
		return err
	})
	d.external.mu.Lock()
	onDemand := maps.Clone(d.external.onDemand)
	d.external.mu.Unlock()
	for _, c := range d.catalogued() {
		m := c.m
		name := m.Name
		switch {
		case startAll(), onDemand[name], packageUsed(s, m), d.holdsState(m):
		case slices.ContainsFunc(m.Provides.Tunnels, func(t protocol.TunnelDecl) bool { return slices.Contains(tunnels, t.Type) }):
		case firstRun && m.Provides.Setup != nil && m.Provides.Setup.FirstRun:
		case slices.ContainsFunc(m.Provides.Recurrences, func(kind string) bool { return armed[kind] }):
			// A timer of one of its recurrence kinds is armed: the package
			// computes when it fires next, and cancels it once its entry
			// is gone (it gets that configure before it stops).
		case slices.ContainsFunc(m.Provides.Verifiers, func(v protocol.VerifierDecl) bool { return slices.Contains(issuers, v.Type) }):
		default:
			continue
		}
		want[name] = true
	}
	return want
}

// holdsState reports whether a resource or container provider's state
// directory holds anything: workspaces or containers it made, which it
// must go on reconciling and sweeping until they are gone.
func (d *daemon) holdsState(m protocol.Manifest) bool {
	var dirs []string
	for _, r := range m.Provides.Resources {
		dirs = append(dirs, resourceDir(d.o.StateDir, r.Type))
	}
	for _, c := range m.Provides.Containers {
		dirs = append(dirs, containerDir(d.o.StateDir, c.Type))
	}
	for _, dir := range dirs {
		if entries, err := os.ReadDir(dir); err == nil && len(entries) > 0 {
			return true
		}
	}
	return false
}

// packageUsed reports whether any scope of s, enabled or not, uses the
// package m.
func packageUsed(s *config.Snapshot, m protocol.Manifest) bool {
	if s == nil {
		return false
	}
	connTypes := connectionTypes(s)
	// A secret reference of a scheme the package serves needs it to
	// resolve (manifest secret_schemes).
	serves := func(refs map[string]string) bool {
		for _, ref := range refs {
			scheme, _, ok := strings.Cut(ref, ":")
			if ok && slices.ContainsFunc(m.Provides.SecretSchemes, func(d protocol.SecretSchemeDecl) bool { return d.Scheme == scheme }) {
				return true
			}
		}
		return false
	}
	for _, c := range s.Connections {
		if serves(c.SecretRefs) {
			return true
		}
	}
	for _, scope := range s.Scopes {
		if scope == nil {
			continue
		}
		if serves(scope.SecretRefs) || usesExtension(scope, m) {
			return true
		}
		// An entry that runs its processes in a sandbox uses the sandbox's
		// provider.
		for _, e := range scope.Extensions {
			if e.Sandbox != nil && slices.ContainsFunc(m.Provides.Sandboxes, func(sb protocol.SandboxDecl) bool { return sb.Type == e.Sandbox.Provider }) {
				return true
			}
		}
		for _, b := range scope.Bindings {
			if bindingUses(b, m, connTypes) {
				return true
			}
		}
	}
	return false
}

// connectionTypes are the type of each of a generation's connections, by ID.
func connectionTypes(s *config.Snapshot) map[string]string {
	out := map[string]string{}
	if s != nil {
		for _, c := range s.Connections {
			out[c.ID] = c.Type
		}
	}
	return out
}

// bindingUses reports whether a binding uses the package m: its executor,
// resource, sandbox or container provider, an operation it or one of its
// grants is given, or a connection of one of its types.
func bindingUses(b config.Binding, m protocol.Manifest, connTypes map[string]string) bool {
	p := m.Provides
	if slices.ContainsFunc(p.Executors, func(e protocol.ExecutorDecl) bool { return e.Name == b.Executor }) ||
		b.Resource != nil && slices.ContainsFunc(p.Resources, func(r protocol.ResourceDecl) bool { return r.Type == b.Resource.Provider }) ||
		b.Sandbox != nil && slices.ContainsFunc(p.Sandboxes, func(s protocol.SandboxDecl) bool { return s.Type == b.Sandbox.Provider }) ||
		b.Container != nil && slices.ContainsFunc(p.Containers, func(c protocol.ContainerDecl) bool { return c.Type == b.Container.Provider }) {
		return true
	}
	if b.Connection != "" && slices.ContainsFunc(p.Connections, func(c protocol.TypeDecl) bool { return c.Type == connTypes[b.Connection] }) {
		return true
	}
	own := func(op string) bool {
		return slices.ContainsFunc(p.Operations, func(o protocol.OperationDecl) bool { return o.Name == op })
	}
	if slices.ContainsFunc(b.Operations, own) {
		return true
	}
	for _, g := range b.Grants {
		if slices.ContainsFunc(g.Operations, own) {
			return true
		}
	}
	return false
}

// liveHolds are, by package, the executions that may still need it under
// s (runtime.HeldExecutionsInTx: live, parked, or finished with their
// resource exclusion unconfirmed): the package of each one's executor, and
// those its binding in s uses.
func (d *daemon) liveHolds(ctx context.Context, s *config.Snapshot) (map[string]map[domain.ID]bool, error) {
	cat := d.catalogued()
	connTypes := connectionTypes(s)
	out := map[string]map[domain.ID]bool{}
	hold := func(name string, id domain.ID) {
		if out[name] == nil {
			out[name] = map[domain.ID]bool{}
		}
		out[name][id] = true
	}
	err := d.coord.Transact(ctx, storage.Guard{Now: d.now()}, func(tx storage.Tx) error {
		clear(out)
		live, err := runtime.HeldExecutionsInTx(tx)
		if err != nil {
			return err
		}
		for _, e := range live {
			var binding *config.Binding
			if w, err := tx.Work(e.WorkID); err == nil && s != nil {
				if scope := s.Scopes[w.ScopeID]; scope != nil {
					for i := range scope.Bindings {
						if scope.Bindings[i].Name == e.ExecutorBinding {
							binding = &scope.Bindings[i]
						}
					}
				}
			}
			executor := e.Executor
			if executor == "" && binding != nil {
				executor = binding.Executor
			}
			for _, c := range cat {
				if slices.ContainsFunc(c.m.Provides.Executors, func(x protocol.ExecutorDecl) bool { return x.Name == executor }) ||
					binding != nil && bindingUses(*binding, c.m, connTypes) {
					hold(c.Name(), e.ID)
				}
			}
		}
		return nil
	})
	return out, err
}

// startingPackages are the packages to start before the configuration
// opens: those the generation it will open uses, worked out from the
// manifests alone (no package is asked), and those live executions hold.
// The configuration's own validation then decides: startWanted and stopUnused
// starts any this missed and stops any it started needlessly.
func (d *daemon) startingPackages(ctx context.Context) []string {
	s := d.manifestSnapshot(ctx)
	want := d.packagesWanted(ctx, s)
	if holds, err := d.liveHolds(ctx, s); err == nil {
		for name := range holds {
			want[name] = true
		}
	}
	var out []string
	for _, c := range d.catalogued() {
		if want[c.Name()] {
			out = append(out, c.Name())
		}
	}
	return out
}

// manifestSnapshot is the generation the configuration will open (the
// committed one, else the config file), validated against the catalogued
// packages' manifests alone, so no package starts for it; nil when there
// is none or it doesn't validate that way.
func (d *daemon) manifestSnapshot(ctx context.Context) *config.Snapshot {
	reg := registry(nil)
	reg.Executors = map[string]extension.Executor{}
	var xs []*installed
	for _, c := range d.catalogued() {
		x := &installed{m: c.m, dir: c.dir}
		xs = append(xs, x)
		x.providerTypes(&reg)
		for _, e := range c.m.Provides.Executors {
			reg.Executors[e.Name] = manifestExecutor{decl: e}
		}
	}
	packageRegistry(&reg, xs)
	generation, normalized, err := management.StoredGeneration(ctx, d.coord, d.now)
	var s *config.Snapshot
	switch {
	case err != nil:
		return nil
	case generation != 0:
		s, err = config.Validate(normalized, reg)
	default:
		data, rerr := config.ReadFile(d.o.ConfigPath)
		if rerr != nil {
			return nil
		}
		s, err = config.ValidateFile(data, reg, config.Includes{Base: filepath.Dir(d.o.ConfigPath)})
	}
	if err != nil {
		return nil
	}
	return s
}

// startWanted starts the packages next, the generation about to be built,
// wants, so that generation composes them. It stops nothing: retireUnused
// does, once the generation is active. It is called with change held.
func (d *daemon) startWanted(ctx context.Context, next *config.Snapshot) {
	want := d.packagesWanted(ctx, next)
	var start []string
	for _, c := range d.catalogued() {
		if want[c.Name()] {
			start = append(start, c.Name())
		}
	}
	d.startPackages(ctx, start)
}

// retireUnused stops the running packages the active generation, next,
// doesn't want and no execution may need under prev (the generation that
// gave them their work), and keeps the rest until those executions finish
// (drainPackages). It runs after next is active and every package has had
// its configure, so a package whose entries went has already let go of
// what it held for them (a schedule's timers); those timers, still armed
// until then, are why it is asked again here rather than before. It
// detaches the idle ones (detachPackages) and returns them: the caller
// rebuilds the generation without them, then stops them (stopUnused). It
// is called with change held.
func (d *daemon) retireUnused(ctx context.Context, prev, next *config.Snapshot) []*installed {
	want := d.packagesWanted(ctx, next)
	var unused []string
	for _, x := range d.extensionPackages() {
		if !want[x.Name()] {
			unused = append(unused, x.Name())
		}
	}
	if len(unused) == 0 {
		return nil
	}
	holds, err := d.liveHolds(ctx, prev)
	var idle []string
	d.external.mu.Lock()
	for _, name := range unused {
		switch {
		case err != nil:
			// Which executions need it is worked out at the next drain.
			if _, ok := d.external.retiring[name]; !ok {
				d.external.retiring[name] = &retiring{from: prev}
			}
		case len(holds[name]) > 0:
			if r, ok := d.external.retiring[name]; ok && r.ids != nil {
				for id := range holds[name] {
					r.ids[id] = true
				}
			} else {
				d.external.retiring[name] = &retiring{from: prev, ids: holds[name]}
			}
		default:
			if _, ok := d.external.retiring[name]; !ok {
				idle = append(idle, name)
			}
		}
	}
	d.external.mu.Unlock()
	return d.detachPackages(idle)
}

// stopUnused retires the packages the active generation next doesn't use
// (retireUnused), rebuilds it without those it detached and then stops
// them, so the generation never composes a stopped package. It is called
// with change held, once next is active and configured.
func (d *daemon) stopUnused(ctx context.Context, prev, next *config.Snapshot) {
	detached := d.retireUnused(ctx, prev, next)
	if len(detached) == 0 {
		return
	}
	var names []string
	for _, x := range detached {
		names = append(names, x.Name())
	}
	d.rebuildWithout(strings.Join(names, ","))
	d.finishStopping(detached)
}

// rebuildWithout rebuilds the active generation once packages it composed
// have stopped, so nothing routes to them. A failed rebuild keeps the
// generation as it was and says so.
func (d *daemon) rebuildWithout(why string) {
	s, gen := d.extensionSnapshot()
	if s == nil || gen == 0 {
		return
	}
	next, err := d.build(s, gen)
	if err != nil {
		d.log.Error("configuration generation not rebuilt without the stopped extensions; restart required", "generation", gen, "extensions", why)
		return
	}
	d.current.Store(next)
	d.log.Info("configuration generation rebuilt without the stopped extensions", "generation", gen, "extensions", why)
	d.poke()
}

// drainPackages stops the retiring packages whose executions have all
// finished, then rebuilds the active generation without them, at most once
// per resource interval and off the loop.
func (d *daemon) drainPackages(ctx context.Context) {
	x := &d.external
	x.mu.Lock()
	due := len(x.retiring) > 0 && !x.draining && d.now().Sub(x.drained) >= d.o.ResourceInterval
	if due {
		x.draining, x.drained = true, d.now()
	}
	x.mu.Unlock()
	if !due {
		return
	}
	d.goSafe("extension_drain", func() {
		defer func() {
			x.mu.Lock()
			x.draining = false
			x.mu.Unlock()
		}()
		x.change.Lock()
		defer x.change.Unlock()
		live := map[domain.ID]bool{}
		err := d.coord.Transact(ctx, storage.Guard{Now: d.now()}, func(tx storage.Tx) error {
			clear(live)
			execs, err := runtime.HeldExecutionsInTx(tx)
			for _, e := range execs {
				live[e.ID] = true
			}
			return err
		})
		if err != nil {
			return
		}
		x.mu.Lock()
		pending := map[string]*config.Snapshot{}
		for name, r := range x.retiring {
			if r.ids == nil {
				pending[name] = r.from
			}
		}
		x.mu.Unlock()
		// Which executions a package retired without knowing them may
		// still need is worked out now, outside the lock doctor reads.
		found := map[string]map[domain.ID]bool{}
		for name, from := range pending {
			if holds, err := d.liveHolds(ctx, from); err == nil {
				found[name] = holds[name]
				if found[name] == nil {
					found[name] = map[domain.ID]bool{}
				}
			}
		}
		var done []string
		x.mu.Lock()
		for name, r := range x.retiring {
			if ids, ok := found[name]; ok && r.ids == nil {
				r.ids = ids
			}
			if r.ids == nil {
				continue
			}
			for id := range r.ids {
				if !live[id] {
					delete(r.ids, id)
				}
			}
			if len(r.ids) == 0 {
				done = append(done, name)
			}
		}
		x.mu.Unlock()
		if len(done) == 0 {
			return
		}
		sort.Strings(done)
		detached := d.detachPackages(done)
		d.rebuildWithout(strings.Join(done, ","))
		d.finishStopping(detached)
	})
}

// reapValidation stops the validation processes nothing has used for
// validationIdle, off the loop.
func (d *daemon) reapValidation() {
	now := time.Now()
	for _, c := range d.catalogued() {
		if v := c.offline; v != nil && v.idle(now, validationIdle) {
			d.goSafe("extension_validation", v.close)
		}
	}
}

// onDemandRetry is how long after a failed start a package started on
// demand isn't tried again.
const onDemandRetry = time.Minute

// startOnDemand starts a catalogued package for a surface an operator asks
// of it (the verifier of an issuer an admin is storing), and keeps it
// running for the daemon's life. A package that failed to start isn't
// tried again within onDemandRetry. It never runs on an unauthenticated
// path: credential checks only find running verifiers.
func (d *daemon) startOnDemand(ctx context.Context, name string) {
	x := &d.external
	x.change.Lock()
	defer x.change.Unlock()
	x.mu.Lock()
	if x.onDemand == nil {
		x.onDemand, x.onDemandFailed = map[string]bool{}, map[string]time.Time{}
	}
	if at, ok := x.onDemandFailed[name]; ok && d.now().Sub(at) < onDemandRetry {
		x.mu.Unlock()
		return
	}
	x.onDemand[name] = true
	x.mu.Unlock()
	d.startPackages(ctx, []string{name})
	if d.runningRemote(name) == nil {
		x.mu.Lock()
		x.onDemandFailed[name] = d.now()
		x.mu.Unlock()
	}
}

// installedExtensions lists every catalogued package for GET /extensions:
// its name, title and version, and whether it runs.
func (d *daemon) installedExtensions() []management.Extension {
	var out []management.Extension
	for _, c := range d.catalogued() {
		out = append(out, management.Extension{Name: c.Name(), Title: c.Title(), Version: c.m.Version, Running: d.runningRemote(c.Name()) != nil})
	}
	return out
}
