package bootstrap

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/extension/inproc"
	"github.com/open-conduit/open-conduit/internal/config"
	"github.com/open-conduit/open-conduit/internal/extproc"
	"github.com/open-conduit/open-conduit/observability"
	"github.com/open-conduit/open-conduit/protocol"
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
// Offline (config validation) r is nil: the declared types accept any
// config and nothing runs.
type installed struct {
	m protocol.Manifest
	r *extproc.Remote
	d *daemon
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
)

func (x *installed) Name() string { return x.m.Name }

func (x *installed) validator(kind, typ string) func(json.RawMessage, []string) error {
	if x.r == nil {
		return nil
	}
	return x.r.Validator(kind, typ)
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
		out = append(out, inproc.ConfigType{Kind: protocol.ValidateExtension, Type: e.Type, Ingress: e.Ingress, Validate: x.validator(protocol.ValidateExtension, e.Type)})
	}
	for _, r := range p.Routes {
		if r.Config {
			out = append(out, inproc.ConfigType{Kind: protocol.ValidateRoute, Type: r.Handler, Validate: x.validator(protocol.ValidateRoute, r.Handler)})
		}
	}
	return out
}

// resourceTypes adds the manifest's resource types to a registry (resource
// providers are not on the contract yet).
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

// Checks are the declared checks. Each result is named "<package> check
// <name>", its detail the line the extensions doctor check lists for it.
func (x *installed) Checks() []inproc.Check {
	var out []inproc.Check
	for _, c := range x.m.Provides.Checks {
		name, deep := c.Name, c.Deep
		out = append(out, inproc.Check{Deep: deep, Run: func(ctx context.Context) inproc.CheckResult {
			res := inproc.CheckResult{Name: x.m.Name + " check " + name, Status: inproc.CheckOK, Code: "ok"}
			cx, cancel := context.WithTimeout(ctx, 20*time.Second)
			r, err := x.r.Check(cx, name, deep)
			cancel()
			switch {
			case err != nil:
				res.Status, res.Code, res.Detail = inproc.CheckWarn, "extension_check_failed", "unavailable ("+errorCode(err)+")"
			case !r.OK:
				res.Status, res.Code, res.Detail = inproc.CheckWarn, "extension_check_failed", r.Detail
			default:
				res.Detail = "ok " + r.Detail
			}
			return res
		}})
	}
	return out
}

// Exporter forwards telemetry when the package exports it, and drops it
// otherwise.
func (x *installed) Exporter() observability.Exporter {
	if e, ok := x.r.Exporter(); ok {
		return e
	}
	return discardExporter{}
}

type discardExporter struct{}

func (discardExporter) Export(context.Context, []observability.Record) error { return nil }
func (discardExporter) Shutdown(context.Context) error                       { return nil }
