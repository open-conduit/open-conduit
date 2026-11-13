package bootstrap

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/internal/config"
	"github.com/open-conduit/open-conduit/internal/extproc"
	"github.com/open-conduit/open-conduit/internal/management"
	"github.com/open-conduit/open-conduit/internal/runtime"
	"github.com/open-conduit/open-conduit/observability"
	"github.com/open-conduit/open-conduit/protocol"
)

// Composition. Every extension, first-party or installed, runs as a
// package (installed), and bootstrap composes each surface by iterating
// the running packages: nothing here names an extension.

// typeSets are an extension's own entry and connection types: what its
// configure carries and which secrets it may resolve.
func typeSets(x *installed) (entries, connections map[string]bool) {
	entries, connections = map[string]bool{}, map[string]bool{}
	for _, c := range x.m.Provides.Extensions {
		entries[c.Type] = true
	}
	for _, c := range x.m.Provides.Connections {
		connections[c.Type] = true
	}
	return entries, connections
}

// packageRegistry adds the packages' config types, operations, toolsets,
// playbooks and route handler names to a registry. These surfaces need no
// host services, so offline validation uses them too.
func packageRegistry(reg *config.Registry, xs []*installed) {
	if reg.Connections == nil {
		reg.Connections = map[string]func(json.RawMessage) error{}
	}
	if reg.Extensions == nil {
		reg.Extensions = map[string]config.ExtensionType{}
	}
	if reg.Handlers == nil {
		reg.Handlers = map[string]bool{}
	}
	if reg.HandlerConfig == nil {
		reg.HandlerConfig = map[string]func(json.RawMessage) error{}
	}
	if reg.HandlerBinding == nil {
		reg.HandlerBinding = map[string]func(extension.RouteBinding) error{}
	}
	for _, x := range xs {
		for _, c := range x.Types() {
			v := c.Validate
			if v == nil {
				v = func(json.RawMessage, []string) error { return nil }
			}
			switch c.Kind {
			case protocol.ValidateConnection:
				reg.Connections[c.Type] = func(raw json.RawMessage) error { return v(raw, nil) }
			case protocol.ValidateExtension:
				reg.Extensions[c.Type] = config.ExtensionType{Ingress: c.Ingress, Validate: v, Settings: c.Settings, Describe: c.Describe}
			case protocol.ValidateRoute:
				reg.HandlerConfig[c.Type] = func(raw json.RawMessage) error { return v(raw, nil) }
				if c.Binding != nil {
					reg.HandlerBinding[c.Type] = c.Binding
				}
				if c.WorkKinds != nil {
					if reg.HandlerWorkKinds == nil {
						reg.HandlerWorkKinds = map[string]func(json.RawMessage) []string{}
					}
					reg.HandlerWorkKinds[c.Type] = c.WorkKinds
				}
				if describe := c.Describe; describe != nil {
					if reg.HandlerDescribe == nil {
						reg.HandlerDescribe = map[string]func(json.RawMessage) string{}
					}
					reg.HandlerDescribe[c.Type] = func(raw json.RawMessage) string {
						var parts []string
						for _, e := range describe(raw) {
							parts = append(parts, e.Detail)
						}
						return strings.Join(parts, "; ")
					}
				}
			}
		}
		reg.Operations = append(reg.Operations, x.Operations()...)
		if reg.Toolsets == nil {
			reg.Toolsets = map[string]extension.Toolset{}
		}
		for _, ts := range x.Toolsets() {
			// One outside the package's namespace, or granting another's
			// operations, is never registered: config that names it fails
			// closed (and discovery refuses the manifest for it).
			if extension.CheckToolset(x.Name(), ts) == nil {
				reg.Toolsets[ts.Name] = ts
			}
		}
		addPlaybooks(reg, x.Playbooks())
		for name := range x.Routes() {
			reg.Handlers[name] = true
		}
	}
}

// addPlaybooks registers the operations each playbook declares it relies
// on. The first playbook of a name is the one written to the plugin
// (packagePlaybooks), so it is the one kept.
func addPlaybooks(reg *config.Registry, books map[string]string) {
	if reg.Playbooks == nil {
		reg.Playbooks = map[string][]string{}
	}
	for name, text := range books {
		if _, ok := reg.Playbooks[name]; !ok {
			reg.Playbooks[name] = extension.PlaybookOperations(text)
		}
	}
}

// packageHandlers adds the packages' route handlers for one generation.
func packageHandlers(into map[string]extension.RouteHandler, xs []*installed) {
	for _, x := range xs {
		for name, h := range x.Routes() {
			into[name] = h
		}
	}
}

// ownsFamily says whether an Event type is one of the package's own
// families: the types named after it.
func (x *installed) ownsFamily(eventType string) bool {
	return strings.HasPrefix(eventType, x.m.Name+".")
}

// errNoGeneration refuses a timer call before the first generation is
// built; the extension's next pass retries.
var errNoGeneration = errors.New("no active generation")

// packageExecutors adds the packages' executors to executors, and the
// capacity pool of each that names one to pools. An executor naming a pool
// no setting bounds is not registered, so it can never run unbounded; its
// name is returned for the host to report.
func packageExecutors(xs []*installed, executors map[string]extension.Executor, pools map[string]string) (refused []string) {
	for _, x := range xs {
		for name, reg := range x.Executors() {
			if !knownPool(reg.Pool) {
				refused = append(refused, name)
				continue
			}
			executors[name] = reg.Executor
			if reg.Pool != "" {
				pools[name] = reg.Pool
			}
		}
	}
	sort.Strings(refused)
	return refused
}

// knownPool reports whether a capacity pool is one a setting bounds: ""
// (agent turns, settings.concurrency) or jobs (settings.job_concurrency).
// Discovery holds a package's manifest to the same rule.
func knownPool(pool string) bool { return pool == "" || pool == jobPool }

// workKinds are the scheduler's Work kind policies: the packages' own
// kinds.
func workKinds(xs []*installed) map[string]runtime.WorkKindPolicy {
	kinds := map[string]runtime.WorkKindPolicy{}
	for _, x := range xs {
		for _, w := range x.WorkKinds() {
			kinds[w.Kind] = runtime.WorkKindPolicy{Standing: w.Standing, RunOnce: w.RunOnce, Trigger: w.Trigger}
		}
	}
	return kinds
}

// timedKinds are the packages' Work kinds that run on a clock, which a dry
// run never starts.
func (d *daemon) timedKinds() map[string]bool {
	out := map[string]bool{}
	for _, x := range d.packages {
		for _, w := range x.WorkKinds() {
			if w.Timed {
				out[w.Kind] = true
			}
		}
	}
	return out
}

// recurrences are the timer recurrences: the packages' own kinds.
func recurrences(xs []*installed) map[string]runtime.RecurrenceFunc {
	r := map[string]runtime.RecurrenceFunc{}
	for _, x := range xs {
		for kind, f := range x.Recurrences() {
			r[kind] = f
		}
	}
	return r
}

// stopPackages stops every package at once and waits for the slowest, as
// startup launches them at once: each package's process is told to go and
// given time to exit, so stopping them in turn would add those times
// together.
func (d *daemon) stopPackages() {
	var wg sync.WaitGroup
	for _, x := range d.packages {
		wg.Add(1)
		go func() {
			defer wg.Done()
			x.Stop()
		}()
	}
	wg.Wait()
}

// configurePackages sends each package configure for the current
// generation, then registers the ingress sources it declares.
func (d *daemon) configurePackages(ctx context.Context) {
	for _, x := range d.packages {
		x.Configure(ctx)
		d.registerSources(x, x.Sources())
	}
}

// registerSources registers a package's ingress sources: only under its
// own registration names, and only producing its own Event families. A
// source's namespace is the operator's to choose (a schedule entry's
// `namespace`); the ingress refuses one another source holds. A
// registration is immutable for one runtime instance: a source registered
// earlier keeps its registration until restart.
func (d *daemon) registerSources(x *installed, sources []ingressSource) {
	if d.ingress == nil {
		return
	}
	name := x.Name()
	for _, s := range sources {
		if !protocol.SourceNamespace(name, s.Name) || s.Ingress == nil || !ownFamilies(x, s.Ingress.Spec()) {
			d.report("extension_ingress", domain.ID(s.Name), "not_own")
			continue
		}
		d.external.mu.Lock()
		if d.external.sources == nil {
			d.external.sources = map[string]bool{}
		}
		done := d.external.sources[s.Name]
		d.external.sources[s.Name] = true
		d.external.mu.Unlock()
		if done {
			continue
		}
		interval := s.Interval
		if interval < time.Second {
			interval = time.Minute
			if snapshot, _ := d.extensionSnapshot(); snapshot != nil && snapshot.Settings.ReconcileSeconds > 0 {
				interval = time.Duration(snapshot.Settings.ReconcileSeconds) * time.Second
			}
		}
		if err := d.ingress.Register(d.lifetime(), s.Name, s.Source, s.Ingress, interval); err != nil {
			d.external.mu.Lock()
			delete(d.external.sources, s.Name)
			d.external.mu.Unlock()
			d.report("extension_ingress", domain.ID(s.Name), errorCode(err))
			continue
		}
		d.log.Info("extension ingress registered", "extension", name, "name", s.Name, "namespace", s.Source.Namespace)
	}
}

// ownFamilies reports whether an ingress produces only the package's own
// Event families.
func ownFamilies(x *installed, spec extension.IngressSpec) bool {
	if len(spec.Families) == 0 {
		return false
	}
	for _, f := range spec.Families {
		if !x.ownsFamily(f.Type) {
			return false
		}
	}
	return true
}

// packageWebhook returns the handler a package gives a webhook path under
// /<name>/, or nil.
func (d *daemon) packageWebhook(path string) http.Handler {
	for _, x := range d.packages {
		if rest, ok := strings.CutPrefix(path, x.Name()+"/"); ok {
			return x.Webhook(rest)
		}
	}
	return nil
}

// authorize answers whether scope from may read ("read") or file in
// ("file") scope to under the operator's authorization config.
func (d *daemon) authorize(kind string, from, to domain.ID) error {
	switch kind {
	case "read":
		return d.mayRead(from, to)
	case "file":
		return d.mayFile(from, to)
	}
	return fmt.Errorf("unknown authorization %q", kind)
}

// waitTarget resolves a named wait target of kind ("job:<name>") through
// the packages that resolve that kind.
func (d *daemon) waitTarget(kind string) func(context.Context, domain.ID, string) (domain.CorrelationKey, []string, bool) {
	return func(ctx context.Context, scope domain.ID, name string) (domain.CorrelationKey, []string, bool) {
		for _, x := range d.packages {
			if f := x.Targets()[kind]; f != nil {
				if key, types, ok := f(ctx, scope, name); ok {
					return key, types, true
				}
			}
		}
		return domain.CorrelationKey{}, nil, false
	}
}

// registerPackageTools registers each package's toolbelt registration; one
// with neither operations, event families nor a gate adds nothing and is
// skipped, as is an entry-gated one (provides.entry_gated) no enabled scope
// of the generation has an entry for.
func (d *daemon) registerPackageTools(belt *runtime.Toolbelt, s *config.Snapshot) error {
	for _, x := range d.packages {
		if x.entryGated() && !hasEntry(s, x) {
			continue
		}
		reg := x.Registration()
		if len(reg.Operations) == 0 && len(reg.EventTypes) == 0 && reg.Gate == nil && reg.StartGate == nil {
			continue
		}
		if err := belt.Register(reg); err != nil {
			return fmt.Errorf("extension %s toolbelt registration: %w", x.Name(), err)
		}
	}
	return nil
}

// hasEntry reports whether an enabled scope of the generation has an entry
// of one of the package's types.
func hasEntry(s *config.Snapshot, x *installed) bool {
	entries, _ := typeSets(x)
	if s == nil {
		return false
	}
	for _, id := range s.Order {
		scope := s.Scopes[id]
		if scope == nil || !scope.Enabled {
			continue
		}
		for _, e := range scope.Extensions {
			if entries[e.Type] {
				return true
			}
		}
	}
	return false
}

func (d *daemon) packageActions(scope domain.ID, out map[string]extension.ActionProvider) {
	for _, x := range d.packages {
		for name, p := range x.Actions(scope) {
			out[name] = p
		}
	}
}

// annotators are the packages that declare annotations, by package name;
// management asks them concurrently and remembers which time out.
func (d *daemon) annotators() []management.Annotator {
	var out []management.Annotator
	for _, x := range d.packages {
		if x.m.Provides.Annotate {
			out = append(out, management.Annotator{Name: x.Name(), Annotate: x.Annotate})
		}
	}
	return out
}

func (d *daemon) packageRender(e domain.Event) (string, bool) {
	for _, x := range d.packages {
		ctx, cancel := context.WithTimeout(d.lifetime(), 5*time.Second)
		text, ok := x.Render(ctx, e)
		cancel()
		if ok {
			return text, true
		}
	}
	return "", false
}

// runPackageTasks starts each task when its pace has elapsed.
func (d *daemon) runPackageTasks(ctx context.Context) {
	for _, x := range d.packages {
		for _, task := range x.Tasks() {
			d.runTask(ctx, x.Name()+"/"+task.Name, task.Pace, task.Run)
		}
	}
}

// packageChecks are the packages' doctor checks, ordinary or deep: each is
// listed under its own name, and runs on the package's view of the current
// configuration (checkConfig). The extensions check keeps only the
// packages' own health.
func (d *daemon) packageChecks(deep bool) []func(context.Context) runtime.Check {
	var out []func(context.Context) runtime.Check
	for _, x := range d.packages {
		for _, check := range x.Checks() {
			if check.Deep != deep {
				continue
			}
			run := check.Run
			out = append(out, func(ctx context.Context) runtime.Check {
				s, _ := d.configs.Current()
				cfg := checkConfig(s, x, d.executors, filepath.Dir(d.o.ConfigPath))
				d.hooks.mu.Lock()
				cfg.Tunnel = tunnelView(x, d.hooks.options)
				d.hooks.mu.Unlock()
				return run(ctx, cfg)
			})
		}
	}
	return out
}

// offlineChecks are the extensions' Offline checks on a config validated
// without a daemon; dir is the config file's directory. A package's run
// only when an enabled scope uses it, so offline doctor starts no package
// the configuration doesn't use (as offline validation starts none).
func offlineChecks(ctx context.Context, s *config.Snapshot, executors map[string]extension.Executor, dir string, pkgs []*installed) []runtime.Check {
	var out []runtime.Check
	var tunnel TunnelOptions
	if s != nil {
		_, _, tunnel, _ = exposure(Options{}, s.Settings.Webhooks)
	}
	for _, x := range pkgs {
		if !usedBy(s, x.m) && tunnelView(x, tunnel) == nil {
			continue
		}
		for _, check := range x.Checks() {
			if check.Offline && !check.Deep {
				cfg := checkConfig(s, x, executors, dir)
				cfg.Tunnel = tunnelView(x, tunnel)
				out = append(out, check.Run(ctx, cfg))
			}
		}
	}
	return out
}

// tunnelView is the webhook tunnel in x's check view
// (protocol.CheckConfig.Tunnel): t, when x provides its type; nil
// otherwise.
func tunnelView(x *installed, t TunnelOptions) *protocol.CheckTunnel {
	if t.Kind == "" {
		return nil
	}
	for _, d := range x.m.Provides.Tunnels {
		if d.Type == t.Kind {
			return &protocol.CheckTunnel{Type: t.Kind, Config: tunnelConfig(t).Data}
		}
	}
	return nil
}

// checkConfig is an extension's read-only view of a configuration for its
// checks (protocol.CheckConfig): each enabled scope that uses it (an entry,
// a route, a binding of its executor, resource, sandbox or container, or
// one granted its operations), with the scope's routes and bindings, its
// own entries, the sandboxes and containers of its own providers, and the
// connections of its own types they name. Secret reference text never enters it.
func checkConfig(s *config.Snapshot, x *installed, executors map[string]extension.Executor, dir string) protocol.CheckConfig {
	cfg := protocol.CheckConfig{Dir: dir}
	if s == nil {
		return cfg
	}
	entries, connections := typeSets(x)
	handlers, operations := map[string]bool{}, map[string]bool{}
	for name := range x.Routes() {
		handlers[name] = true
	}
	for _, op := range x.Operations() {
		operations[op] = true
	}
	own := ownExecutors(x)
	ownSandboxes, ownContainers := map[string]bool{}, map[string]bool{}
	for _, s := range x.m.Provides.Sandboxes {
		ownSandboxes[s.Type] = true
	}
	for _, c := range x.m.Provides.Containers {
		ownContainers[c.Type] = true
	}
	uses := func(scope *config.EffectiveScope) bool {
		if usesExtension(scope, x.m) {
			return true
		}
		for _, e := range scope.Extensions {
			if entries[e.Type] {
				return true
			}
		}
		for _, r := range scope.Routes {
			if handlers[r.Handler] {
				return true
			}
		}
		for _, b := range scope.Bindings {
			if own[b.Executor] || slices.ContainsFunc(b.Operations, func(op string) bool { return operations[op] }) {
				return true
			}
		}
		return false
	}
	connType := map[string]string{}
	for _, c := range s.Connections {
		connType[c.ID] = c.Type
	}
	named := map[string]bool{}
	for _, id := range s.Order {
		scope := s.Scopes[id]
		if scope == nil || !scope.Enabled {
			continue
		}
		ownConn := false
		for _, b := range scope.Bindings {
			ownConn = ownConn || connections[connType[b.Connection]]
		}
		if !ownConn && !uses(scope) {
			continue
		}
		v := protocol.CheckScope{ID: string(id)}
		for _, e := range scope.Extensions {
			if entries[e.Type] {
				v.Entries = append(v.Entries, protocol.CheckEntry{Name: e.Name, Type: e.Type, Connection: e.Connection, Config: e.Config, SecretNames: e.SecretNames})
				named[e.Connection] = true
			}
		}
		for _, r := range scope.Routes {
			v.Routes = append(v.Routes, protocol.CheckRoute{Name: r.Name, Handler: r.Handler, Binding: r.Binding})
		}
		for _, b := range scope.Bindings {
			cb := protocol.CheckBinding{Name: b.Name, Executor: b.Executor, Operations: b.Operations, Connection: b.Connection}
			if e, ok := executors[b.Executor]; ok {
				cb.Toolbelt = e.Capabilities().Toolbelt
			}
			if own[b.Executor] {
				cb.Config = b.Config
			}
			if b.Resource != nil {
				cb.Resource = &protocol.CheckResource{Provider: b.Resource.Provider, Config: b.Resource.Config}
			}
			if b.Sandbox != nil && ownSandboxes[b.Sandbox.Provider] {
				cb.Sandbox = &protocol.CheckSandbox{Provider: b.Sandbox.Provider, Config: b.Sandbox.Config}
			}
			if b.Container != nil && ownContainers[b.Container.Provider] {
				cb.Container = &protocol.CheckContainer{Provider: b.Container.Provider, Config: b.Container.Config}
			}
			switch {
			case b.Sandbox != nil:
				cb.Isolation = protocol.IsolationSandbox
			case b.Container != nil:
				cb.Isolation = protocol.IsolationContainer
			}
			v.Bindings = append(v.Bindings, cb)
			named[b.Connection] = true
		}
		cfg.Scopes = append(cfg.Scopes, v)
	}
	for _, c := range s.Connections {
		if connections[c.Type] && named[c.ID] {
			cfg.Connections = append(cfg.Connections, protocol.CheckConnection{ID: c.ID, Type: c.Type, Config: c.Config})
		}
	}
	return cfg
}

// ownExecutors are the executors a package declares, whether or not it is
// running.
func ownExecutors(x *installed) map[string]bool {
	out := map[string]bool{}
	for _, e := range x.m.Provides.Executors {
		out[e.Name] = true
	}
	return out
}

// packagePlaybooks adds the packages' playbooks. A package's playbook never
// replaces one already there, and its `{{bin}}` is how a turn runs Open
// Conduit. Packages that share a playbook (the trackers'
// work-tracker-issue) give the same text, which is no conflict.
func (d *daemon) packagePlaybooks(into map[string]string) {
	for _, x := range d.packages {
		for name, text := range x.Playbooks() {
			text = extproc.RenderPlaybook(text, "open-conduit")
			if have, taken := into[name]; taken {
				if have == text {
					continue
				}
				d.log.Warn("extension playbook not written: the name is taken", "extension", x.Name(), "playbook", name)
				continue
			}
			into[name] = text
		}
	}
}

func (d *daemon) packageExporters() []observability.Exporter {
	var out []observability.Exporter
	for _, x := range d.packages {
		out = append(out, x.Exporter())
	}
	return out
}

// telemetryFanout writes the host's telemetry events to the events log,
// then hands them to every extension that takes them; a forwarding failure
// never fails the log.
type telemetryFanout struct {
	primary interface {
		Capture(ctx context.Context, scope, name string, at time.Time, props map[string]any) error
	}
	// sinks are the packages, read per event since they join the list
	// after the log opens; each forwards the events only with the
	// telemetry.sink permission.
	sinks func() []*installed
}

func (f telemetryFanout) Capture(ctx context.Context, scope, name string, at time.Time, props map[string]any) error {
	err := f.primary.Capture(ctx, scope, name, at, props)
	if f.sinks != nil {
		for _, s := range f.sinks() {
			_ = s.Capture(ctx, scope, name, at, props)
		}
	}
	return err
}

func (d *daemon) sinks() []*installed { return d.packages }

// usedBy says whether an enabled scope of the configuration uses the
// package (usesExtension).
func usedBy(s *config.Snapshot, m protocol.Manifest) bool {
	if s == nil {
		return false
	}
	for _, scope := range s.Scopes {
		if scope != nil && scope.Enabled && usesExtension(scope, m) {
			return true
		}
	}
	return false
}
