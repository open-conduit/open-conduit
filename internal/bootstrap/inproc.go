package bootstrap

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/extension/inproc"
	"github.com/open-conduit/open-conduit/extensions/human"
	"github.com/open-conduit/open-conduit/extensions/posthog"
	"github.com/open-conduit/open-conduit/internal/config"
	"github.com/open-conduit/open-conduit/internal/management"
	"github.com/open-conduit/open-conduit/internal/runtime"
	"github.com/open-conduit/open-conduit/observability"
	"github.com/open-conduit/open-conduit/protocol"
)

// In-process extensions (issue #66). Each surface is composed by iterating
// the list below through the public inproc contract; nothing here names an
// extension except to build the list. Installed packages join the same list
// once started (installed.go, issue #69). Extensions not yet on the contract
// are still wired by their own files.

// firstParty is the list of first-party in-process extensions. Adding one
// touches its own package and this list.
func firstParty() []inproc.Extension {
	return []inproc.Extension{posthog.New(), human.New()}
}

// typeSets are an extension's own entry and connection types: what its
// configure carries and which secrets it may resolve.
func typeSets(x inproc.Extension) (entries, connections map[string]bool) {
	entries, connections = map[string]bool{}, map[string]bool{}
	if t, ok := x.(inproc.Typed); ok {
		for _, c := range t.Types() {
			switch c.Kind {
			case protocol.ValidateExtension:
				entries[c.Type] = true
			case protocol.ValidateConnection:
				connections[c.Type] = true
			}
		}
	}
	return entries, connections
}

// inprocRegistry adds the extensions' config types, operations and route
// handler names to a registry. These surfaces need no host services, so
// offline validation uses them too.
func inprocRegistry(reg *config.Registry, xs []inproc.Extension) {
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
	for _, x := range xs {
		if t, ok := x.(inproc.Typed); ok {
			for _, c := range t.Types() {
				v := c.Validate
				if v == nil {
					v = func(json.RawMessage, []string) error { return nil }
				}
				switch c.Kind {
				case protocol.ValidateConnection:
					reg.Connections[c.Type] = func(raw json.RawMessage) error { return v(raw, nil) }
				case protocol.ValidateExtension:
					reg.Extensions[c.Type] = config.ExtensionType{Ingress: c.Ingress, Validate: v}
				case protocol.ValidateRoute:
					reg.HandlerConfig[c.Type] = func(raw json.RawMessage) error { return v(raw, nil) }
				}
			}
		}
		if o, ok := x.(inproc.Operator); ok {
			reg.Operations = append(reg.Operations, o.Operations()...)
		}
		if r, ok := x.(inproc.Router); ok {
			for name := range r.Routes() {
				reg.Handlers[name] = true
			}
		}
	}
}

// inprocHandlers adds the extensions' route handlers for one generation.
func inprocHandlers(into map[string]extension.RouteHandler, xs []inproc.Extension) {
	for _, x := range xs {
		if r, ok := x.(inproc.Router); ok {
			for name, h := range r.Routes() {
				into[name] = h
			}
		}
	}
}

// startInproc starts each extension behind its host services. Secrets are
// scoped to the extension's own entries and connections (B05), exactly as
// for an installed package.
func (d *daemon) startInproc(ctx context.Context) error {
	for _, x := range d.inproc {
		s, ok := x.(inproc.Starter)
		if !ok {
			continue
		}
		name := x.Name()
		entries, connections := typeSets(x)
		h := inproc.Host{Store: d.coord, Now: d.now, Log: d.log.With("extension", name),
			Secrets: d.scopedSecrets(entries, connections),
			Report:  func(code string) { d.report(name, "", code) }}
		if err := s.Start(ctx, h); err != nil {
			return fmt.Errorf("extension %s: %w", name, err)
		}
	}
	return nil
}

func (d *daemon) stopInproc() {
	for _, x := range d.inproc {
		if s, ok := x.(inproc.Stopper); ok {
			s.Stop()
		}
	}
}

// configureInproc hands each extension the current generation's entries of
// its own types.
func (d *daemon) configureInproc(ctx context.Context) {
	for _, x := range d.inproc {
		if c, ok := x.(inproc.Configurer); ok {
			entries, connections := typeSets(x)
			c.Configure(ctx, d.configureParams(entries, connections))
		}
	}
}

// registerInprocTools registers each extension's toolbelt registration; one
// with neither operations nor event families adds nothing and is skipped.
func (d *daemon) registerInprocTools(belt *runtime.Toolbelt) error {
	for _, x := range d.inproc {
		if t, ok := x.(inproc.Tooled); ok {
			reg := t.Registration()
			if len(reg.Operations) == 0 && len(reg.EventTypes) == 0 {
				continue
			}
			if err := belt.Register(reg); err != nil {
				return fmt.Errorf("extension %s toolbelt registration: %w", x.Name(), err)
			}
		}
	}
	return nil
}

func (d *daemon) inprocActions(scope domain.ID, out map[string]extension.ActionProvider) {
	for _, x := range d.inproc {
		if a, ok := x.(inproc.Actor); ok {
			for name, p := range a.Actions(scope) {
				out[name] = p
			}
		}
	}
}

func (d *daemon) inprocAnnotations(ctx context.Context, w domain.Work) []management.Annotation {
	var out []management.Annotation
	for _, x := range d.inproc {
		if a, ok := x.(inproc.Annotator); ok {
			for _, l := range a.Annotate(ctx, w) {
				out = append(out, management.Annotation{Source: l.Source, Name: l.Name, Value: l.Value})
			}
		}
	}
	return out
}

func (d *daemon) inprocRender(e domain.Event) (string, bool) {
	for _, x := range d.inproc {
		if r, ok := x.(inproc.Renderer); ok {
			ctx, cancel := context.WithTimeout(d.lifetime(), 5*time.Second)
			text, ok := r.Render(ctx, e)
			cancel()
			if ok {
				return text, true
			}
		}
	}
	return "", false
}

// runInprocTasks starts each task when its pace has elapsed.
func (d *daemon) runInprocTasks(ctx context.Context) {
	for _, x := range d.inproc {
		if t, ok := x.(inproc.Tasker); ok {
			for _, task := range t.Tasks() {
				d.runTask(ctx, x.Name()+"/"+task.Name, task.Pace, task.Run)
			}
		}
	}
}

// inprocChecks are the extensions' doctor checks, ordinary or deep. An
// installed package's checks are reported inside the extensions check
// instead (extensionCheck), so they are left out here.
func (d *daemon) inprocChecks(deep bool) []func(context.Context) runtime.Check {
	var out []func(context.Context) runtime.Check
	for _, x := range d.inproc {
		c, ok := x.(inproc.Checker)
		if _, pkg := x.(*installed); !ok || pkg {
			continue
		}
		for _, check := range c.Checks() {
			if check.Deep != deep {
				continue
			}
			run := check.Run
			out = append(out, func(ctx context.Context) runtime.Check {
				r := run(ctx)
				return runtime.Check{Name: r.Name, Status: r.Status, Code: r.Code, Detail: r.Detail, Items: r.Items}
			})
		}
	}
	return out
}

func (d *daemon) inprocPlaybooks(into map[string]string) {
	for _, x := range d.inproc {
		if p, ok := x.(inproc.Playbooker); ok {
			for name, text := range p.Playbooks() {
				into[name] = text
			}
		}
	}
}

func (d *daemon) inprocExporters() []observability.Exporter {
	var out []observability.Exporter
	for _, x := range d.inproc {
		if e, ok := x.(inproc.Exporting); ok {
			out = append(out, e.Exporter())
		}
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
	sinks []inproc.Sink
}

func (f telemetryFanout) Capture(ctx context.Context, scope, name string, at time.Time, props map[string]any) error {
	err := f.primary.Capture(ctx, scope, name, at, props)
	for _, s := range f.sinks {
		_ = s.Capture(ctx, scope, name, at, props)
	}
	return err
}

func (d *daemon) inprocSinks() []inproc.Sink {
	var out []inproc.Sink
	for _, x := range d.inproc {
		if s, ok := x.(inproc.Sink); ok {
			out = append(out, s)
		}
	}
	return out
}
