package bootstrap

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/extension/inproc"
	bb "github.com/open-conduit/open-conduit/extensions/bitbucket"
	"github.com/open-conduit/open-conduit/extensions/claudecode"
	gh "github.com/open-conduit/open-conduit/extensions/github"
	"github.com/open-conduit/open-conduit/extensions/introspection"
	"github.com/open-conduit/open-conduit/extensions/jira"
	"github.com/open-conduit/open-conduit/extensions/linear"
	"github.com/open-conduit/open-conduit/extensions/observe"
	"github.com/open-conduit/open-conduit/extensions/schedule"
	"github.com/open-conduit/open-conduit/extensions/shell"
	sl "github.com/open-conduit/open-conduit/extensions/slack"
	"github.com/open-conduit/open-conduit/internal/config"
	"github.com/open-conduit/open-conduit/internal/extproc"
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
	return []inproc.Extension{shell.NewExtension(), claudecode.NewExtension(), gh.New(), introspection.New(), observe.New(), schedule.New(), linear.New(), jira.New(), bb.New(), sl.New()}
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
	if reg.HandlerBinding == nil {
		reg.HandlerBinding = map[string]func(extension.RouteBinding) error{}
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
					t := config.ExtensionType{Ingress: c.Ingress, Validate: v, Settings: c.Settings}
					if describe := c.Describe; describe != nil {
						t.Describe = func(raw json.RawMessage) []config.Entry {
							var out []config.Entry
							for _, e := range describe(raw) {
								out = append(out, config.Entry{Name: e.Name, Detail: e.Detail})
							}
							return out
						}
					}
					reg.Extensions[c.Type] = t
				case protocol.ValidateRoute:
					reg.HandlerConfig[c.Type] = func(raw json.RawMessage) error { return v(raw, nil) }
					if c.Binding != nil {
						reg.HandlerBinding[c.Type] = c.Binding
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
			Artifacts: d.artifacts, ToolbeltDir: d.o.ToolbeltDir, PlaybookDir: d.pluginDir(),
			Render: func(ctx context.Context, e domain.Event) (string, bool) { return d.inprocRender(e) },
			OpenLog: func(name string) (io.WriteCloser, error) {
				return openLog(filepath.Join(d.o.StateDir, "logs", name+".jsonl"))
			},
			AcceptInTx: func(tx storage.Tx, scope domain.ID, e domain.Event) (domain.AcceptedEvent, bool, error) {
				if !own.family(e.Type) {
					return domain.AcceptedEvent{}, false, inproc.ErrNotOwn
				}
				return runtime.AcceptInTx(tx, scope, e)
			},
			AcceptDerivedInTx: func(tx storage.Tx, scope, parent domain.ID, e domain.Event) (domain.AcceptedEvent, bool, error) {
				if !own.family(e.Type) {
					return domain.AcceptedEvent{}, false, inproc.ErrNotOwn
				}
				p, err := tx.Event(parent)
				if err != nil {
					return domain.AcceptedEvent{}, false, err
				}
				if ps, err := runtime.EventScopeInTx(tx, p); err != nil || ps != scope {
					return domain.AcceptedEvent{}, false, extension.ErrUnauthorized
				}
				a, added, err := runtime.AcceptDerivedInTx(tx, d.now(), d.causalLimits(), scope, parent, e)
				if errors.Is(err, runtime.ErrCausalRejected) {
					err = fmt.Errorf("%w: %w", inproc.ErrCausalLimit, err)
				}
				return a, added, err
			},
			EventScope: runtime.EventScopeInTx,
			Deliver: func(ctx context.Context, source string, delivery extension.Delivery) error {
				if source != name && !strings.HasPrefix(source, name+"/") {
					return inproc.ErrNotOwn
				}
				if _, err := d.ingress.Push(ctx, source, delivery); err != nil {
					return err
				}
				d.poke()
				return nil
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
			SettleHold: func(ctx context.Context, work domain.ID, admit bool, reason, key string) (domain.ID, error) {
				return d.current.Load().control.SettleGateHold(ctx, name, work, admit, reason, key)
			},
			Authorize: d.authorize,
			WaitTarget: func(ctx context.Context, scope domain.ID, kind, name string) (domain.CorrelationKey, []string, bool) {
				return d.waitTarget(kind)(ctx, scope, name)
			},
			Act:     d.act(name),
			Actions: d.listActions(name),
			Config:  d.configHost(),
			Probes:  d.setupProbes,
			Capture: d.telemetry.Capture,
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

// inprocExecutors adds the extensions' executors to executors, and the
// capacity pool of each that names one to pools. An executor naming a pool
// no setting bounds is not registered, so it can never run unbounded; its
// name is returned for the host to report.
func inprocExecutors(xs []inproc.Extension, executors map[string]extension.Executor, pools map[string]string) (refused []string) {
	for _, x := range xs {
		e, ok := x.(inproc.Executing)
		if !ok {
			continue
		}
		for name, reg := range e.Executors() {
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
// Installed packages' manifests are held to the same rule.
func knownPool(pool string) bool { return pool == "" || pool == inproc.PoolJobs }

// causalLimits are the current generation's causal bounds.
func (d *daemon) causalLimits() runtime.CausalLimits {
	if s, _ := d.extensionSnapshot(); s != nil {
		return runtime.CausalLimits{MaxDepth: s.Settings.CausalMaxDepth, PerMinute: s.Settings.CausalPerMinute}
	}
	return runtime.CausalLimits{}
}

// workKinds are the scheduler's Work kind policies: the in-process
// extensions' own kinds.
func workKinds(xs []inproc.Extension) map[string]runtime.WorkKindPolicy {
	kinds := map[string]runtime.WorkKindPolicy{}
	for _, x := range xs {
		if k, ok := x.(inproc.WorkKinder); ok {
			for _, w := range k.WorkKinds() {
				kinds[w.Kind] = runtime.WorkKindPolicy{Standing: w.Standing, RunOnce: w.RunOnce, Trigger: w.Trigger}
			}
		}
	}
	return kinds
}

// timedKinds are the Work kinds that run on a clock, which a dry run never
// starts: the in-process extensions' and the installed packages'.
func (d *daemon) timedKinds() map[string]bool {
	out := map[string]bool{}
	for _, x := range d.inproc {
		if k, ok := x.(inproc.WorkKinder); ok {
			for _, w := range k.WorkKinds() {
				if w.Timed {
					out[w.Kind] = true
				}
			}
		}
	}
	return out
}

// recurrences are the timer recurrences: the in-process extensions' own
// kinds.
func recurrences(xs []inproc.Extension) map[string]runtime.RecurrenceFunc {
	r := map[string]runtime.RecurrenceFunc{}
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
// its own types, then registers the ingress sources it declares.
func (d *daemon) configureInproc(ctx context.Context) {
	for _, x := range d.inproc {
		if c, ok := x.(inproc.Configurer); ok {
			entries, connections := typeSets(x)
			c.Configure(ctx, d.configureParams(entries, connections))
		}
		if i, ok := x.(inproc.Ingester); ok {
			d.registerInprocSources(x, i.Sources())
		}
	}
}

// registerInprocSources registers an extension's ingress sources: only
// under its own registration names, and only producing its own Event
// families, as the wire contract requires of an installed package. A
// source's namespace is the operator's to choose (a schedule entry's
// `namespace`); the ingress refuses one another source holds. A
// registration is immutable for one runtime instance: a source registered
// earlier keeps its registration until restart.
func (d *daemon) registerInprocSources(x inproc.Extension, sources []inproc.Source) {
	if d.ingress == nil {
		return
	}
	name := x.Name()
	own := ownership{x: x}
	for _, s := range sources {
		if !protocol.SourceNamespace(name, s.Name) || s.Ingress == nil || !ownFamilies(own, s.Ingress.Spec()) {
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

// ownFamilies reports whether an ingress produces only the extension's own
// Event families.
func ownFamilies(own ownership, spec extension.IngressSpec) bool {
	if len(spec.Families) == 0 {
		return false
	}
	for _, f := range spec.Families {
		if !own.family(f.Type) {
			return false
		}
	}
	return true
}

// inprocWebhook returns the handler an in-process extension gives a webhook
// path under /<name>/, or nil.
func (d *daemon) inprocWebhook(path string) http.Handler {
	for _, x := range d.inproc {
		w, ok := x.(inproc.Webhooker)
		if !ok {
			continue
		}
		if rest, ok := strings.CutPrefix(path, x.Name()+"/"); ok {
			return w.Webhook(rest)
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
// the extensions that resolve that kind.
func (d *daemon) waitTarget(kind string) func(context.Context, domain.ID, string) (domain.CorrelationKey, []string, bool) {
	return func(ctx context.Context, scope domain.ID, name string) (domain.CorrelationKey, []string, bool) {
		for _, x := range d.inproc {
			t, ok := x.(inproc.Targeter)
			if !ok {
				continue
			}
			if f := t.Targets()[kind]; f != nil {
				if key, types, ok := f(ctx, scope, name); ok {
					return key, types, true
				}
			}
		}
		return domain.CorrelationKey{}, nil, false
	}
}

// registerInprocTools registers each extension's toolbelt registration; one
// with neither operations, event families nor a gate adds nothing and is skipped,
// as is an entry-gated one no enabled scope of the generation has an entry
// for.
func (d *daemon) registerInprocTools(belt *runtime.Toolbelt, s *config.Snapshot) error {
	for _, x := range d.inproc {
		if t, ok := x.(inproc.Tooled); ok {
			if entryGated(x) && !hasEntry(s, x) {
				continue
			}
			reg := t.Registration()
			if d, ok := x.(inproc.Declarer); ok {
				reg = inproc.Describe(reg, d.Declarations())
			}
			if len(reg.Operations) == 0 && len(reg.EventTypes) == 0 && reg.Gate == nil && reg.StartGate == nil {
				continue
			}
			if err := belt.Register(reg); err != nil {
				return fmt.Errorf("extension %s toolbelt registration: %w", x.Name(), err)
			}
		}
	}
	return nil
}

// entryGated says whether an extension's operations join only a generation
// with one of its entries: an in-process inproc.EntryGated, or an installed
// package that declares provides.entry_gated.
func entryGated(x inproc.Extension) bool {
	if _, ok := x.(inproc.EntryGated); ok {
		return true
	}
	p, ok := x.(*installed)
	return ok && p.entryGated()
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

// inprocChecks are the extensions' doctor checks, ordinary or deep,
// installed packages' included: each is listed under its own name, and runs
// on the extension's view of the current configuration (checkConfig). The
// extensions check keeps only the packages' own health.
func (d *daemon) inprocChecks(deep bool) []func(context.Context) runtime.Check {
	var out []func(context.Context) runtime.Check
	for _, x := range d.inproc {
		c, ok := x.(inproc.Checker)
		if !ok {
			continue
		}
		for _, check := range c.Checks() {
			if check.Deep != deep {
				continue
			}
			run := check.Run
			out = append(out, func(ctx context.Context) runtime.Check {
				s, _ := d.configs.Current()
				return runtimeCheck(run(ctx, checkConfig(s, x, d.executors, filepath.Dir(d.o.ConfigPath))))
			})
		}
	}
	return out
}

// offlineChecks are the first-party extensions' Offline checks on a config
// validated without a daemon; dir is the config file's directory.
func offlineChecks(ctx context.Context, s *config.Snapshot, executors map[string]extension.Executor, dir string) []runtime.Check {
	var out []runtime.Check
	for _, x := range firstParty() {
		c, ok := x.(inproc.Checker)
		if !ok {
			continue
		}
		for _, check := range c.Checks() {
			if check.Offline && !check.Deep {
				out = append(out, runtimeCheck(check.Run(ctx, checkConfig(s, x, executors, dir))))
			}
		}
	}
	return out
}

func runtimeCheck(r inproc.CheckResult) runtime.Check {
	return runtime.Check{Name: r.Name, Status: r.Status, Code: r.Code, Detail: r.Detail, Items: r.Items}
}

// checkConfig is an extension's read-only view of a configuration for its
// checks (protocol.CheckConfig): each enabled scope that uses it, with the
// scope's routes and bindings, its own entries, and the connections of its
// own types they name. Secret reference text never enters it.
func checkConfig(s *config.Snapshot, x inproc.Extension, executors map[string]extension.Executor, dir string) protocol.CheckConfig {
	cfg := protocol.CheckConfig{Dir: dir}
	if s == nil {
		return cfg
	}
	entries, connections := typeSets(x)
	handlers, operations := map[string]bool{}, map[string]bool{}
	if r, ok := x.(inproc.Router); ok {
		for name := range r.Routes() {
			handlers[name] = true
		}
	}
	if o, ok := x.(inproc.Operator); ok {
		for _, op := range o.Operations() {
			operations[op] = true
		}
	}
	uses := func(scope *config.EffectiveScope) bool {
		if pkg, ok := x.(*installed); ok && usesExtension(scope, pkg.m) {
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
			if slices.ContainsFunc(b.Operations, func(op string) bool { return operations[op] }) {
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
			if b.Resource != nil {
				cb.Resource = &protocol.CheckResource{Provider: b.Resource.Provider, Config: b.Resource.Config}
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

// inprocPlaybooks adds the extensions' playbooks, first-party then
// installed. An installed package's playbook never replaces one already
// there, and its `{{bin}}` is how a turn runs Open Conduit.
func (d *daemon) inprocPlaybooks(into map[string]string) {
	for _, x := range d.inproc {
		p, ok := x.(inproc.Playbooker)
		if !ok {
			continue
		}
		_, pkg := x.(*installed)
		for name, text := range p.Playbooks() {
			if !pkg {
				into[name] = text
				continue
			}
			if _, taken := into[name]; taken {
				d.log.Warn("extension playbook not written: the name is taken", "extension", x.Name(), "playbook", name)
				continue
			}
			into[name] = extproc.RenderPlaybook(text, "open-conduit")
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
	// sinks are the extensions that take the events (installed packages
	// with the telemetry.sink permission among them), read per event since
	// packages join the list after the log opens.
	sinks func() []inproc.Sink
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

func (d *daemon) sinks() []inproc.Sink {
	var out []inproc.Sink
	for _, x := range d.inproc {
		if s, ok := x.(inproc.Sink); ok {
			out = append(out, s)
		}
	}
	return out
}
