package bootstrap

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/internal/config"
	"github.com/open-conduit/open-conduit/internal/extproc"
	"github.com/open-conduit/open-conduit/internal/runtime"
	"github.com/open-conduit/open-conduit/observability"
	"github.com/open-conduit/open-conduit/protocol"
)

// Installed extension packages (07-03). At start the daemon discovers
// <config dir>/extensions/<name>/open-conduit-extension.json, refuses any
// set that conflicts with the first-party registrations (atomically, as the
// in-process Registrar does), starts each package behind its scoped host
// service and registers the adapters next to the first-party ones: config
// types and validators, route handlers, executors, resource providers, work
// kinds, recurrences, toolbelt operations, ingress sources, webhook paths,
// tasks, checks, annotations, prompt rendering and telemetry export. Nothing
// here names an extension: every decision reads the manifest. Packages are
// read only at start, so install, update and removal take effect at the
// next serve; configuration changes reach running packages through
// configure on every apply and tunnel URL change.

type externalState struct {
	root string
	// current is the generation packages see. It is published before
	// configure is sent, so nothing here reads the config service while
	// its apply holds the configuration lock.
	current atomic.Pointer[generationSnapshot]

	mu       sync.Mutex
	remotes  []*extproc.Remote
	byName   map[string]*extproc.Remote
	failed   []extproc.Diagnostic
	sources  map[string]bool            // registered ingress names
	webhooks map[string]*extproc.Remote // webhook path (no leading slash) → package
	tasks    map[string]time.Time       // last start by package/task
	running  map[string]bool            // task in flight
	public   string                     // public URL last sent in configure
}

type generationSnapshot struct {
	s   *config.Snapshot
	gen uint64
}

// publishGeneration records the generation packages are configured from.
func (d *daemon) publishGeneration(s *config.Snapshot, gen uint64) {
	d.external.current.Store(&generationSnapshot{s: s, gen: gen})
}

func (d *daemon) extensionSnapshot() (*config.Snapshot, uint64) {
	if g := d.external.current.Load(); g != nil {
		return g.s, g.gen
	}
	return nil, 0
}

func extensionsRoot(configPath string) string {
	return filepath.Join(filepath.Dir(configPath), extproc.Dir)
}

// takenNames lists the names the first-party registrations own, so a
// package cannot claim one.
func takenNames(reg config.Registry) extproc.Taken {
	t := extproc.Taken{}
	t.Add("name", "core", "execution", "manual", "os", "open-conduit")
	for name := range reg.Connections {
		t.Add("type", name)
		t.Add("name", name)
	}
	for name := range reg.Extensions {
		t.Add("type", name)
		t.Add("name", name)
	}
	for name := range reg.Handlers {
		t.Add("handler", name)
		t.Add("name", name)
	}
	for name := range reg.Executors {
		t.Add("executor", name)
		t.Add("name", name)
	}
	for name := range reg.Resources {
		t.Add("resource", name)
		t.Add("name", name)
	}
	t.Add("operation", reg.Operations...)
	t.Add("operation", extension.CreateWork, extension.DeclareOutcome, extension.Emit, extension.ReadContext, extension.Wait)
	for kind := range workKinds() {
		t.Add("work_kind", kind)
	}
	for kind := range recurrences() {
		t.Add("recurrence", kind)
	}
	return t
}

// manifestExecutor stands in for a package's executor in offline config
// validation: it carries the declared capabilities and runs nothing.
type manifestExecutor struct{ decl protocol.ExecutorDecl }

func (e manifestExecutor) Capabilities() domain.Capabilities { return e.decl.Capabilities.Domain() }
func (e manifestExecutor) Run(context.Context, extension.RunRequest) (extension.RunHandle, error) {
	return nil, extension.ErrUnsupported
}
func (e manifestExecutor) Recover(context.Context, extension.RunRequest, domain.Payload) (extension.RunHandle, error) {
	return nil, extension.ErrUnsupported
}
func (e manifestExecutor) Resume(context.Context, extension.RunRequest, domain.Payload) (extension.RunHandle, error) {
	return nil, extension.ErrUnsupported
}
func (e manifestExecutor) Interrupt(context.Context, domain.ID) error {
	return extension.ErrUnsupported
}
func (e manifestExecutor) Inject(context.Context, domain.ID, domain.Event) error {
	return extension.ErrUnsupported
}
func (e manifestExecutor) Stop(context.Context, domain.ID, uint64) error {
	return extension.ErrUnsupported
}
func (e manifestExecutor) Forget(context.Context, domain.ID) error { return extension.ErrUnsupported }
func (e manifestExecutor) Attach(context.Context, domain.ID) (extension.AttachSpec, error) {
	return extension.AttachSpec{}, extension.ErrUnsupported
}

// validatorFunc supplies the validator of one declared type of a package.
type validatorFunc func(m protocol.Manifest, kind, typ string) func(json.RawMessage, []string) error

// externalRegistry adds the packages' declared config types, handlers,
// resource types and operations to a registry. Executors are added by the
// caller (live adapters, or manifestExecutor offline).
func externalRegistry(reg *config.Registry, manifests []protocol.Manifest, validator validatorFunc) {
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
	if reg.Resources == nil {
		reg.Resources = map[string]bool{}
	}
	if reg.ResourceConfig == nil {
		reg.ResourceConfig = map[string]func(json.RawMessage) error{}
	}
	for _, m := range manifests {
		p := m.Provides
		for _, c := range p.Connections {
			v := validator(m, protocol.ValidateConnection, c.Type)
			reg.Connections[c.Type] = func(raw json.RawMessage) error { return v(raw, nil) }
		}
		for _, x := range p.Extensions {
			reg.Extensions[x.Type] = config.ExtensionType{Ingress: x.Ingress, Validate: validator(m, protocol.ValidateExtension, x.Type)}
		}
		for _, r := range p.Routes {
			reg.Handlers[r.Handler] = true
			if r.Config {
				v := validator(m, protocol.ValidateRoute, r.Handler)
				reg.HandlerConfig[r.Handler] = func(raw json.RawMessage) error { return v(raw, nil) }
			}
		}
		for _, r := range p.Resources {
			reg.Resources[r.Type] = true
			v := validator(m, protocol.ValidateResource, r.Type)
			reg.ResourceConfig[r.Type] = func(raw json.RawMessage) error { return v(raw, nil) }
		}
		for _, o := range p.Operations {
			reg.Operations = append(reg.Operations, o.Name)
		}
	}
}

// offlineExternal adds the packages installed beside configPath to an
// offline registry: declared types are known, but their opaque config is
// checked by the package only when a daemon validates or applies it (an
// offline check never starts a process).
func offlineExternal(reg *config.Registry, configPath, exclude string) {
	if configPath == "" {
		return
	}
	all, _ := extproc.Discover(extensionsRoot(configPath))
	var found []extproc.Found
	for _, f := range all {
		if f.Manifest.Name != exclude {
			found = append(found, f)
		}
	}
	if len(found) == 0 || extproc.CheckConflicts(found, takenNames(*reg)) != nil {
		return
	}
	var manifests []protocol.Manifest
	for _, f := range found {
		manifests = append(manifests, f.Manifest)
		for _, x := range f.Manifest.Provides.Executors {
			reg.Executors[x.Name] = manifestExecutor{x}
		}
	}
	externalRegistry(reg, manifests, func(protocol.Manifest, string, string) func(json.RawMessage, []string) error {
		return func(json.RawMessage, []string) error { return nil }
	})
}

// openExtensions discovers, admits and starts the installed packages, and
// registers their executors and resource providers. It runs before the
// configuration opens, so the packages' validators can check it; the first
// configure carries an empty generation, and configureExtensions sends the
// real one once the configuration and ingress exist.
func (d *daemon) openExtensions(ctx context.Context) {
	x := &d.external
	x.root = extensionsRoot(d.o.ConfigPath)
	x.byName, x.sources, x.webhooks, x.tasks, x.running = map[string]*extproc.Remote{}, map[string]bool{}, map[string]*extproc.Remote{}, map[string]time.Time{}, map[string]bool{}
	found, errs := extproc.Discover(x.root)
	for _, err := range errs {
		name, _, _ := strings.Cut(err.Error(), ":")
		x.failed = append(x.failed, extproc.Diagnose(name, err, extproc.Paths{}))
	}
	if len(found) == 0 {
		return
	}
	if err := extproc.CheckConflicts(found, takenNames(registry(d.executors))); err != nil {
		for _, f := range found {
			x.failed = append(x.failed, extproc.Diagnose(f.Manifest.Name, err, extproc.Paths{}))
		}
		d.log.Error("installed extensions refused: registration conflict", "error", err)
		return
	}
	for _, f := range found {
		name := f.Manifest.Name
		paths := extproc.StatePaths(d.o.StateDir, name)
		r, err := extproc.Load(ctx, f, d.extensionDeps(f.Manifest), d.extensionOptions(f, paths))
		if err != nil {
			diag := extproc.Diagnose(name, err, paths)
			x.failed = append(x.failed, diag)
			d.log.Error("extension not started", "extension", name, "code", diag.Code, "hint", diag.Hint)
			continue
		}
		x.mu.Lock()
		x.remotes = append(x.remotes, r)
		x.byName[name] = r
		x.mu.Unlock()
		for _, e := range f.Manifest.Provides.Executors {
			if adapter, ok := r.Executor(e.Name); ok {
				d.executors[e.Name] = adapter
			}
		}
		for _, res := range f.Manifest.Provides.Resources {
			if p, ok := r.Provider(res.Type); ok {
				d.resources.providers[res.Type] = p
			}
		}
		d.log.Info("extension started", "extension", name, "version", f.Manifest.Version, "protocol", protocol.Version)
	}
}

// closeExtensions shuts every package down (shutdown, stdin closed, then
// signals to its process group). Executor jobs it detached keep running and
// are adopted by the next daemon through executor.recover.
func (d *daemon) closeExtensions() {
	for _, r := range d.extensionRemotes() {
		_ = r.Process().Close()
	}
}

func (d *daemon) extensionRemotes() []*extproc.Remote {
	d.external.mu.Lock()
	defer d.external.mu.Unlock()
	return append([]*extproc.Remote(nil), d.external.remotes...)
}

// extensionRegistry adds the running packages' declared types, validated
// through their validate method, to the serve registry.
func (d *daemon) extensionRegistry(reg *config.Registry) {
	var manifests []protocol.Manifest
	for _, r := range d.extensionRemotes() {
		manifests = append(manifests, r.Manifest())
	}
	externalRegistry(reg, manifests, func(m protocol.Manifest, kind, typ string) func(json.RawMessage, []string) error {
		d.external.mu.Lock()
		r := d.external.byName[m.Name]
		d.external.mu.Unlock()
		return r.Validator(kind, typ)
	})
}

// usesExtension reports whether a scope's configuration uses one of the
// package's declared types, handlers, executors or resource providers.
func usesExtension(scope *config.EffectiveScope, m protocol.Manifest) bool {
	p := m.Provides
	types := map[string]bool{}
	for _, x := range p.Extensions {
		types[x.Type] = true
	}
	for _, x := range scope.Extensions {
		if types[x.Type] {
			return true
		}
	}
	handlers := map[string]bool{}
	for _, r := range p.Routes {
		handlers[r.Handler] = true
	}
	for _, r := range scope.Routes {
		if handlers[r.Handler] {
			return true
		}
	}
	executors := map[string]bool{}
	for _, e := range p.Executors {
		executors[e.Name] = true
	}
	resources := map[string]bool{}
	for _, r := range p.Resources {
		resources[r.Type] = true
	}
	for _, b := range scope.Bindings {
		if executors[b.Executor] || b.Resource != nil && resources[b.Resource.Provider] {
			return true
		}
	}
	return false
}

func (d *daemon) extensionDeps(m protocol.Manifest) extproc.Deps {
	connTypes := map[string]bool{}
	for _, c := range m.Provides.Connections {
		connTypes[c.Type] = true
	}
	extTypes := map[string]bool{}
	for _, x := range m.Provides.Extensions {
		extTypes[x.Type] = true
	}
	snapshot := func() *config.Snapshot {
		s, _ := d.extensionSnapshot()
		return s
	}
	return extproc.Deps{
		Store: d.coord, Now: d.now,
		CurrentLimits: func() runtime.CausalLimits {
			if s := snapshot(); s != nil {
				return runtime.CausalLimits{MaxDepth: s.Settings.CausalMaxDepth, PerMinute: s.Settings.CausalPerMinute}
			}
			return runtime.CausalLimits{}
		},
		Scopes: func() []domain.ID {
			s := snapshot()
			if s == nil {
				return nil
			}
			var out []domain.ID
			for _, id := range s.Order {
				if scope := s.Scopes[id]; scope != nil && scope.Enabled && usesExtension(scope, m) {
					out = append(out, id)
				}
			}
			return out
		},
		// Secrets admits only names bound to the package's own entries in
		// the scope: their selected secret names, or the secret names of a
		// connection of a declared type one of them names (B05).
		Secrets: func(ctx context.Context, id domain.ID, connection string, names []string) (map[string]string, error) {
			s := snapshot()
			if s == nil || s.Scopes[id] == nil {
				return nil, extension.ErrUnauthorized
			}
			scope := s.Scopes[id]
			allowed := map[string]bool{}
			usesConn := false
			for _, x := range scope.Extensions {
				if !extTypes[x.Type] {
					continue
				}
				for _, n := range x.SecretNames {
					allowed[n] = true
				}
				usesConn = usesConn || connection != "" && x.Connection == connection
			}
			if connection == "" {
				for _, n := range names {
					if !allowed[n] {
						return nil, extension.ErrUnauthorized
					}
				}
				return d.resolver.ResolveNames(ctx, scope, names)
			}
			if !usesConn {
				return nil, extension.ErrUnauthorized
			}
			for _, c := range s.Connections {
				if c.ID != connection || !connTypes[c.Type] {
					continue
				}
				out := map[string]string{}
				for _, n := range names {
					ref, ok := c.SecretRefs[n]
					if !ok || ref == "" {
						return nil, extension.ErrUnauthorized
					}
					v, err := d.resolver.Resolve(ctx, ref)
					if err != nil {
						return nil, err
					}
					out[n] = v
				}
				return out, nil
			}
			return nil, extension.ErrUnauthorized
		},
		Deliver: func(ctx context.Context, name string, delivery extension.Delivery) ([]domain.AcceptedEvent, error) {
			accepted, err := d.ingress.Push(ctx, name, delivery)
			if err == nil {
				d.poke()
			}
			return accepted, err
		},
		Artifacts: d.artifacts,
		Timers:    currentTimers{d},
		CancelWork: func(ctx context.Context, work domain.ID, expected domain.Revision, reason, key string) (domain.ID, error) {
			return d.current.Load().control.CancelWork(ctx, work, expected, reason, key)
		},
	}
}

// currentTimers arms through the current generation's timers.
type currentTimers struct{ d *daemon }

func (t currentTimers) ArmRecurrence(ctx context.Context, scope domain.ID, timer domain.Timer, expected domain.Revision, kind string, spec []byte) error {
	return t.d.current.Load().timers.ArmRecurrence(ctx, scope, timer, expected, kind, spec)
}
func (t currentTimers) Cancel(ctx context.Context, id domain.ID, expected domain.Revision) error {
	return t.d.current.Load().timers.Cancel(ctx, id, expected)
}

func (d *daemon) extensionOptions(f extproc.Found, paths extproc.Paths) extproc.Options {
	name := f.Manifest.Name
	return extproc.Options{
		StateDir: paths.State, LogDir: filepath.Dir(paths.Log),
		Host:      protocol.HostInfo{Name: "open-conduit", Version: d.o.Version},
		Configure: func() protocol.ConfigureParams { return d.extensionConfigure(f.Manifest) },
		OnConfigured: func(res protocol.ConfigureResult) {
			d.extensionConfigured(name, res)
		},
		KV:  extproc.StoreKV{Store: d.coord, Now: d.now},
		Log: d.log,
	}
}

// extensionConfigure builds a package's configure params from the current
// generation: every enabled scope's entries of its declared types, with the
// connection each names when that is of one of its declared types.
func (d *daemon) extensionConfigure(m protocol.Manifest) protocol.ConfigureParams {
	params := protocol.ConfigureParams{DryRun: d.o.DryRun}
	d.hooks.mu.Lock()
	params.PublicURL = d.hooks.public
	d.hooks.mu.Unlock()
	s, gen := d.extensionSnapshot()
	if s == nil {
		return params
	}
	params.Generation = strconv.FormatUint(gen, 10)
	types := map[string]bool{}
	for _, x := range m.Provides.Extensions {
		types[x.Type] = true
	}
	connTypes := map[string]bool{}
	for _, c := range m.Provides.Connections {
		connTypes[c.Type] = true
	}
	for _, id := range s.Order {
		scope := s.Scopes[id]
		if scope == nil || !scope.Enabled {
			continue
		}
		for _, x := range scope.Extensions {
			if !types[x.Type] {
				continue
			}
			entry := protocol.ConfigEntry{ScopeID: string(id), Type: x.Type, Name: x.Name, Config: x.Config, Secrets: x.SecretNames}
			for _, c := range s.Connections {
				if c.ID == x.Connection && connTypes[c.Type] {
					names := make([]string, 0, len(c.SecretRefs))
					for n := range c.SecretRefs {
						names = append(names, n)
					}
					sort.Strings(names)
					entry.Connection = &protocol.ConnectionEntry{ID: c.ID, Type: c.Type, Config: c.Config, Secrets: names}
				}
			}
			params.Entries = append(params.Entries, entry)
		}
	}
	return params
}

// extensionConfigured registers the ingress sources and webhook paths a
// configure result declares. Registrations are immutable for one runtime
// instance, as for first-party ingress: a source registered earlier keeps
// its registration until restart.
func (d *daemon) extensionConfigured(name string, res protocol.ConfigureResult) {
	x := &d.external
	x.mu.Lock()
	r := x.byName[name]
	for _, path := range res.WebhookPaths {
		if r != nil {
			x.webhooks[strings.TrimPrefix(path, "/")] = r
		}
	}
	x.mu.Unlock()
	if r == nil || d.ingress == nil {
		return
	}
	for _, decl := range res.Sources {
		x.mu.Lock()
		done := x.sources[decl.Name]
		x.sources[decl.Name] = true
		x.mu.Unlock()
		if done {
			continue
		}
		interval := time.Duration(decl.IntervalSeconds) * time.Second
		if interval < time.Second {
			interval = time.Minute
			if s, _ := d.extensionSnapshot(); s != nil && s.Settings.ReconcileSeconds > 0 {
				interval = time.Duration(s.Settings.ReconcileSeconds) * time.Second
			}
		}
		if err := d.ingress.Register(d.lifetime(), decl.Name, decl.Source.Extension(), r.Ingress(decl), interval); err != nil {
			x.mu.Lock()
			delete(x.sources, decl.Name)
			x.mu.Unlock()
			d.report("extension_ingress", domain.ID(decl.Name), errorCode(err))
			continue
		}
		d.log.Info("extension ingress registered", "extension", name, "name", decl.Name, "namespace", decl.Source.Namespace)
	}
}

func (d *daemon) lifetime() context.Context {
	if d.ctx != nil {
		return d.ctx
	}
	return context.Background()
}

// configureExtensions sends configure to every running package (config
// apply, tunnel URL change); a failure is reported and retried at the next
// apply or URL change.
func (d *daemon) configureExtensions(ctx context.Context) {
	for _, r := range d.extensionRemotes() {
		cx, cancel := context.WithTimeout(ctx, 30*time.Second)
		err := r.Process().Reconfigure(cx)
		cancel()
		if err != nil {
			d.report("extension_configure", domain.ID(r.Process().Name()), errorCode(err))
			continue
		}
		d.clear("extension_configure", domain.ID(r.Process().Name()))
	}
	d.hooks.mu.Lock()
	d.external.mu.Lock()
	d.external.public = d.hooks.public
	d.external.mu.Unlock()
	d.hooks.mu.Unlock()
}

// reconfigureOnURLChange re-sends configure when the tunnel's public URL
// changed since the last configure.
func (d *daemon) reconfigureOnURLChange(ctx context.Context) {
	d.hooks.mu.Lock()
	public := d.hooks.public
	d.hooks.mu.Unlock()
	d.external.mu.Lock()
	changed := public != d.external.public && len(d.external.remotes) > 0
	d.external.mu.Unlock()
	if changed {
		d.configureExtensions(ctx)
	}
}

// extensionHandlers, extensionWorkKinds and extensionRecurrences are the
// packages' runtime callbacks for one generation's router, scheduler and
// timers.
func (d *daemon) extensionHandlers(into map[string]extension.RouteHandler) {
	for _, r := range d.extensionRemotes() {
		for _, h := range r.Manifest().Provides.Routes {
			into[h.Handler] = r.RouteHandler(h.Handler)
		}
	}
}

func (d *daemon) extensionWorkKinds(into map[string]runtime.WorkKindPolicy) {
	for _, r := range d.extensionRemotes() {
		for _, k := range r.Manifest().Provides.WorkKinds {
			if p, ok := r.WorkKind(k.Kind); ok {
				into[k.Kind] = p
			}
		}
	}
}

func (d *daemon) extensionRecurrences(into map[string]runtime.RecurrenceFunc) {
	for _, r := range d.extensionRemotes() {
		for _, kind := range r.Manifest().Provides.Recurrences {
			into[kind] = r.Recurrence(kind)
		}
	}
}

// registerExtensionTools adds each package's toolbelt registration.
func (d *daemon) registerExtensionTools(belt *runtime.Toolbelt) error {
	for _, r := range d.extensionRemotes() {
		m := r.Manifest()
		if len(m.Provides.Operations) == 0 && len(m.Provides.Families) == 0 {
			continue
		}
		if err := belt.Register(r.Registration()); err != nil {
			return fmt.Errorf("extension %s toolbelt registration: %w", m.Name, err)
		}
	}
	return nil
}

// extensionWebhook returns the handler of a webhook path a package
// registered at configure.
func (d *daemon) extensionWebhook(name string) http.Handler {
	d.external.mu.Lock()
	r := d.external.webhooks[name]
	d.external.mu.Unlock()
	if r == nil {
		return nil
	}
	return r.Webhook("/"+name, 0)
}

// runExtensionTasks starts each declared task when its interval has
// elapsed, on its own goroutine; one task never overlaps itself.
func (d *daemon) runExtensionTasks(ctx context.Context) {
	for _, r := range d.extensionRemotes() {
		for _, t := range r.Manifest().Provides.Tasks {
			key := r.Process().Name() + "/" + t.Name
			interval := time.Duration(t.IntervalSeconds) * time.Second
			d.external.mu.Lock()
			due := !d.external.running[key] && d.now().Sub(d.external.tasks[key]) >= interval
			if due {
				d.external.running[key], d.external.tasks[key] = true, d.now()
			}
			d.external.mu.Unlock()
			if !due {
				continue
			}
			d.wg.Add(1)
			go func(r *extproc.Remote, task, key string, bound time.Duration) {
				defer d.wg.Done()
				cx, cancel := context.WithTimeout(ctx, min(max(bound, time.Minute), 10*time.Minute))
				err := r.Task(cx, task)
				cancel()
				d.external.mu.Lock()
				d.external.running[key] = false
				d.external.mu.Unlock()
				if err != nil && ctx.Err() == nil {
					d.report("extension_task", domain.ID(key), errorCode(err))
				} else {
					d.clear("extension_task", domain.ID(key))
				}
			}(r, t.Name, key, interval)
		}
	}
}

// extensionCheck reports each installed package for doctor: refused or
// failed packages, process state and restarts, and its declared checks.
func (d *daemon) extensionCheck(ctx context.Context) runtime.Check {
	return d.runExtensionChecks(ctx, false)
}

// extensionDeepCheck runs the packages' deep checks (check_secrets).
func (d *daemon) extensionDeepCheck(ctx context.Context) runtime.Check {
	return d.runExtensionChecks(ctx, true)
}

func (d *daemon) runExtensionChecks(ctx context.Context, deep bool) runtime.Check {
	name := "extensions"
	if deep {
		name = "extensions_deep"
	}
	check := runtime.Check{Name: name, Status: runtime.CheckOK, Code: "ok"}
	raise := func(status, code string) {
		if rank(status) > rank(check.Status) {
			check.Status, check.Code = status, code
		}
	}
	d.external.mu.Lock()
	failed := append([]extproc.Diagnostic(nil), d.external.failed...)
	d.external.mu.Unlock()
	remotes := d.extensionRemotes()
	if len(remotes) == 0 && len(failed) == 0 {
		check.Status, check.Code, check.Detail = runtime.CheckInfo, "not_installed", "no extension packages are installed"
		return check
	}
	if !deep {
		for _, f := range failed {
			raise(runtime.CheckFail, f.Code)
			check.Items = append(check.Items, f.String())
		}
	}
	for _, r := range remotes {
		p, m := r.Process(), r.Manifest()
		if !deep {
			state := "running"
			if err := p.Err(); err != nil {
				diag := extproc.Diagnose(m.Name, err, extproc.StatePaths(d.o.StateDir, m.Name))
				raise(runtime.CheckFail, diag.Code)
				state = "stopped: " + diag.Summary + " (" + diag.Code + ")"
			}
			item := fmt.Sprintf("%s %s: %s, protocol %d, %d restart(s)", m.Name, m.Version, state, protocol.Version, p.Restarts())
			if n := r.SchemaSkips(); n > 0 {
				item += fmt.Sprintf(", %d unreadable payload(s) skipped", n)
			}
			if p.Restarts() > 0 {
				raise(runtime.CheckWarn, "extension_restarted")
			}
			check.Items = append(check.Items, item)
		}
		for _, c := range m.Provides.Checks {
			if c.Deep != deep {
				continue
			}
			cx, cancel := context.WithTimeout(ctx, 20*time.Second)
			res, err := r.Check(cx, c.Name, deep)
			cancel()
			switch {
			case err != nil:
				raise(runtime.CheckWarn, "extension_check_failed")
				check.Items = append(check.Items, m.Name+" check "+c.Name+": unavailable ("+errorCode(err)+")")
			case !res.OK:
				raise(runtime.CheckWarn, "extension_check_failed")
				check.Items = append(check.Items, m.Name+" check "+c.Name+": "+res.Detail)
			default:
				check.Items = append(check.Items, m.Name+" check "+c.Name+": ok "+res.Detail)
			}
		}
	}
	if deep && len(check.Items) == 0 {
		check.Status, check.Code = runtime.CheckInfo, "no_deep_checks"
	}
	sort.Strings(check.Items)
	return check
}

// extensionAnnotations are the packages' contributions to GET /work/{id}.
func (d *daemon) extensionAnnotations(ctx context.Context, w domain.Work) [][3]string {
	var out [][3]string
	for _, r := range d.extensionRemotes() {
		for _, a := range r.Annotate(ctx, w) {
			out = append(out, [3]string{r.Process().Name(), a.Name, a.Value})
		}
	}
	return out
}

// extensionActions are the action providers of the packages a scope's
// current configuration uses, by package name.
func (d *daemon) extensionActions(scope domain.ID, out map[string]extension.ActionProvider) {
	s, _ := d.extensionSnapshot()
	if s == nil || s.Scopes[scope] == nil || !s.Scopes[scope].Enabled {
		return
	}
	for _, r := range d.extensionRemotes() {
		if p, ok := r.Actions(); ok && usesExtension(s.Scopes[scope], r.Manifest()) {
			out[r.Process().Name()] = p
		}
	}
}

// extensionRender renders an Event through the package that owns its
// family, for agent prompts; false falls back to generic rendering.
func (d *daemon) extensionRender(e domain.Event) (string, bool) {
	for _, r := range d.extensionRemotes() {
		if !strings.HasPrefix(e.Type, r.Process().Name()+".") {
			continue
		}
		ctx, cancel := context.WithTimeout(d.lifetime(), 5*time.Second)
		text, ok := r.Render(ctx, e)
		cancel()
		return text, ok
	}
	return "", false
}

// extensionExporter fans telemetry out to the packages that export it. It
// joins the pipeline at start, before packages load, and reaches whichever
// are running.
type extensionExporter struct{ d *daemon }

func (e extensionExporter) Export(ctx context.Context, records []observability.Record) error {
	var errs []error
	for _, r := range e.d.extensionRemotes() {
		if x, ok := r.Exporter(); ok {
			errs = append(errs, x.Export(ctx, records))
		}
	}
	return errors.Join(errs...)
}
func (e extensionExporter) Shutdown(context.Context) error { return nil }
