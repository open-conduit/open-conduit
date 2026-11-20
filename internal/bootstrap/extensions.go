package bootstrap

import (
	"context"
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
	"sync/atomic"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/internal/config"
	"github.com/open-conduit/open-conduit/internal/extproc"
	"github.com/open-conduit/open-conduit/internal/management"
	"github.com/open-conduit/open-conduit/internal/runtime"
	"github.com/open-conduit/open-conduit/observability"
	"github.com/open-conduit/open-conduit/protocol"
	"github.com/open-conduit/open-conduit/storage"
)

// Extension packages (07-03), bundled and installed. At start the daemon
// discovers <config dir>/extensions/<name>/open-conduit-extension.json and
// the bundled packages, refuses any set that conflicts with the host's own
// names (atomically), starts each package behind its scoped host service
// and adds it to the packages composition iterates (compose.go), so config
// types and validators, route handlers, toolbelt operations, configure,
// tasks, checks, actions, annotations, prompt rendering and telemetry
// export all come from it. Its resource, sandbox, container and tunnel
// providers are registered as it starts (publishRunning). Nothing here
// names an extension: every decision reads the manifest. Only the packages
// something needs run (demand.go): the rest stay in the catalogue, which
// validates their config through a validation process. Packages are
// discovered only at start, so install, update and removal take effect at
// the next serve; configuration changes reach running packages through
// configure on every apply and tunnel URL change, and start or stop them.

type externalState struct {
	root string
	// current is the generation packages see. It is published before
	// configure is sent, so nothing here reads the config service while
	// its apply holds the configuration lock.
	current atomic.Pointer[generationSnapshot]

	// change serialises starting and stopping packages and the
	// generation builds that follow (lock order: the configuration's lock,
	// then change, then mu).
	change sync.Mutex

	mu sync.Mutex
	// catalogue are the discovered, admitted packages, running or not, in
	// discovery order (catalogued); found and order index them by name.
	catalogue []*installed
	found     map[string]extproc.Found
	order     map[string]int
	// packages are the running ones, in discovery order.
	packages []*installed
	byName   map[string]*installed
	// executors and pools are the running packages' executors and their
	// capacity pools (publishRunning).
	executors map[string]extension.Executor
	pools     map[string]string
	// retiring are the running packages the configuration no longer uses,
	// kept for live executions (drainPackages); onDemand those started for
	// a surface the host asked of them.
	retiring map[string]*retiring
	onDemand map[string]bool
	// onDemandFailed is when each package started on demand last failed
	// to start (startOnDemand).
	onDemandFailed map[string]time.Time
	draining       bool
	drained        time.Time
	failed         []extproc.Diagnostic
	sources        map[string]bool      // registered ingress names
	tasks          map[string]time.Time // last start by package/task
	running        map[string]bool      // task in flight
	public         string               // public URL last sent in configure
	// startGrants are the package grants (settings.extensions) the
	// packages start with before a generation is published: the
	// committed generation's, or the config file's on a first start.
	// Written before any package starts, read-only after.
	startGrants map[string][]string
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

// bundledRoot is the directory of the packages that ship with this binary:
// `extensions` beside the real executable, links resolved, as the `os`
// toolbelt is found. A release archive carries the setup package there, so
// a fresh install has it before anything is installed. Tests replace it.
var bundledRoot = func() string {
	self, err := os.Executable()
	if err != nil {
		return ""
	}
	if real, err := filepath.EvalSymlinks(self); err == nil {
		self = real
	}
	return filepath.Join(filepath.Dir(self), extproc.Dir)
}

// UseBundledRoot points discovery of bundled packages at root instead of
// beside this executable, and returns what restores it: for a test that
// checks configuration offline as the release's binary would, with the
// release's packages beside it.
func UseBundledRoot(root string) (restore func()) {
	prev := bundledRoot
	bundledRoot = func() string { return root }
	return func() { bundledRoot = prev }
}

// discoverPackages finds the packages installed beside configPath, then
// the bundled ones no installed package of the same name replaces. An
// installed copy replaces the bundled one even when it is refused, so a
// newer package an older host cannot run is reported, never swapped
// silently for the bundled release. exclude leaves out the installed copy
// of one name (removal's check), which brings the bundled one back.
func discoverPackages(configPath, exclude string) ([]extproc.Found, []error) {
	var found []extproc.Found
	var errs []error
	shadow := map[string]bool{}
	if configPath != "" {
		root := extensionsRoot(configPath)
		all, e := extproc.Discover(root)
		errs = append(errs, e...)
		for _, f := range all {
			if f.Manifest.Name != exclude {
				found = append(found, f)
			}
		}
		if entries, err := os.ReadDir(root); err == nil {
			for _, e := range entries {
				if e.IsDir() && !strings.HasPrefix(e.Name(), ".") && e.Name() != exclude {
					shadow[e.Name()] = true
				}
			}
		}
	}
	if root := bundledRoot(); root != "" {
		all, e := extproc.Discover(root)
		errs = append(errs, e...)
		for _, f := range all {
			if !shadow[f.Manifest.Name] {
				f.Bundled = true
				found = append(found, f)
			}
		}
	}
	return found, errs
}

// takenNames lists the names a registry already holds (the host's own, and
// at offline validation those of the packages it has), so a package
// cannot claim one.
func takenNames(reg config.Registry) extproc.Taken {
	t := extproc.Taken{}
	t.Add("name", "core", "execution", "manual", "os", "open-conduit", extension.CallerOperator)
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
	t.Add("operation", extension.CancelWork, extension.CreateWork, extension.DeclareOutcome, extension.Emit, extension.ReadContext, extension.Wait)
	return t
}

// manifestExecutor stands in for a package's executor in offline config
// validation: it carries the declared capabilities, answers the resource
// mode it requests through the package (x) and runs nothing.
type manifestExecutor struct {
	decl protocol.ExecutorDecl
	x    *installed
}

// ResourceMode asks the package which resource mode a binding config makes
// its executor request, when the manifest declares resource_mode
// (config.ResourceModer); "" (the resource config's default) otherwise.
func (e manifestExecutor) ResourceMode(raw json.RawMessage) string {
	if !e.decl.ResourceMode || e.x == nil {
		return ""
	}
	r, err := e.x.remote()
	if err != nil {
		return ""
	}
	return r.ExecutorResourceMode(e.decl.Name)(raw)
}

// Playbook asks the package which playbook a binding config starts its
// executor's turns on, when the manifest declares playbook
// (extension.PlaybookNamer); "" otherwise.
func (e manifestExecutor) Playbook(raw json.RawMessage) string {
	if !e.decl.Playbook || e.x == nil {
		return ""
	}
	r, err := e.x.remote()
	if err != nil {
		return ""
	}
	return r.ExecutorPlaybook(e.decl.Name)(raw)
}

// VersionProbe asks the package for the command that shows its executor
// is installed where a binding config runs it, when the manifest declares
// version_probe (extension.VersionProber); none otherwise.
func (e manifestExecutor) VersionProbe(raw json.RawMessage) ([]string, []string) {
	if !e.decl.VersionProbe || e.x == nil {
		return nil, nil
	}
	r, err := e.x.remote()
	if err != nil {
		return nil, nil
	}
	return r.ExecutorVersionProbe(e.decl.Name)(raw)
}

// FolderConfig asks the package to adapt a binding config from an included
// folder, when the manifest declares folder_config
// (extension.FolderConfigurer); the config unchanged otherwise. A package
// that cannot start refuses it, so the binding is never stored unadapted.
func (e manifestExecutor) FolderConfig(raw json.RawMessage, folder string, resource bool) (json.RawMessage, error) {
	// With no package to ask (manifestSnapshot), the config stands as
	// written.
	if !e.decl.FolderConfig || e.x == nil {
		return raw, nil
	}
	r, err := e.x.remote()
	if err != nil {
		return nil, err
	}
	return r.ExecutorFolderConfig(e.decl.Name)(raw, folder, resource)
}

// ValidateConfig asks the package to check a binding's executor config
// (config.ConfigValidator), so offline validation refuses what the daemon
// refuses, with the package's own diagnostics. A package that cannot
// start refuses it.
func (e manifestExecutor) ValidateConfig(raw json.RawMessage, secretNames []string) error {
	if e.x == nil {
		return nil
	}
	r, err := e.x.remote()
	if err != nil {
		return err
	}
	return r.Validator(protocol.ValidateExecutor, e.decl.Name)(raw, secretNames)
}

// ConfigWarnings asks the package for its warnings about a binding config
// (config.ConfigWarner): validate with kind executor, whose answer an older
// package leaves without any.
func (e manifestExecutor) ConfigWarnings(raw json.RawMessage) []string {
	if e.x == nil {
		return nil
	}
	r, err := e.x.remote()
	if err != nil {
		return nil
	}
	return r.ExecutorWarnings(e.decl.Name)(raw)
}

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

// offlineExternal adds the packages installed beside configPath (but
// exclude), and those bundled with the binary, to an offline registry: their declared types and executors, with
// each package's config validated by the package itself. A package starts,
// only once a config object of its types needs checking, for validate alone:
// no coordination store, a throwaway state directory, and stopped by the
// returned validation's close. A package that cannot start refuses its
// objects, and the validation says why (offlinePackages.diagnostics).
func offlineExternal(reg *config.Registry, configPath, exclude string, data []byte) *offlinePackages {
	o := &offlinePackages{}
	// A package started to check a config starts with the grant that
	// config gives it, as the daemon would start it.
	grants := config.FileGrants(data)
	// With no config file, the bundled packages still count: a first-party
	// extension served by open-conduit itself (#428) validates its own
	// config objects however the config is given.
	found, _ := discoverPackages(configPath, exclude)
	if len(found) == 0 || extproc.CheckConflicts(found, takenNames(*reg)) != nil {
		return o
	}
	for _, f := range found {
		v := &offlinePackage{f: f, passEnv: grants[f.Manifest.Name]}
		o.packages = append(o.packages, v)
		x := &installed{m: f.Manifest, offline: v, dir: f.Dir}
		o.installed = append(o.installed, x)
		x.providerTypes(reg)
		for _, e := range f.Manifest.Provides.Executors {
			reg.Executors[e.Name] = manifestExecutor{decl: e, x: x}
		}
	}
	packageRegistry(reg, o.installed)
	return o
}

// defaultConfig is DefaultConfig for the running daemon: a running
// package's first-run setup is asked through its own process.
func (d *daemon) defaultConfig() []byte {
	var setups []extension.Setup
	d.external.mu.Lock()
	for _, p := range d.external.packages {
		if s, ok := p.r.Setup(); ok {
			setups = append(setups, s)
		}
	}
	d.external.mu.Unlock()
	return firstRunConfig(setups)
}

// offlineStartTimeout bounds starting a package to validate offline.
const offlineStartTimeout = 15 * time.Second

// offlinePackages are the packages one offline validation may start.
type offlinePackages struct {
	packages []*offlinePackage
	// installed adapt them as extensions, for their offline checks.
	installed []*installed
}

// close stops every package the validation started and removes its state.
func (o *offlinePackages) close() {
	for _, p := range o.packages {
		p.close()
	}
}

// diagnostics are the packages that could not start to validate their
// config, so their objects were refused unchecked.
func (o *offlinePackages) diagnostics() []config.Diagnostic {
	var out []config.Diagnostic
	for _, p := range o.packages {
		if p.err != nil {
			d := extproc.Diagnose(p.f.Manifest.Name, p.err, extproc.Paths{})
			out = append(out, config.Diagnostic{Path: p.f.Dir, Code: "extension_unavailable",
				Message: fmt.Sprintf("package %s could not start to validate its config: %s (%s). %s. See %s", p.f.Manifest.Name, d.Summary, d.Code, d.Hint, d.Docs)})
		}
	}
	return out
}

// offlinePackage is one installed package started on first use to
// validate config offline.
//
// The daemon keeps one per catalogued package, for a package that isn't
// running: env gives it the grant the package would start with, and it is
// stopped once idle (close) and starts again when next asked. last keeps
// the remote it ran, so what the package said when it validated a config
// object is still known after it stops.
type offlinePackage struct {
	f       extproc.Found
	passEnv []string
	env     func() []string

	mu      sync.Mutex
	started bool
	r       *extproc.Remote
	last    *extproc.Remote
	dir     string
	err     error
	used    time.Time
}

func (p *offlinePackage) validator(kind, typ string) func(json.RawMessage, []string) error {
	return func(raw json.RawMessage, secretNames []string) error {
		r, err := p.remote()
		if err != nil {
			return err
		}
		return r.Validator(kind, typ)(raw, secretNames)
	}
}

func (p *offlinePackage) bindingCheck(handler string) func(extension.RouteBinding) error {
	return func(b extension.RouteBinding) error {
		r, err := p.remote()
		if err != nil {
			return err
		}
		return r.BindingCheck(handler)(b)
	}
}

// remote starts the package on first use.
func (p *offlinePackage) remote() (*extproc.Remote, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.started {
		p.started = true
		p.start()
		if p.r != nil {
			p.last = p.r
		}
	}
	p.used = time.Now()
	return p.r, p.err
}

// validated is the remote that last validated config, running or stopped,
// whose answers about the objects it validated are kept; nil before any.
func (p *offlinePackage) validated() *extproc.Remote {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.last
}

// idle reports whether the package runs and has gone unused for d.
func (p *offlinePackage) idle(now time.Time, d time.Duration) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.started && now.Sub(p.used) >= d
}

func (p *offlinePackage) start() {
	if p.dir, p.err = os.MkdirTemp("", "open-conduit-validate-"); p.err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), offlineStartTimeout)
	defer cancel()
	// An empty generation, with the KV layout reported current so the
	// package never migrates: there is no store to migrate.
	kv := p.f.Manifest.KVVersion
	passEnv := p.passEnv
	if p.env != nil {
		passEnv = p.env()
	}
	p.r, p.err = extproc.Load(ctx, p.f, extproc.Deps{Now: time.Now}, extproc.Options{StateDir: filepath.Join(p.dir, "state"), Host: protocol.HostInfo{Name: "open-conduit"},
		PassEnv:   func() []string { return passEnv },
		Configure: func() protocol.ConfigureParams { return protocol.ConfigureParams{KV: protocol.KVState{Stored: kv}} }})
}

// close stops the package and removes its state; it starts afresh when
// next asked.
func (p *offlinePackage) close() {
	p.mu.Lock()
	r, dir := p.r, p.dir
	p.started, p.r, p.dir, p.err = false, nil, "", nil
	p.mu.Unlock()
	if r != nil {
		_ = r.Process().Close()
	}
	if dir != "" {
		_ = os.RemoveAll(dir)
	}
}

// openExtensions discovers and admits the packages into the catalogue. It
// starts none: startPackages does, for the packages something needs
// (demand.go). It runs before the configuration opens, so the catalogue's
// validators can check it.
func (d *daemon) openExtensions(ctx context.Context) {
	x := &d.external
	x.root = extensionsRoot(d.o.ConfigPath)
	x.byName, x.tasks, x.running = map[string]*installed{}, map[string]time.Time{}, map[string]bool{}
	x.found, x.order, x.retiring = map[string]extproc.Found{}, map[string]int{}, map[string]*retiring{}
	if x.sources == nil {
		x.sources = map[string]bool{}
	}
	d.publishRunning()
	found, errs := discoverPackages(d.o.ConfigPath, "")
	for _, err := range errs {
		name, _, _ := strings.Cut(err.Error(), ":")
		x.failed = append(x.failed, extproc.Diagnose(name, err, extproc.Paths{}))
	}
	if len(found) == 0 {
		return
	}
	x.startGrants = d.startGrants(ctx)
	if err := extproc.CheckConflicts(found, takenNames(registry(nil))); err != nil {
		for _, f := range found {
			x.failed = append(x.failed, extproc.Diagnose(f.Manifest.Name, err, extproc.Paths{}))
		}
		d.log.Error("installed extensions refused: registration conflict", "error", err)
		return
	}
	for i, f := range found {
		name := f.Manifest.Name
		v := &offlinePackage{f: f, env: func() []string { return d.packagePassEnv(name) }}
		x.catalogue = append(x.catalogue, &installed{m: f.Manifest, d: d, dir: f.Dir, offline: v})
		x.found[name], x.order[name] = f, i
	}
}

// configRegistry is the serve registry: the host's own registrations and
// every catalogued package's declared types and executors, each validated
// by the package itself, through its running process or, when it isn't
// running, a validation process.
func (d *daemon) configRegistry() config.Registry {
	executors := map[string]extension.Executor{}
	cat := d.catalogued()
	for _, x := range cat {
		for _, e := range x.m.Provides.Executors {
			executors[e.Name] = manifestExecutor{decl: e, x: x}
		}
	}
	reg := registry(executors)
	for _, x := range cat {
		x.providerTypes(&reg)
	}
	packageRegistry(&reg, cat)
	return reg
}

func (d *daemon) extensionPackages() []*installed {
	d.external.mu.Lock()
	defer d.external.mu.Unlock()
	return append([]*installed(nil), d.external.packages...)
}

// usesExtension reports whether a scope's configuration uses one of the
// package's declared types, handlers, executors or resource, sandbox or
// container providers.
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
	sandboxes := map[string]bool{}
	for _, s := range p.Sandboxes {
		sandboxes[s.Type] = true
	}
	containers := map[string]bool{}
	for _, c := range p.Containers {
		containers[c.Type] = true
	}
	for _, b := range scope.Bindings {
		if executors[b.Executor] || b.Resource != nil && resources[b.Resource.Provider] || b.Sandbox != nil && sandboxes[b.Sandbox.Provider] || b.Container != nil && containers[b.Container.Provider] {
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
		Secrets:        d.scopedSecrets(m.Name, extTypes, connTypes),
		BindingSecrets: d.bindingSecrets(m),
		Deliver: func(ctx context.Context, name string, delivery extension.Delivery) ([]domain.AcceptedEvent, []bool, error) {
			accepted, added, err := d.ingress.Deliver(ctx, name, delivery)
			if err == nil {
				d.poke()
			}
			return accepted, added, err
		},
		Artifacts: d.artifacts,
		Timers:    currentTimers{d},
		CancelWork: func(ctx context.Context, work domain.ID, expected domain.Revision, reason, key string) (domain.ID, error) {
			// A package's first configure comes at its handshake, before a
			// generation is active (currentTimers).
			c := d.current.Load()
			if c == nil {
				return "", errNoGeneration
			}
			return c.control.CancelWork(ctx, work, expected, reason, key)
		},
		WaitProgress: func(ctx context.Context, owner string, work domain.ID, progress string) error {
			return d.waitProgress(owner)(ctx, work, progress)
		},
		Nudge:   d.poke,
		Capture: d.telemetry.Capture,
		Act: func(ctx context.Context, owner string, r extension.ActRequest) (extension.ActResult, error) {
			return d.act(owner)(ctx, r)
		},
		Actions: func(ctx context.Context, owner string, scope, work domain.ID) ([]extension.ActionOffer, error) {
			return d.listActions(owner)(ctx, scope, work)
		},
		Requests: func(ctx context.Context, owner string, scope, work domain.ID) ([]extension.RequestOffer, error) {
			return d.listRequests(owner)(ctx, scope, work)
		},
		Probes: d.setupProbes,
		Config: d.configHost(),
		SettleHold: func(ctx context.Context, owner string, work domain.ID, admit bool, reason, key string) (domain.ID, error) {
			c := d.current.Load()
			if c == nil {
				return "", errNoGeneration
			}
			return c.control.SettleGateHold(ctx, owner, work, admit, reason, key)
		},
		Render: func(ctx context.Context, e domain.Event) (string, bool) { return d.packageRender(e) },
		WaitTarget: func(ctx context.Context, scope domain.ID, kind, name string) (domain.CorrelationKey, []string, bool) {
			return d.waitTarget(kind)(ctx, scope, name)
		},
		Authorize:    d.authorize,
		EntrySandbox: d.entrySandbox(m),
		Sites:        d.resolveSite,
	}
}

// waitProgress sets the progress line of a Work's active Wait for owner,
// the extension whose operation declared it (runtime.SetWaitProgressInTx).
func (d *daemon) waitProgress(owner string) func(ctx context.Context, work domain.ID, progress string) error {
	return func(ctx context.Context, work domain.ID, progress string) error {
		return d.coord.Transact(ctx, storage.Guard{Now: d.now()}, func(tx storage.Tx) error {
			return runtime.SetWaitProgressInTx(tx, owner, work, progress)
		})
	}
}

// scopedSecrets resolves, for an extension whose entry and connection types
// are extTypes and connTypes, only names bound to its own entries in the
// scope: their selected secret names, or the secret names of a connection
// of one of its types that one of them or one of the scope's bindings names
// (B05, config.Snapshot.ExtensionConnection). It is the one host resolver
// for every package, and resolves for pkg, so a provider package is never
// asked for a credential of its own through a scheme it serves.
func (d *daemon) scopedSecrets(pkg string, extTypes, connTypes map[string]bool) func(ctx context.Context, id domain.ID, connection string, names []string) (map[string]string, error) {
	return func(ctx context.Context, id domain.ID, connection string, names []string) (map[string]string, error) {
		resolver := d.resolver.For(pkg)
		s, _ := d.extensionSnapshot()
		if s == nil || s.Scopes[id] == nil {
			return nil, extension.ErrUnauthorized
		}
		scope := s.Scopes[id]
		if connection == "" {
			allowed := map[string]bool{}
			for _, x := range scope.Extensions {
				if extTypes[x.Type] {
					for _, n := range x.SecretNames {
						allowed[n] = true
					}
				}
			}
			for _, n := range names {
				if !allowed[n] {
					return nil, extension.ErrUnauthorized
				}
			}
			return resolver.ResolveNames(ctx, scope, names)
		}
		c, ok := s.ExtensionConnection(id, connection, extTypes, connTypes)
		if !ok {
			return nil, extension.ErrUnauthorized
		}
		out := map[string]string{}
		for _, n := range names {
			ref, ok := c.SecretRefs[n]
			if !ok || ref == "" {
				return nil, extension.ErrUnauthorized
			}
			v, err := resolver.ResolveNamed(ctx, n, ref)
			if err != nil {
				return nil, err
			}
			out[n] = v
		}
		return out, nil
	}
}

// bindingSecrets resolves names a binding in an enabled scope selects,
// for the package whose executor the binding names: the values each of the
// binding's runs is given (ConfigService.Secrets), which the executor's
// doctor check of the binding's credential needs. Any other binding, or a
// name the binding doesn't select, is refused.
func (d *daemon) bindingSecrets(m protocol.Manifest) func(ctx context.Context, id domain.ID, binding string, names []string) (map[string]string, error) {
	own := map[string]bool{}
	for _, e := range m.Provides.Executors {
		own[e.Name] = true
	}
	return func(ctx context.Context, id domain.ID, binding string, names []string) (map[string]string, error) {
		s, _ := d.extensionSnapshot()
		if s == nil || s.Scopes[id] == nil || !s.Scopes[id].Enabled {
			return nil, extension.ErrUnauthorized
		}
		scope := s.Scopes[id]
		b, ok := scope.Binding(binding)
		if !ok || !own[b.Executor] {
			return nil, extension.ErrUnauthorized
		}
		for _, n := range names {
			if !slices.Contains(b.SecretNames, n) {
				return nil, extension.ErrUnauthorized
			}
		}
		if len(names) == 0 {
			return map[string]string{}, nil
		}
		return d.resolver.For(m.Name).ResolveNames(ctx, scope, names)
	}
}

// currentTimers arms through the current generation's timers. A package's
// first configure comes at its handshake, before a generation is active;
// a timer call then fails (errNoGeneration) and the package arms at the
// configure that follows activation (configureExtensions).
type currentTimers struct{ d *daemon }

func (t currentTimers) ArmRecurrence(ctx context.Context, scope domain.ID, timer domain.Timer, expected domain.Revision, kind string, spec []byte) error {
	c := t.d.current.Load()
	if c == nil {
		return errNoGeneration
	}
	return c.timers.ArmRecurrence(ctx, scope, timer, expected, kind, spec)
}
func (t currentTimers) Cancel(ctx context.Context, id domain.ID, expected domain.Revision) error {
	c := t.d.current.Load()
	if c == nil {
		return errNoGeneration
	}
	return c.timers.Cancel(ctx, id, expected)
}

// startGrants are the package grants to start packages with before the
// configuration opens: the committed generation's (validated when it was
// applied), or on a first start the config file's (PackageGrants leaves
// out what validation would refuse). None when neither can be read.
func (d *daemon) startGrants(ctx context.Context) map[string][]string {
	generation, normalized, err := management.StoredGeneration(ctx, d.coord, d.now)
	switch {
	case err != nil:
		return nil
	case generation != 0:
		doc, err := config.Parse(normalized)
		if err != nil {
			return nil
		}
		return doc.PackageGrants()
	case d.o.ConfigPath == "":
		return nil
	}
	data, err := config.ReadFile(d.o.ConfigPath)
	if err != nil {
		return nil
	}
	return config.FileGrants(data)
}

// packagePassEnv is the grant a package starts with now: the published
// generation's, or before one is published the start grant.
func (d *daemon) packagePassEnv(name string) []string {
	if s, _ := d.extensionSnapshot(); s != nil {
		return s.Settings.PackagePassEnv(name)
	}
	return slices.Clone(d.external.startGrants[name])
}

func (d *daemon) extensionOptions(f extproc.Found, paths extproc.Paths) extproc.Options {
	name := f.Manifest.Name
	executors := map[string]string{}
	for _, e := range f.Manifest.Provides.Executors {
		executors[e.Name] = executorDir(d.o.StateDir, e.Name)
	}
	resources := map[string]string{}
	for _, r := range f.Manifest.Provides.Resources {
		resources[r.Type] = resourceDir(d.o.StateDir, r.Type)
	}
	var containers map[string]string
	var containerHost string
	var formerHosts []string
	var env []string
	if len(f.Manifest.Provides.Containers) > 0 {
		hosts := d.containerHosts()
		containers, containerHost, formerHosts, env = map[string]string{}, hosts.Host, hosts.Former, containerSwitches(os.Environ())
		for _, c := range f.Manifest.Provides.Containers {
			containers[c.Type] = containerDir(d.o.StateDir, c.Type)
		}
	}
	var configDir string
	if d.o.ConfigPath != "" {
		configDir = filepath.Dir(d.o.ConfigPath)
	}
	return extproc.Options{
		StateDir: paths.State, LogDir: filepath.Dir(paths.Log), ExecutorDirs: executors, ResourceDirs: resources, ConfigDir: configDir,
		ContainerDirs: containers, ContainerHost: containerHost, ContainerFormerHosts: formerHosts, Env: env,
		Host:      protocol.HostInfo{Name: "open-conduit", Version: d.o.Version},
		PassEnv:   func() []string { return d.packagePassEnv(name) },
		Configure: func() protocol.ConfigureParams { return d.extensionConfigure(f.Manifest) },
		OnConfigured: func(res protocol.ConfigureResult) {
			d.extensionConfigured(name, res)
		},
		KV:  extproc.StoreKV{Store: d.coord, Now: d.now},
		Log: d.log,
	}
}

// extensionConfigure builds a package's configure params from the current
// generation.
func (d *daemon) extensionConfigure(m protocol.Manifest) protocol.ConfigureParams {
	types := map[string]bool{}
	for _, x := range m.Provides.Extensions {
		types[x.Type] = true
	}
	connTypes := map[string]bool{}
	for _, c := range m.Provides.Connections {
		connTypes[c.Type] = true
	}
	params := d.configureParams(types, connTypes)
	params.Containers = d.containerUses(m)
	return params
}

// containerUses are the enabled scopes' bindings whose container provider
// the manifest declares, in config order (protocol.ConfigureParams.Containers).
func (d *daemon) containerUses(m protocol.Manifest) []protocol.ContainerUse {
	own := map[string]bool{}
	for _, c := range m.Provides.Containers {
		own[c.Type] = true
	}
	s, _ := d.extensionSnapshot()
	if s == nil || len(own) == 0 {
		return nil
	}
	var out []protocol.ContainerUse
	for _, id := range s.Order {
		scope := s.Scopes[id]
		if scope == nil || !scope.Enabled {
			continue
		}
		for _, b := range scope.Bindings {
			if b.Container != nil && own[b.Container.Provider] {
				out = append(out, protocol.ContainerUse{ScopeID: string(id), Binding: b.Name, Provider: b.Container.Provider, Config: b.Container.Config})
			}
		}
	}
	return out
}

// configureParams are an extension's configure params: every enabled
// scope's entries of its entry types, with the connection each names and
// the connections the scope's bindings name, when they are of one of its
// connection types.
func (d *daemon) configureParams(types, connTypes map[string]bool) protocol.ConfigureParams {
	params := protocol.ConfigureParams{DryRun: d.o.DryRun}
	d.hooks.mu.Lock()
	params.PublicURL = d.hooks.public
	d.hooks.mu.Unlock()
	s, gen := d.extensionSnapshot()
	if s == nil {
		return params
	}
	params.Generation = strconv.FormatUint(gen, 10)
	for _, id := range s.Order {
		scope := s.Scopes[id]
		if scope == nil {
			continue
		}
		if !scope.Enabled {
			for _, x := range scope.Extensions {
				if types[x.Type] {
					params.Disabled = append(params.Disabled, protocol.ConfigEntry{ScopeID: string(id), Type: x.Type, Name: x.Name, Config: x.Config, Secrets: x.SecretNames})
				}
			}
			continue
		}
		var bound []protocol.ConnectionEntry
		for _, c := range s.BindingConnections(id, connTypes) {
			bound = append(bound, connectionEntry(c))
		}
		for _, x := range scope.Extensions {
			if !types[x.Type] {
				continue
			}
			entry := protocol.ConfigEntry{ScopeID: string(id), Type: x.Type, Name: x.Name, Config: x.Config, Secrets: x.SecretNames, BindingConnections: bound}
			for _, c := range s.Connections {
				if c.ID == x.Connection && connTypes[c.Type] {
					ce := connectionEntry(c)
					entry.Connection = &ce
				}
			}
			params.Entries = append(params.Entries, entry)
		}
	}
	return params
}

// connectionEntry is a connection as configure sends it: its config and
// secret names, never reference text.
func connectionEntry(c config.Connection) protocol.ConnectionEntry {
	names := make([]string, 0, len(c.SecretRefs))
	for n := range c.SecretRefs {
		names = append(names, n)
	}
	sort.Strings(names)
	return protocol.ConnectionEntry{ID: c.ID, Type: c.Type, Config: c.Config, Secrets: names}
}

// extensionConfigured records what a package's configure result declares
// and registers its new ingress sources at once (a package that restarts
// configures again); configurePackages registers them after each apply too.
func (d *daemon) extensionConfigured(name string, res protocol.ConfigureResult) {
	d.external.mu.Lock()
	pkg := d.external.byName[name]
	d.external.mu.Unlock()
	if pkg == nil {
		return
	}
	pkg.configured(res)
	d.registerSources(pkg, pkg.Sources())
}

func (d *daemon) lifetime() context.Context {
	if d.ctx != nil {
		return d.ctx
	}
	return context.Background()
}

// configureExtensions configures every package, bundled and installed
// (config apply, tunnel URL change).
func (d *daemon) configureExtensions(ctx context.Context) {
	d.configurePackages(ctx)
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
	changed := public != d.external.public
	d.external.mu.Unlock()
	if changed {
		d.configureExtensions(ctx)
	}
}

// runTask starts an extension task, keyed extension/task, on its own
// goroutine once pace has elapsed since its last start; one task never
// overlaps itself. Its error is reported as an extension_task diagnostic.
func (d *daemon) runTask(ctx context.Context, key string, pace time.Duration, run func(context.Context) error) {
	d.external.mu.Lock()
	if d.external.tasks == nil {
		d.external.tasks, d.external.running = map[string]time.Time{}, map[string]bool{}
	}
	due := !d.external.running[key] && d.now().Sub(d.external.tasks[key]) >= pace
	if due {
		d.external.running[key], d.external.tasks[key] = true, d.now()
	}
	d.external.mu.Unlock()
	if !due {
		return
	}
	d.goSafe("extension_task", func() {
		defer func() {
			d.external.mu.Lock()
			d.external.running[key] = false
			d.external.mu.Unlock()
		}()
		cx, cancel := context.WithTimeout(ctx, min(max(pace, time.Minute), 10*time.Minute))
		defer cancel()
		if err := run(cx); err != nil && ctx.Err() == nil {
			d.report("extension_task", domain.ID(key), errorCode(err))
		} else {
			d.clear("extension_task", domain.ID(key))
		}
	})
}

// extensionCheck reports each installed package's own health for doctor:
// refused or failed packages, process state, version and restarts. Its
// declared checks are listed under their own names (packageChecks).
func (d *daemon) extensionCheck(context.Context) runtime.Check {
	check := runtime.Check{Name: "extensions", Status: runtime.CheckOK, Code: "ok"}
	raise := func(status, code string) {
		if rank(status) > rank(check.Status) {
			check.Status, check.Code = status, code
		}
	}
	d.external.mu.Lock()
	failed := append([]extproc.Diagnostic(nil), d.external.failed...)
	retiringFor := map[string]int{}
	for name, r := range d.external.retiring {
		retiringFor[name] = len(r.ids)
	}
	d.external.mu.Unlock()
	packages := d.extensionPackages()
	running := map[string]bool{}
	for _, x := range packages {
		running[x.Name()] = true
	}
	// The installed packages nothing needs are listed as such, so doctor
	// shows what is installed as well as what runs.
	unfailed := func(name string) bool {
		return !slices.ContainsFunc(failed, func(f extproc.Diagnostic) bool { return f.Extension == name })
	}
	var idle []string
	for _, c := range d.catalogued() {
		if !running[c.Name()] && unfailed(c.Name()) {
			idle = append(idle, fmt.Sprintf("%s %s: not running (nothing uses it)", c.Name(), c.m.Version))
		}
	}
	if len(packages) == 0 && len(failed) == 0 && len(idle) == 0 {
		check.Status, check.Code, check.Detail = runtime.CheckInfo, "not_installed", "no extension packages are installed"
		return check
	}
	for _, f := range failed {
		raise(runtime.CheckFail, f.Code)
		check.Items = append(check.Items, f.String())
	}
	for _, x := range packages {
		p, m := x.r.Process(), x.m
		state := "running"
		if err := p.Err(); err != nil {
			diag := extproc.Diagnose(m.Name, err, extproc.StatePaths(d.o.StateDir, m.Name))
			raise(runtime.CheckFail, diag.Code)
			state = "stopped: " + diag.Summary + " (" + diag.Code + ")"
		}
		item := fmt.Sprintf("%s %s: %s, protocol %d, %d restart(s)", m.Name, m.Version, state, protocol.Version, p.Restarts())
		if n := x.r.SchemaSkips(); n > 0 {
			item += fmt.Sprintf(", %d unreadable payload(s) skipped", n)
		}
		if p.Restarts() > 0 {
			raise(runtime.CheckWarn, "extension_restarted")
		}
		env, pending := grantItem(p.PassedEnv(), d.packagePassEnv(m.Name), p.Options().Environ)
		item += env
		if n, ok := retiringFor[m.Name]; ok {
			item += fmt.Sprintf(", unused: stops once the %d live execution(s) that may need it finish", n)
		}
		if pending {
			raise(runtime.CheckWarn, "extension_grant_pending")
		}
		check.Items = append(check.Items, item)
	}
	check.Items = append(check.Items, idle...)
	sort.Strings(check.Items)
	return check
}

// grantItem describes a package's environment grant for doctor, names only:
// the granted variables it started with, those granted but unset in the
// daemon's environment, and pending when the current generation grants
// other names than it started with, which it takes at its next start.
func grantItem(passed, granted, environ []string) (string, bool) {
	set := map[string]bool{}
	for _, kv := range environ {
		name, _, _ := strings.Cut(kv, "=")
		set[name] = true
	}
	var unset, want []string
	for _, name := range granted {
		if set[name] {
			want = append(want, name)
		} else {
			unset = append(unset, name)
		}
	}
	slices.Sort(want)
	slices.Sort(unset)
	var out string
	if len(passed) > 0 {
		out += ", pass_env " + strings.Join(passed, " ")
	}
	if len(unset) > 0 {
		out += ", pass_env not set in the daemon's environment: " + strings.Join(unset, " ")
	}
	pending := !slices.Equal(passed, want)
	if pending {
		out += ", pass_env changed since it started (restart the daemon to apply)"
	}
	return out, pending
}

// extensionExporter fans telemetry out to the packages. It joins the
// pipeline at start, before packages load, and reaches whichever are
// running through their contract exporter.
type extensionExporter struct{ d *daemon }

func (e extensionExporter) Export(ctx context.Context, records []observability.Record) error {
	var errs []error
	for _, x := range e.d.extensionPackages() {
		errs = append(errs, x.Exporter().Export(ctx, records))
	}
	return errors.Join(errs...)
}
func (e extensionExporter) Shutdown(context.Context) error { return nil }

// executorDir is an executor's state directory, <state>/executors/<name>:
// the same whether it runs in process or in a package, so its runs live
// under one root the host's sandbox and container guards know (RunRoot)
// and survive a move between the two.
func executorDir(state, name string) string { return filepath.Join(state, "executors", name) }

// resourceDir is a resource provider type's state directory, the same in
// process or out (protocol.InitializeParams.ResourceDirs).
func resourceDir(state, typ string) string { return filepath.Join(state, "resources", typ) }

// containerDir is a container provider type's state directory
// (protocol.InitializeParams.ContainerDirs).
func containerDir(state, typ string) string { return filepath.Join(state, "containers", typ) }

// containerSwitchNames are the host's container switches: the gateway
// image a build without a pinned one runs (make gateway-image), whether a
// missing gateway image is pulled (off on a host with no registry access),
// and the toolbelt forward a test forces. They are the operator's settings for
// the host's container isolation, never secrets, so the host sets them
// itself in a container provider package's environment, which the
// sanitized baseline and the operator's grant (OPEN_CONDUIT_* is the
// host's own) would otherwise never carry.
var containerSwitchNames = []string{"OPEN_CONDUIT_GATEWAY_IMAGE", "OPEN_CONDUIT_GATEWAY_PULL", "OPEN_CONDUIT_CONTAINER_FORWARD"}

// containerSwitches are the container switches set in environ.
func containerSwitches(environ []string) []string {
	var out []string
	for _, kv := range environ {
		name, _, _ := strings.Cut(kv, "=")
		if slices.Contains(containerSwitchNames, name) {
			out = append(out, kv)
		}
	}
	return out
}
