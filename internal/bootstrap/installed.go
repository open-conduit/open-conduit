package bootstrap

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/extension/inproc"
	"github.com/open-conduit/open-conduit/internal/config"
	"github.com/open-conduit/open-conduit/internal/extproc"
	"github.com/open-conduit/open-conduit/observability"
	"github.com/open-conduit/open-conduit/protocol"
	"github.com/open-conduit/open-conduit/storage"
)

// installed adapts an installed package (extproc.Remote) onto the
// in-process contract (issue #69), so bootstrap composes first-party and
// installed extensions through one list. Every surface reads the manifest
// and goes through the existing Remote methods.
//
// Executors, resource providers, work kinds, recurrences, ingress sources
// and webhook paths are not on the contract yet: no first-party extension
// has them, so they join it with the extensions that need them, and stay on
// the package's own path in extensions.go until then.
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
}

var (
	_ inproc.Typed      = (*installed)(nil)
	_ inproc.Operator   = (*installed)(nil)
	_ inproc.Router     = (*installed)(nil)
	_ inproc.Configurer = (*installed)(nil)
	_ inproc.Stopper    = (*installed)(nil)
	_ inproc.Tooled     = (*installed)(nil)
	_ inproc.Actor      = (*installed)(nil)
	_ inproc.Annotator  = (*installed)(nil)
	_ inproc.Renderer   = (*installed)(nil)
	_ inproc.Tasker     = (*installed)(nil)
	_ inproc.Checker    = (*installed)(nil)
	_ inproc.Exporting  = (*installed)(nil)
	_ inproc.Sink       = (*installed)(nil)
	_ inproc.Paged      = (*installed)(nil)
	_ inproc.Playbooker = (*installed)(nil)

	_ extension.ConfigCandidate = (*installed)(nil)
)

func (x *installed) Name() string { return x.m.Name }

// Title is the manifest's display name (inproc.Titled).
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
func (x *installed) Types() []inproc.ConfigType {
	p := x.m.Provides
	var out []inproc.ConfigType
	for _, c := range p.Connections {
		out = append(out, inproc.ConfigType{Kind: protocol.ValidateConnection, Type: c.Type, Validate: x.validator(protocol.ValidateConnection, c.Type)})
	}
	for _, e := range p.Extensions {
		out = append(out, inproc.ConfigType{Kind: protocol.ValidateExtension, Type: e.Type, Ingress: e.Ingress, Validate: x.validator(protocol.ValidateExtension, e.Type),
			Settings: declaredSettings(e.Settings), Describe: declaredDescribe(e.Describe)})
	}
	for _, r := range p.Routes {
		if r.Config || r.Binding {
			t := inproc.ConfigType{Kind: protocol.ValidateRoute, Type: r.Handler}
			if r.Config {
				t.Validate = x.validator(protocol.ValidateRoute, r.Handler)
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
// with one of its entries (provides.entry_gated, inproc.EntryGated).
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

// resourceTypes adds the manifest's resource types to a registry, with the
// workspace checks a type declares (mode_default, sandbox) answered through
// the package as git answers them in process (resource providers are not
// on the in-process contract yet).
func (x *installed) resourceTypes(reg *config.Registry) {
	if reg.Resources == nil {
		reg.Resources = map[string]bool{}
	}
	if reg.ResourceConfig == nil {
		reg.ResourceConfig = map[string]func(json.RawMessage) error{}
	}
	for _, r := range x.m.Provides.Resources {
		reg.Resources[r.Type] = true
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
func (x *installed) Configure(ctx context.Context, _ protocol.ConfigureParams) {
	cx, cancel := context.WithTimeout(ctx, 30*time.Second)
	err := x.r.Process().Reconfigure(cx)
	cancel()
	if err != nil {
		x.d.report("extension_configure", domain.ID(x.m.Name), errorCode(err))
		return
	}
	x.d.clear("extension_configure", domain.ID(x.m.Name))
}

// Stop shuts the package down (shutdown, stdin closed, then signals to its
// process group). Executor jobs it detached keep running and are adopted by
// the next daemon through executor.recover.
func (x *installed) Stop() { _ = x.r.Process().Close() }

// Registration is the package's toolbelt registration.
func (x *installed) Registration() extension.Registration { return x.r.Registration() }

// Actions offers the package's action provider, keyed by its name, on a
// scope whose current configuration uses it.
func (x *installed) Actions(scope domain.ID) map[string]extension.ActionProvider {
	s, _ := x.d.extensionSnapshot()
	if s == nil || s.Scopes[scope] == nil || !s.Scopes[scope].Enabled {
		return nil
	}
	p, ok := x.r.Actions()
	if !ok || !usesExtension(s.Scopes[scope], x.m) {
		return nil
	}
	return map[string]extension.ActionProvider{x.m.Name: p}
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

func (x *installed) Annotate(ctx context.Context, w domain.Work) []inproc.Annotation {
	var out []inproc.Annotation
	for _, a := range x.r.Annotate(ctx, w) {
		out = append(out, inproc.Annotation{Source: x.m.Name, Name: a.Name, Value: a.Value})
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
func (x *installed) Tasks() []inproc.Task {
	var out []inproc.Task
	for _, t := range x.m.Provides.Tasks {
		name := t.Name
		out = append(out, inproc.Task{Name: name, Pace: time.Duration(t.IntervalSeconds) * time.Second, Run: func(ctx context.Context) error { return x.r.Task(ctx, name) }})
	}
	return out
}

// Checks are the declared checks, each listed in doctor under its own
// name (checkName). check.run carries the package's view of the
// configuration; a result without a status is ok or, when not OK, a warning.
func (x *installed) Checks() []inproc.Check {
	var out []inproc.Check
	for _, c := range x.m.Provides.Checks {
		name, deep := c.Name, c.Deep
		out = append(out, inproc.Check{Deep: deep, Run: func(ctx context.Context, cfg protocol.CheckConfig) inproc.CheckResult {
			res := inproc.CheckResult{Name: checkName(x.m.Name, name), Status: inproc.CheckOK, Code: "ok"}
			cx, cancel := context.WithTimeout(ctx, 20*time.Second)
			r, err := x.r.Check(cx, name, deep, cfg)
			cancel()
			switch {
			case err != nil:
				res.Status, res.Code, res.Detail = inproc.CheckWarn, "extension_check_failed", "unavailable ("+errorCode(err)+")"
			case r.Status != "":
				res.Status, res.Code, res.Detail, res.Items = r.Status, r.Code, r.Detail, r.Items
				switch r.Status {
				case inproc.CheckOK, inproc.CheckInfo, inproc.CheckWarn, inproc.CheckFail:
				default:
					res.Status, res.Code, res.Detail, res.Items = inproc.CheckWarn, "extension_check_invalid", "unknown status "+strconv.Quote(r.Status), nil
				}
				if res.Code == "" {
					res.Code = res.Status
				}
			case !r.OK:
				res.Status, res.Code, res.Detail = inproc.CheckWarn, "extension_check_failed", r.Detail
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
		var config map[string]json.RawMessage
		if json.Unmarshal(raw, &config) != nil {
			return nil
		}
		var out []extension.Setting
		for _, d := range decls {
			if v, ok := d.Value(config); ok {
				out = append(out, extension.Setting{Name: d.Field, Value: v})
			}
		}
		return out
	}
}

// declaredDescribe reads the lines an entry type's describe declaration
// yields from an entry's config.
func declaredDescribe(d *protocol.DescribeDecl) func(json.RawMessage) []inproc.Entry {
	if d == nil {
		return nil
	}
	return func(raw json.RawMessage) []inproc.Entry {
		var config map[string]json.RawMessage
		if json.Unmarshal(raw, &config) != nil {
			return nil
		}
		var out []inproc.Entry
		for _, e := range d.Entries(config) {
			out = append(out, inproc.Entry{Name: e.Name, Detail: e.Detail})
		}
		return out
	}
}
