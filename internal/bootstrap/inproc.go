package bootstrap

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/extension/inproc"
	"github.com/open-conduit/open-conduit/extensions/human"
	"github.com/open-conduit/open-conduit/extensions/introspection"
	"github.com/open-conduit/open-conduit/extensions/posthog"
	"github.com/open-conduit/open-conduit/internal/config"
	"github.com/open-conduit/open-conduit/internal/management"
	"github.com/open-conduit/open-conduit/internal/runtime"
	"github.com/open-conduit/open-conduit/observability"
	"github.com/open-conduit/open-conduit/protocol"
	"github.com/open-conduit/open-conduit/storage"
)

// In-process extensions (issue #66). Each surface is composed by iterating
// the list below through the public inproc contract; nothing here names an
// extension except to build the list. Installed packages join the same list
// once started (installed.go, issue #69). Extensions not yet on the contract
// are still wired by their own files.

// firstParty is the list of first-party in-process extensions. Adding one
// touches its own package and this list.
func firstParty() []inproc.Extension {
	return []inproc.Extension{posthog.New(), human.New(), introspection.New()}
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
// for an installed package, and acceptance, timers and cancellation to its
// own Event families, recurrence kinds and Work kinds.
func (d *daemon) startInproc(ctx context.Context) error {
	for _, x := range d.inproc {
		s, ok := x.(inproc.Starter)
		if !ok {
			continue
		}
		name := x.Name()
		entries, connections := typeSets(x)
		own := ownership{x: x}
		h := inproc.Host{Store: d.coord, Now: d.now, Log: d.log.With("extension", name),
			Secrets:  d.scopedSecrets(entries, connections),
			Report:   func(code string) { d.report(name, "", code) },
			Nudge:    d.poke,
			StateDir: d.o.StateDir, DryRun: d.o.DryRun,
			AcceptInTx: func(tx storage.Tx, scope domain.ID, e domain.Event) (domain.AcceptedEvent, bool, error) {
				if !own.family(e.Type) {
					return domain.AcceptedEvent{}, false, inproc.ErrNotOwn
				}
				return runtime.AcceptInTx(tx, scope, e)
			},
			Timers: inprocTimers{d: d, own: own},
			CancelWork: func(ctx context.Context, work domain.ID, expected domain.Revision, reason, key string) (domain.ID, error) {
				var w domain.Work
				if err := d.coord.Transact(ctx, storage.Guard{Now: d.now()}, func(tx storage.Tx) error {
					var err error
					w, err = tx.Work(work)
					return err
				}); err != nil {
					return "", err
				}
				if !own.workKind(w.Kind) {
					return "", inproc.ErrNotOwn
				}
				id, err := d.current.Load().control.CancelWork(ctx, work, expected, reason, name+"/"+key)
				if errors.Is(err, runtime.ErrRevision) {
					err = fmt.Errorf("%w: %w", storage.ErrConflict, err)
				}
				return id, err
			},
			WaitProgress: d.waitProgress(name),
			Capture:      d.telemetry.Capture,
		}
		if err := s.Start(ctx, h); err != nil {
			return fmt.Errorf("extension %s: %w", name, err)
		}
	}
	return nil
}

// ownership answers what an extension declared as its own.
type ownership struct{ x inproc.Extension }

// family: an extension's Event families are the types named after it.
func (o ownership) family(eventType string) bool {
	return strings.HasPrefix(eventType, o.x.Name()+".")
}

func (o ownership) recurrence(kind string) bool {
	r, ok := o.x.(inproc.Recurring)
	if !ok {
		return false
	}
	_, own := r.Recurrences()[kind]
	return own
}

func (o ownership) workKind(kind string) bool {
	k, ok := o.x.(inproc.WorkKinder)
	if !ok {
		return false
	}
	for _, w := range k.WorkKinds() {
		if w.Kind == kind {
			return true
		}
	}
	return false
}

// errNoGeneration refuses a timer call before the first generation is
// built; the extension's next pass retries.
var errNoGeneration = errors.New("no active generation")

// inprocTimers arms an extension's own recurrences through the current
// generation's timers.
type inprocTimers struct {
	d   *daemon
	own ownership
}

func (t inprocTimers) ArmRecurrence(ctx context.Context, scope domain.ID, timer domain.Timer, expected domain.Revision, kind string, spec []byte) error {
	if !t.own.recurrence(kind) || !t.own.family(timer.Event.Type) {
		return inproc.ErrNotOwn
	}
	c := t.d.current.Load()
	if c == nil {
		return errNoGeneration
	}
	return c.timers.ArmRecurrence(ctx, scope, timer, expected, kind, spec)
}

func (t inprocTimers) Cancel(ctx context.Context, id domain.ID, expected domain.Revision) error {
	own := true
	if err := t.d.coord.Transact(ctx, storage.Guard{Now: t.d.now()}, func(tx storage.Tx) error {
		timer, err := tx.Timer(id)
		if errors.Is(err, storage.ErrNotFound) {
			return nil // cancelling an absent timer is a no-op
		}
		own = err == nil && t.own.family(timer.Event.Type)
		return err
	}); err != nil {
		return err
	}
	if !own {
		return inproc.ErrNotOwn
	}
	c := t.d.current.Load()
	if c == nil {
		return errNoGeneration
	}
	return c.timers.Cancel(ctx, id, expected)
}

// workKinds are the scheduler's Work kind policies: scheduled jobs and the
// in-process extensions' own kinds.
func workKinds(xs []inproc.Extension) map[string]runtime.WorkKindPolicy {
	kinds := scheduleWorkKinds()
	for _, x := range xs {
		if k, ok := x.(inproc.WorkKinder); ok {
			for _, w := range k.WorkKinds() {
				kinds[w.Kind] = runtime.WorkKindPolicy{Standing: w.Standing, Trigger: w.Trigger}
			}
		}
	}
	return kinds
}

// recurrences are the timer recurrences: scheduled jobs and the in-process
// extensions' own kinds.
func recurrences(xs []inproc.Extension) map[string]runtime.RecurrenceFunc {
	r := scheduleRecurrences()
	for _, x := range xs {
		if rc, ok := x.(inproc.Recurring); ok {
			for kind, f := range rc.Recurrences() {
				r[kind] = runtime.RecurrenceFunc(f)
			}
		}
	}
	return r
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
// with neither operations nor event families adds nothing and is skipped,
// as is an entry-gated one no enabled scope of the generation has an entry
// for.
func (d *daemon) registerInprocTools(belt *runtime.Toolbelt, s *config.Snapshot) error {
	for _, x := range d.inproc {
		if t, ok := x.(inproc.Tooled); ok {
			if _, gated := x.(inproc.EntryGated); gated && !hasEntry(s, x) {
				continue
			}
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

// hasEntry reports whether an enabled scope of the generation has an entry
// of one of the extension's types.
func hasEntry(s *config.Snapshot, x inproc.Extension) bool {
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
