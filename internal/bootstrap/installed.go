package bootstrap

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strconv"
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
	"github.com/open-conduit/open-conduit/storage"
)

// installed is one extension package, bundled or installed, as bootstrap
// composes it: every surface reads the manifest and goes through the
// running package (extproc.Remote). Every extension, first-party or not,
// runs as a package, so this is the only shape composition knows; its
// resource, sandbox, container and tunnel providers are registered where
// the package starts (extensions.go).
//
// Offline (config validation) r is nil: offline starts the package to
// validate its declared types when a config object of one needs checking;
// with neither, they accept any config and nothing runs.
type installed struct {
	m protocol.Manifest
	r *extproc.Remote
	d *daemon
	// dir is the installed package directory (its playbooks).
	dir string
	// offline starts the package to validate config when there is no
	// daemon (offlineExternal).
	offline *offlinePackage

	// mu guards what the package's configure results declared: its ingress
	// sources (by name, the first declaration kept) and webhook paths.
	mu      sync.Mutex
	sources []protocol.SourceDecl
	hooks   map[string]bool
}

var _ extension.ConfigCandidate = (*installed)(nil)

// configType is a config type a package declares. Kind is
// protocol.ValidateConnection, protocol.ValidateExtension or
// protocol.ValidateRoute; Validate checks a config object through the
// package (nil accepts any), Binding a route's binding for a handler that
// declares a binding check, and Settings and Describe an entry's safe
// fields and what it sets up; WorkKinds, for a route type, the kinds of
// child Work a route's config binds; Bindings, for an extension type, the
// binding names an entry's config refers to.
type configType struct {
	Kind      string
	Type      string
	Ingress   bool
	Validate  func(config json.RawMessage, secretNames []string) error
	Binding   func(extension.RouteBinding) error
	Settings  func(config json.RawMessage) []extension.Setting
	Describe  func(config json.RawMessage) []config.Entry
	WorkKinds func(config json.RawMessage) []extension.WorkKind
	Bindings  func(config json.RawMessage) []string
}

// ingressSource is an ingress source a package's configure result
// declared; without an Interval the host's reconcile setting applies.
type ingressSource struct {
	Name     string
	Source   extension.Source
	Ingress  extension.Ingress
	Interval time.Duration
}

// packageExecutor is an executor a package declares, with the capacity
// pool it counts against ("" for agent turns, jobPool for jobs).
type packageExecutor struct {
	Executor extension.Executor
	Pool     string
}

// workKind is the lifecycle of a Work kind a package declares. Timed
// marks a kind that runs on a clock, which a dry run never starts.
type workKind struct {
	Kind     string
	Standing bool
	RunOnce  bool
	Timed    bool
	Trigger  func(inputs []domain.Event) int
}

// task is a package's paced background task.
type task struct {
	Name string
	Pace time.Duration
	Run  func(ctx context.Context) error
}

// packageCheck is a doctor check a package declares: Deep runs only with
// --deep, Offline also without a daemon.
type packageCheck struct {
	Deep    bool
	Offline bool
	Run     func(ctx context.Context, cfg protocol.CheckConfig) runtime.Check
}

func (x *installed) Name() string { return x.m.Name }

// Title is the manifest's display name.
func (x *installed) Title() string { return x.m.Title }

func (x *installed) validator(kind, typ string) func(json.RawMessage, []string) error {
	switch {
	case x.r != nil:
		return x.r.Validator(kind, typ)
	case x.offline != nil:
		return x.offline.validator(kind, typ)
	}
	return nil
}

// Types are the manifest's connection, extension and configurable route
// types, validated through the package.
func (x *installed) Types() []configType {
	p := x.m.Provides
	var out []configType
	for _, c := range p.Connections {
		out = append(out, configType{Kind: protocol.ValidateConnection, Type: c.Type, Validate: x.validator(protocol.ValidateConnection, c.Type)})
	}
	for _, e := range p.Extensions {
		out = append(out, configType{Kind: protocol.ValidateExtension, Type: e.Type, Ingress: e.Ingress, Validate: x.validator(protocol.ValidateExtension, e.Type),
			Settings: declaredSettings(e.Settings), Describe: x.describe(protocol.ValidateExtension, e.Type, declaredDescribe(e.Describe)), Bindings: x.bindings(e.Type)})
	}
	for _, r := range p.Routes {
		if r.Config || r.Binding {
			t := configType{Kind: protocol.ValidateRoute, Type: r.Handler}
			if r.Config {
				t.Validate = x.validator(protocol.ValidateRoute, r.Handler)
				t.Describe = x.describe(protocol.ValidateRoute, r.Handler, nil)
				t.WorkKinds = x.workKinds(r.Handler)
			}
			if r.Binding {
				t.Binding = x.bindingCheck(r.Handler)
			}
			out = append(out, t)
		}
	}
	return out
}

// bindingCheck is a handler's binding check, through the package.
func (x *installed) bindingCheck(handler string) func(extension.RouteBinding) error {
	switch {
	case x.r != nil:
		return x.r.BindingCheck(handler)
	case x.offline != nil:
		return x.offline.bindingCheck(handler)
	}
	return nil
}

// Playbooks are the package's declared playbooks (extproc.Playbooks), for
// the agent executors' plugin. One that cannot be read is logged and left
// out.
func (x *installed) Playbooks() map[string]string {
	books, err := extproc.Playbooks(x.dir, x.m)
	if err != nil && x.d != nil {
		x.d.log.Warn("extension playbooks not read", "extension", x.m.Name, "error", err)
	}
	return books
}

// entryGated says whether the package's operations join only a generation
// with one of its entries (provides.entry_gated).
func (x *installed) entryGated() bool { return x.m.Provides.EntryGated }

// remote is the running package, started on first use offline.
func (x *installed) remote() (*extproc.Remote, error) {
	switch {
	case x.r != nil:
		return x.r, nil
	case x.offline != nil:
		return x.offline.remote()
	}
	return nil, errNoPackage
}

// errNoPackage is a package with neither a process nor an offline start.
var errNoPackage = errors.New("extension package is not running")

// providerTypes adds the package's name (Registry.Packages) and its
// manifest's resource, sandbox, container and tunnel provider types to a registry:
// each validates its own config through the package, and a
// resource type's workspace checks (mode_default, sandbox) are answered
// through it too.
func (x *installed) providerTypes(reg *config.Registry) {
	if reg.Packages == nil {
		reg.Packages = map[string]bool{}
	}
	reg.Packages[x.m.Name] = true
	if len(x.m.Provides.Containers) > 0 && reg.Containers == nil {
		reg.Containers = map[string]func(json.RawMessage) error{}
	}
	for _, c := range x.m.Provides.Containers {
		v := x.validator(protocol.ValidateContainer, c.Type)
		if v == nil {
			v = func(json.RawMessage, []string) error { return nil }
		}
		reg.Containers[c.Type] = func(raw json.RawMessage) error { return v(raw, nil) }
	}
	if len(x.m.Provides.Sandboxes) > 0 && reg.Sandboxes == nil {
		reg.Sandboxes = map[string]func(json.RawMessage) error{}
	}
	for _, s := range x.m.Provides.Sandboxes {
		v := x.validator(protocol.ValidateSandbox, s.Type)
		if v == nil {
			v = func(json.RawMessage, []string) error { return nil }
		}
		reg.Sandboxes[s.Type] = func(raw json.RawMessage) error { return v(raw, nil) }
	}
	if len(x.m.Provides.Tunnels) > 0 && reg.Tunnels == nil {
		reg.Tunnels = map[string]func(json.RawMessage) error{}
	}
	for _, t := range x.m.Provides.Tunnels {
		v := x.validator(protocol.ValidateTunnel, t.Type)
		if v == nil {
			v = func(json.RawMessage, []string) error { return nil }
		}
		reg.Tunnels[t.Type] = func(raw json.RawMessage) error { return v(raw, nil) }
	}
	if reg.Resources == nil {
		reg.Resources = map[string]bool{}
	}
	if reg.ResourceConfig == nil {
		reg.ResourceConfig = map[string]func(json.RawMessage) error{}
	}
	for _, r := range x.m.Provides.Resources {
		reg.Resources[r.Type] = true
		if s := declaredSettings(r.Settings); s != nil {
			if reg.ResourceSettings == nil {
				reg.ResourceSettings = map[string]func(json.RawMessage) []extension.Setting{}
			}
			reg.ResourceSettings[r.Type] = s
		}
		v := x.validator(protocol.ValidateResource, r.Type)
		if v == nil {
			v = func(json.RawMessage, []string) error { return nil }
		}
		reg.ResourceConfig[r.Type] = func(raw json.RawMessage) error { return v(raw, nil) }
		typ := r.Type
		if r.ModeDefault {
			if reg.ResourceModeDefault == nil {
				reg.ResourceModeDefault = map[string]func(json.RawMessage) string{}
			}
			reg.ResourceModeDefault[typ] = func(raw json.RawMessage) string {
				p, err := x.remote()
				if err != nil {
					return ""
				}
				return p.ResourceModeDefault(typ)(raw)
			}
		}
		if r.Sandbox {
			if reg.ResourceSandbox == nil {
				reg.ResourceSandbox = map[string]func(json.RawMessage, string) error{}
			}
			reg.ResourceSandbox[typ] = func(raw json.RawMessage, mode string) error {
				p, err := x.remote()
				if err != nil {
					// The diagnostics say why it didn't start
					// (extension_unavailable); the refusal stays fixed.
					return errors.New("the resource provider could not start to check this workspace for a sandbox")
				}
				return p.ResourceSandbox(typ)(raw, mode)
			}
		}
	}
}

// Toolsets are the package's declared toolsets (provides.toolsets),
// checked with the manifest at discovery.
func (x *installed) Toolsets() []extension.Toolset {
	var out []extension.Toolset
	for _, t := range x.m.Provides.Toolsets {
		out = append(out, extension.Toolset{Name: t.Name, Summary: t.Summary, Operations: slices.Clone(t.Operations)})
	}
	return out
}

func (x *installed) Operations() []string {
	var out []string
	for _, o := range x.m.Provides.Operations {
		out = append(out, o.Name)
	}
	return out
}

// Routes are the declared handlers (named only, offline).
func (x *installed) Routes() map[string]extension.RouteHandler {
	out := map[string]extension.RouteHandler{}
	for _, r := range x.m.Provides.Routes {
		if x.r != nil {
			out[r.Handler] = x.r.RouteHandler(r.Handler)
		} else {
			out[r.Handler] = nil
		}
	}
	return out
}

// Configure re-sends configure: the package builds its params from the
// current generation itself (extproc.Options.Configure), the same params
// p carries. A failure is reported and retried at the next apply or URL
// change.
func (x *installed) Configure(ctx context.Context) {
	cx, cancel := context.WithTimeout(ctx, 30*time.Second)
	err := x.r.Process().Reconfigure(cx)
	cancel()
	if err != nil {
		x.d.report("extension_configure", domain.ID(x.m.Name), errorCode(err))
		return
	}
	x.d.clear("extension_configure", domain.ID(x.m.Name))
}

// configured records the ingress sources and webhook paths a configure
// result declares (extproc has checked they are the package's own).
func (x *installed) configured(res protocol.ConfigureResult) {
	x.mu.Lock()
	defer x.mu.Unlock()
	for _, s := range res.Sources {
		if !slices.ContainsFunc(x.sources, func(o protocol.SourceDecl) bool { return o.Name == s.Name }) {
			x.sources = append(x.sources, s)
		}
	}
	if x.hooks == nil {
		x.hooks = map[string]bool{}
	}
	for _, path := range res.WebhookPaths {
		x.hooks[strings.TrimPrefix(path, "/")] = true
	}
}

// Sources are the ingress sources the package's configure results
// declared, each pushing through ingress.push. Without an interval, the
// host's reconcile setting applies.
func (x *installed) Sources() []ingressSource {
	if x.r == nil {
		return nil
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	out := make([]ingressSource, 0, len(x.sources))
	for _, s := range x.sources {
		out = append(out, ingressSource{Name: s.Name, Source: s.Source.Extension(), Ingress: x.r.Ingress(s), Interval: time.Duration(s.IntervalSeconds) * time.Second})
	}
	return out
}

// Webhook is the handler of a path under /<name>/ that a configure result
// declared, or beneath one, through webhook.handle; any other path has
// none.
func (x *installed) Webhook(path string) http.Handler {
	full := x.m.Name + "/" + path
	x.mu.Lock()
	ok := false
	for mount := range x.hooks {
		ok = ok || full == mount || strings.HasPrefix(full, mount+"/")
	}
	x.mu.Unlock()
	if !ok || x.r == nil {
		return nil
	}
	return x.r.Webhook("/"+full, 0)
}

// Executors are the package's declared executors, each with the capacity
// pool its manifest names, calling the package's executor methods.
func (x *installed) Executors() map[string]packageExecutor {
	out := map[string]packageExecutor{}
	if x.r == nil {
		return out
	}
	for _, e := range x.m.Provides.Executors {
		if adapter, ok := x.r.Executor(e.Name); ok {
			out[e.Name] = packageExecutor{Executor: adapter, Pool: e.Pool}
		}
	}
	return out
}

// WorkKinds are the package's declared Work kinds; a trigger kind's choice
// goes through work_kind.trigger.
func (x *installed) WorkKinds() []workKind {
	var out []workKind
	for _, k := range x.m.Provides.WorkKinds {
		w := workKind{Kind: k.Kind, Standing: k.Standing, RunOnce: k.RunOnce, Timed: k.Timed}
		if x.r != nil {
			if p, ok := x.r.WorkKind(k.Kind); ok {
				w.Trigger = p.Trigger
			}
		}
		out = append(out, w)
	}
	return out
}

// Recurrences are the package's declared timer recurrence kinds, each
// through recurrence.next.
func (x *installed) Recurrences() map[string]runtime.RecurrenceFunc {
	out := map[string]runtime.RecurrenceFunc{}
	if x.r == nil {
		return out
	}
	for _, kind := range x.m.Provides.Recurrences {
		out[kind] = x.r.Recurrence(kind)
	}
	return out
}

// Targets are the wait target kinds the package resolves (manifest
// targets), each through target.resolve.
func (x *installed) Targets() map[string]func(ctx context.Context, scope domain.ID, name string) (domain.CorrelationKey, []string, bool) {
	out := map[string]func(ctx context.Context, scope domain.ID, name string) (domain.CorrelationKey, []string, bool){}
	if x.r == nil {
		return out
	}
	for _, kind := range x.m.Provides.Targets {
		out[kind] = x.r.Target(kind)
	}
	return out
}

// SegmentEnded tells the package an execution's container segment ended
// (segment.ended), when its manifest asks to be told.
func (x *installed) SegmentEnded(ctx context.Context, execution domain.ID) {
	if x.r != nil {
		x.r.SegmentEnded(ctx, execution)
	}
}

// Stop shuts the package down (shutdown, stdin closed, then signals to its
// process group). Executor jobs it detached keep running and are adopted by
// the next daemon through executor.recover.
func (x *installed) Stop() { _ = x.r.Process().Close() }

// Registration is the package's toolbelt registration.
func (x *installed) Registration() extension.Registration { return x.r.Registration() }

// Actions offers the package's action provider, keyed by its name (or,
// entry_named, its first entry's), on a scope whose current configuration
// uses it.
func (x *installed) Actions(scope domain.ID) map[string]extension.ActionProvider {
	s, _ := x.d.extensionSnapshot()
	if s == nil || s.Scopes[scope] == nil || !s.Scopes[scope].Enabled {
		return nil
	}
	p, ok := x.r.Actions()
	if !ok || !usesExtension(s.Scopes[scope], x.m) {
		return nil
	}
	return map[string]extension.ActionProvider{x.nameIn(s.Scopes[scope]): p}
}

// nameIn is the name the package goes by on a scope: its own, or with
// entry_named the name of its first entry there (protocol.Provides).
func (x *installed) nameIn(scope *config.EffectiveScope) string {
	if x.m.Provides.EntryNamed && scope != nil {
		for _, e := range scope.Extensions {
			if slices.ContainsFunc(x.m.Provides.Extensions, func(d protocol.ExtensionDecl) bool { return d.Type == e.Type }) {
				return e.Name
			}
		}
	}
	return x.m.Name
}

// Pages are the manifest's pages.
func (x *installed) Pages() []extension.Page {
	var out []extension.Page
	for _, p := range x.m.Provides.Pages {
		out = append(out, p.Page())
	}
	return out
}

// ownsKind reports whether the manifest declares the Work kind.
func (x *installed) ownsKind(kind string) bool {
	for _, k := range x.m.Provides.WorkKinds {
		if k.Kind == kind {
			return true
		}
	}
	return false
}

// ConfigCandidate is what one of the package's Work proposes.
func (x *installed) ConfigCandidate(ctx context.Context, w domain.Work) (extension.Candidate, error) {
	if x.r == nil || !x.ownsKind(w.Kind) {
		return extension.Candidate{}, storage.ErrNotFound
	}
	return x.r.ConfigCandidate(ctx, w)
}

// Annotate is what the package says about the Work, under the name it
// goes by on the Work's scope.
func (x *installed) Annotate(ctx context.Context, w domain.Work) []management.Annotation {
	source := x.m.Name
	if x.m.Provides.EntryNamed {
		if s, _ := x.d.extensionSnapshot(); s != nil {
			source = x.nameIn(s.Scopes[w.ScopeID])
		}
	}
	var out []management.Annotation
	for _, a := range x.r.Annotate(ctx, w) {
		out = append(out, management.Annotation{Source: source, Name: a.Name, Value: a.Value})
	}
	return out
}

// Render renders the package's own families (types prefixed by its name).
func (x *installed) Render(ctx context.Context, e domain.Event) (string, bool) {
	if !strings.HasPrefix(e.Type, x.m.Name+".") {
		return "", false
	}
	return x.r.Render(ctx, e)
}

// Tasks are the manifest's tasks, paced by their intervals.
func (x *installed) Tasks() []task {
	var out []task
	for _, t := range x.m.Provides.Tasks {
		name := t.Name
		out = append(out, task{Name: name, Pace: time.Duration(t.IntervalSeconds) * time.Second, Run: func(ctx context.Context) error { return x.r.Task(ctx, name) }})
	}
	return out
}

// Checks are the declared checks, each listed in doctor under its own
// name (checkName). check.run carries the package's view of the
// configuration; a result without a status is ok or, when not OK, a warning.
func (x *installed) Checks() []packageCheck {
	var out []packageCheck
	for _, c := range x.m.Provides.Checks {
		name, deep := c.Name, c.Deep
		out = append(out, packageCheck{Deep: deep, Offline: c.Offline, Run: func(ctx context.Context, cfg protocol.CheckConfig) runtime.Check {
			res := runtime.Check{Name: checkName(x.m.Name, name), Status: runtime.CheckOK, Code: "ok"}
			var r protocol.CheckRunResult
			remote, err := x.remote()
			if err == nil {
				cx, cancel := context.WithTimeout(ctx, 20*time.Second)
				r, err = remote.Check(cx, name, deep, cfg)
				cancel()
			}
			switch {
			case err != nil:
				res.Status, res.Code, res.Detail = runtime.CheckWarn, "extension_check_failed", "unavailable ("+errorCode(err)+")"
			case r.Status != "":
				res.Status, res.Code, res.Detail, res.Items = r.Status, r.Code, r.Detail, r.Items
				switch r.Status {
				case runtime.CheckOK, runtime.CheckInfo, runtime.CheckWarn, runtime.CheckFail:
				default:
					res.Status, res.Code, res.Detail, res.Items = runtime.CheckWarn, "extension_check_invalid", "unknown status "+strconv.Quote(r.Status), nil
				}
				if res.Code == "" {
					res.Code = res.Status
				}
			case !r.OK:
				res.Status, res.Code, res.Detail = runtime.CheckWarn, "extension_check_failed", r.Detail
			default:
				res.Detail = r.Detail
			}
			return res
		}})
	}
	return out
}

// checkName is the doctor name of a package's declared check: the check's
// own name when it is the package's name or starts with "<package>_" or
// "<package>-", else "<package>_<check>", so no package's check takes
// another's or the host's.
func checkName(pkg, check string) string {
	if check == pkg || strings.HasPrefix(check, pkg+"_") || strings.HasPrefix(check, pkg+"-") {
		return check
	}
	return pkg + "_" + check
}

// Exporter forwards telemetry when the package exports it, and drops it
// otherwise.
func (x *installed) Exporter() observability.Exporter {
	if e, ok := x.r.Exporter(); ok {
		return e
	}
	return discardExporter{}
}

// Capture hands the package one of the host's telemetry events when its
// manifest declares the telemetry.sink permission, and drops it otherwise.
func (x *installed) Capture(ctx context.Context, scope, name string, at time.Time, props map[string]any) error {
	if x.r == nil {
		return nil
	}
	return x.r.Capture(ctx, scope, name, at, props)
}

type discardExporter struct{}

func (discardExporter) Export(context.Context, []observability.Record) error { return nil }
func (discardExporter) Shutdown(context.Context) error                       { return nil }

// declaredSettings reads the settings an entry type declares from an
// entry's config, so the host shows a package's safe fields without asking
// the package (and offline).
func declaredSettings(decls []protocol.SettingDecl) func(json.RawMessage) []extension.Setting {
	if len(decls) == 0 {
		return nil
	}
	return func(raw json.RawMessage) []extension.Setting {
		var fields map[string]json.RawMessage
		if json.Unmarshal(raw, &fields) != nil {
			return nil
		}
		var out []extension.Setting
		for _, d := range decls {
			if v, ok := d.Value(fields); ok {
				out = append(out, extension.Setting{Name: d.Field, Value: v})
			}
		}
		return out
	}
}

// describe says what a config object of an extension or route type sets
// up: the lines the package gave when it validated that object
// (protocol.ValidateResult.Describe), else the declared ones. It never
// calls the package.
func (x *installed) describe(kind, typ string, declared func(json.RawMessage) []config.Entry) func(json.RawMessage) []config.Entry {
	return func(raw json.RawMessage) []config.Entry {
		r := x.r
		if r == nil && x.offline != nil && x.offline.started {
			r = x.offline.r
		}
		if r != nil {
			if lines, ok := r.Described(kind, typ, raw); ok && len(lines) > 0 {
				out := make([]config.Entry, len(lines))
				for i, l := range lines {
					out[i] = config.Entry{Name: l.Name, Detail: l.Detail}
				}
				return out
			}
		}
		if declared == nil {
			return nil
		}
		return declared(raw)
	}
}

// workKinds are the kinds of child Work a route config of handler binds:
// what the package said when it validated that config
// (protocol.ValidateResult.WorkKinds), else none. It never calls the
// package; validation does, before a binding's work_kinds are checked.
func (x *installed) workKinds(handler string) func(json.RawMessage) []extension.WorkKind {
	return func(raw json.RawMessage) []extension.WorkKind {
		r := x.r
		if r == nil && x.offline != nil && x.offline.started {
			r = x.offline.r
		}
		if r == nil {
			return nil
		}
		kinds, _ := r.WorkKinds(handler, raw)
		out := make([]extension.WorkKind, 0, len(kinds))
		for _, k := range kinds {
			out = append(out, extension.WorkKind{Kind: k.Kind, Description: k.Description})
		}
		return out
	}
}

// bindings are the binding names an entry config of extension type typ
// refers to: what the package said when it validated that config
// (protocol.ValidateResult.Bindings), else none. It never calls the
// package; validation does, before the names are checked.
func (x *installed) bindings(typ string) func(json.RawMessage) []string {
	return func(raw json.RawMessage) []string {
		r := x.r
		if r == nil && x.offline != nil && x.offline.started {
			r = x.offline.r
		}
		if r == nil {
			return nil
		}
		names, _ := r.Bindings(typ, raw)
		return names
	}
}

// declaredDescribe reads the lines an entry type's describe declaration
// yields from an entry's config.
func declaredDescribe(d *protocol.DescribeDecl) func(json.RawMessage) []config.Entry {
	if d == nil {
		return nil
	}
	return func(raw json.RawMessage) []config.Entry {
		var fields map[string]json.RawMessage
		if json.Unmarshal(raw, &fields) != nil {
			return nil
		}
		var out []config.Entry
		for _, e := range d.Entries(fields) {
			out = append(out, config.Entry{Name: e.Name, Detail: e.Detail})
		}
		return out
	}
}
