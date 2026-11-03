// Package inprocsdk serves an extension written against the in-process
// contract (extension/inproc) as an installed package over protocol
// version 1, unchanged. It is the proof that the in-process contract is
// fully carried by the wire (#357): a first-party extension and a package
// differ only in how they are run.
//
// The bridge derives the manifest from the extension's surfaces where they
// say enough (config types, routes, Work kinds, recurrences, executors,
// tasks, wait target kinds and the flags of the optional surfaces) and
// takes the rest from a base manifest the package supplies: its version,
// summary and command, Event families and schemas (an ingress declares its
// families only once configured), operation kinds (a toolbelt registration
// exists only once started) and the names of its doctor checks, by position
// (an inproc.Check is named by its result).
//
// The extension is started at the first configure, with an inproc.Host
// whose services are the host's methods: the store is sdk.Store (the
// extension's own KV and scoped state reads), secrets go through
// secret.resolve, deliveries through ingress.deliver, and Authorize,
// WaitTarget, WaitProgress, Render, Act, Actions, SettleHold and Capture
// through their methods. In-transaction acceptance returns the Event from
// the transaction as a kv.commit Emission, which the host accepts with the
// transaction's writes (services.go); timers go through timer.arm and
// timer.cancel, Work cancellation through work.cancel, and, with their
// permissions declared, the config host through config.* and the probes
// through probe.list and probe.run.
//
// Webhook paths are the registered sources the extension has a webhook
// handler for. Action providers, which an in-process extension offers per
// scope by name, are offered as one provider: the actions of every provider
// on the target's scope, an invocation going to the first that offers it.
package inprocsdk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/extension/inproc"
	"github.com/open-conduit/open-conduit/protocol"
	"github.com/open-conduit/open-conduit/protocol/sdk"
)

// Bridge serves one in-process extension.
type Bridge struct {
	x   inproc.Extension
	ext *sdk.Extension
	srv *sdk.Server

	mu      sync.Mutex
	started bool
	dryRun  bool
	sources map[string]inproc.Source
	types   map[string]inproc.ConfigType // by validate kind and type
	checks  []string                     // declared check names, in order

	// startMu serialises starting: the extension counts as started only
	// once Start has succeeded, so a configure after a failed Start tries
	// again rather than configuring an extension that never started.
	startMu sync.Mutex
}

// New bridges x, completing base into its manifest. It fails when base
// lacks what only it can say: an operation kind for each operation x
// registers, a family for each ingress type, a name for each check.
func New(x inproc.Extension, base protocol.Manifest) (*Bridge, error) {
	b := &Bridge{x: x, sources: map[string]inproc.Source{}, types: map[string]inproc.ConfigType{}}
	m, err := b.manifest(base)
	if err != nil {
		return nil, err
	}
	b.ext = &sdk.Extension{Manifest: m}
	b.wire()
	b.wireSurfaces()
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return b, nil
}

// Extension is the served extension (for sdk.PackageCommand and tests).
func (b *Bridge) Extension() *sdk.Extension { return b.ext }

// Serve runs the bridge on the process's stdin and stdout until the host
// closes stdin, as sdk.Serve does.
func (b *Bridge) Serve() error {
	out := os.Stdout
	os.Stdout = os.Stderr
	b.Attach(sdk.NewServer(b.ext, os.Stdin, out))
	<-b.srv.Done()
	b.stop()
	return nil
}

// Attach binds the server serving the bridge (its state directory comes
// from initialize). Serve attaches its own; a test attaches a pipe server.
func (b *Bridge) Attach(s *sdk.Server) {
	b.mu.Lock()
	b.srv = s
	b.mu.Unlock()
}

func (b *Bridge) stop() {
	if s, ok := b.x.(inproc.Stopper); ok {
		b.mu.Lock()
		started := b.started
		b.mu.Unlock()
		if started {
			s.Stop()
		}
	}
}

func (b *Bridge) manifest(base protocol.Manifest) (protocol.Manifest, error) {
	m := base
	name := b.x.Name()
	if m.Name == "" {
		m.Name = name
	}
	if m.Name != name {
		return m, fmt.Errorf("inprocsdk: base manifest names %q, the extension %q", m.Name, name)
	}
	if len(m.Protocol) == 0 {
		m.Protocol = []int{protocol.Version}
	}
	if t, ok := b.x.(inproc.Titled); ok && m.Title == "" {
		m.Title = t.Title()
	}
	p := &m.Provides
	if t, ok := b.x.(inproc.Typed); ok {
		for _, ct := range t.Types() {
			b.types[ct.Kind+"/"+ct.Type] = ct
			switch ct.Kind {
			case protocol.ValidateConnection:
				p.Connections = appendType(p.Connections, ct.Type)
			case protocol.ValidateExtension:
				if !hasExtension(p.Extensions, ct.Type) {
					p.Extensions = append(p.Extensions, protocol.ExtensionDecl{Type: ct.Type, Ingress: ct.Ingress})
				}
			}
		}
	}
	if r, ok := b.x.(inproc.Router); ok {
		routes := r.Routes()
		for _, h := range sortedKeys(routes) {
			if !hasRoute(p.Routes, h) {
				ct, configured := b.types[protocol.ValidateRoute+"/"+h]
				_, samples := routes[h].(extension.RouteSampler)
				p.Routes = append(p.Routes, protocol.RouteDecl{Handler: h, Config: configured && ct.Validate != nil, Binding: configured && ct.Binding != nil, Samples: samples})
			}
		}
	}
	if o, ok := b.x.(inproc.Operator); ok {
		for _, op := range o.Operations() {
			if !slices.ContainsFunc(p.Operations, func(d protocol.OperationDecl) bool { return d.Name == op }) {
				return m, fmt.Errorf("inprocsdk: the base manifest declares no kind for operation %q", op)
			}
		}
	}
	if d, ok := b.x.(inproc.Declarer); ok {
		for _, op := range d.Declarations() {
			i := slices.IndexFunc(p.Operations, func(d protocol.OperationDecl) bool { return d.Name == op.Name })
			if i < 0 {
				continue
			}
			// The extension's own declaration gives what the base leaves
			// unset, as the host does for an in-process registration.
			if p.Operations[i].Summary == "" {
				p.Operations[i].Summary = op.Summary
			}
			if len(p.Operations[i].Input) == 0 {
				p.Operations[i].Input = op.Input
			}
		}
	}
	if t, ok := b.x.(inproc.Toolsetter); ok && len(p.Toolsets) == 0 {
		for _, ts := range t.Toolsets() {
			p.Toolsets = append(p.Toolsets, protocol.ToolsetDecl{Name: ts.Name, Summary: ts.Summary, Operations: slices.Clone(ts.Operations)})
		}
	}
	if _, ok := b.x.(inproc.EntryGated); ok {
		p.EntryGated = true
	}
	if s, ok := b.x.(extension.SetupProbes); ok && len(p.Probes) == 0 {
		for _, pr := range s.SetupProbes() {
			info := protocol.ProbeInfo{Name: pr.Name, Summary: pr.Summary}
			for _, a := range pr.Args {
				info.Args = append(info.Args, protocol.ProbeArg{Name: a.Name, Summary: a.Summary, Required: a.Required})
			}
			p.Probes = append(p.Probes, info)
		}
	}
	if k, ok := b.x.(inproc.WorkKinder); ok && len(p.WorkKinds) == 0 {
		for _, w := range k.WorkKinds() {
			p.WorkKinds = append(p.WorkKinds, protocol.WorkKindDecl{Kind: w.Kind, Standing: w.Standing, Trigger: w.Trigger != nil, Timed: w.Timed, RunOnce: w.RunOnce})
		}
	}
	if r, ok := b.x.(inproc.Recurring); ok && len(p.Recurrences) == 0 {
		p.Recurrences = sortedKeys(r.Recurrences())
	}
	if e, ok := b.x.(inproc.Executing); ok && len(p.Executors) == 0 {
		execs := e.Executors()
		for _, n := range sortedKeys(execs) {
			x := execs[n].Executor
			c := x.Capabilities()
			_, stop := x.(extension.StopExecutor)
			_, forget := x.(extension.ForgetExecutor)
			_, attach := x.(extension.AttachExecutor)
			p.Executors = append(p.Executors, protocol.ExecutorDecl{Name: n, Pool: execs[n].Pool,
				Capabilities: protocol.Capabilities{Interrupt: c.Interrupt, Inject: c.Inject, Resume: c.Resume, Workspace: c.Workspace, Toolbelt: c.Toolbelt, Sandbox: c.Sandbox, Container: c.Container},
				Controls:     protocol.Controls{Stop: stop, Forget: forget, Attach: attach}})
		}
	}
	if t, ok := b.x.(inproc.Tasker); ok && len(p.Tasks) == 0 {
		for _, task := range t.Tasks() {
			p.Tasks = append(p.Tasks, protocol.TaskDecl{Name: taskName(name, task.Name), IntervalSeconds: max(1, int(task.Pace/time.Second))})
		}
	}
	if c, ok := b.x.(inproc.Checker); ok {
		n := len(c.Checks())
		if len(p.Checks) != n {
			return m, fmt.Errorf("inprocsdk: the extension has %d checks and the base manifest names %d", n, len(p.Checks))
		}
		for i := range p.Checks {
			b.checks = append(b.checks, p.Checks[i].Name)
		}
		for i, ch := range c.Checks() {
			p.Checks[i].Deep = p.Checks[i].Deep || ch.Deep
			p.Checks[i].Offline = p.Checks[i].Offline || ch.Offline
		}
	}
	if t, ok := b.x.(inproc.Targeter); ok && len(p.Targets) == 0 {
		p.Targets = sortedKeys(t.Targets())
	}
	_, p.Webhooks = b.x.(inproc.Webhooker)
	_, p.Render = b.x.(inproc.Renderer)
	_, p.Annotate = b.x.(inproc.Annotator)
	_, p.Actions = b.x.(inproc.Actor)
	_, p.RunEnvironment = b.x.(extension.RunEnvironment)
	_, p.SegmentEnd = b.x.(inproc.SegmentEnder)
	_, p.Exporters = b.x.(inproc.Exporting)
	return m, nil
}

// wire sets the served surfaces.
func (b *Bridge) wire() {
	e, name := b.ext, b.x.Name()
	e.Validate = b.validate
	e.Configure = b.configure
	e.Describe = b.describe
	if s, ok := b.x.(extension.SetupProbes); ok {
		e.Probes = s.SetupProbes()
	}
	e.Ingress = func(source string) (extension.Ingress, bool) {
		b.mu.Lock()
		defer b.mu.Unlock()
		s, ok := b.sources[source]
		return s.Ingress, ok && s.Ingress != nil
	}
	if r, ok := b.x.(inproc.Router); ok {
		e.Routes = r.Routes()
	}
	// The registration exists once started; each operation's handler asks
	// it at call time.
	for _, d := range e.Manifest.Provides.Operations {
		e.Operations = append(e.Operations, b.operation(d))
	}
	e.Filter = func(filter domain.Payload, ev domain.Event) bool {
		if f := b.registration().DependencyFilter; f != nil {
			return f(filter, ev)
		}
		return false
	}
	if w, ok := b.x.(inproc.Webhooker); ok {
		e.Webhooks = func(path string) (http.Handler, bool) {
			rest, ok := strings.CutPrefix(path, "/"+name+"/")
			if !ok {
				return nil, false
			}
			h := w.Webhook(rest)
			return h, h != nil
		}
	}
	if x, ok := b.x.(inproc.Executing); ok {
		e.Executors = map[string]extension.Executor{}
		for n, ex := range x.Executors() {
			e.Executors[n] = ex.Executor
		}
	}
	if k, ok := b.x.(inproc.WorkKinder); ok {
		e.Triggers = map[string]func([]domain.Event) int{}
		for _, w := range k.WorkKinds() {
			if w.Trigger != nil {
				e.Triggers[w.Kind] = w.Trigger
			}
		}
	}
	if r, ok := b.x.(inproc.Recurring); ok {
		e.Recurrences = map[string]sdk.RecurrenceFunc{}
		for kind, f := range r.Recurrences() {
			e.Recurrences[kind] = sdk.RecurrenceFunc(f)
		}
	}
	if t, ok := b.x.(inproc.Tasker); ok {
		e.Tasks = map[string]func(context.Context, *sdk.Host) error{}
		for _, task := range t.Tasks() {
			run := task.Run
			e.Tasks[taskName(name, task.Name)] = func(ctx context.Context, _ *sdk.Host) error { return run(ctx) }
		}
	}
	if c, ok := b.x.(inproc.Checker); ok {
		e.CheckRuns = map[string]func(context.Context, *sdk.Host, protocol.CheckRunParams) (protocol.CheckRunResult, error){}
		for i, ch := range c.Checks() {
			run := ch.Run
			e.CheckRuns[b.checks[i]] = func(ctx context.Context, _ *sdk.Host, p protocol.CheckRunParams) (protocol.CheckRunResult, error) {
				r := run(ctx, p.Config)
				return protocol.CheckRunResult{OK: r.Status == inproc.CheckOK || r.Status == inproc.CheckInfo, Status: r.Status, Code: r.Code, Detail: r.Detail, Items: r.Items}, nil
			}
		}
	}
	if r, ok := b.x.(inproc.Renderer); ok {
		e.Render = func(ev domain.Event) (string, bool) { return r.Render(context.Background(), ev) }
	}
	if a, ok := b.x.(inproc.Annotator); ok {
		e.Annotate = func(ctx context.Context, _ *sdk.Host, w domain.Work) ([]protocol.Annotation, error) {
			var out []protocol.Annotation
			for _, an := range a.Annotate(ctx, w) {
				out = append(out, protocol.Annotation{Name: an.Name, Value: an.Value})
			}
			return out, nil
		}
	}
	if a, ok := b.x.(inproc.Actor); ok {
		e.Actions = func(ctx context.Context, _ *sdk.Host, t extension.ActionTarget) ([]extension.Action, error) {
			var out []extension.Action
			seen := map[string]bool{}
			for _, p := range providers(a, t.Scope.ID) {
				actions, err := p.Actions(ctx, t)
				if err != nil {
					return nil, err
				}
				for _, act := range actions {
					if !seen[act.ID] {
						seen[act.ID] = true
						out = append(out, act)
					}
				}
			}
			return out, nil
		}
		// work.act names no provider: the call goes to the one offering
		// the action now, else to the scope's only provider, whose own
		// refusal (an answer to a closed question, say) says why, as the
		// host's call to a named in-process provider would.
		e.Act = func(ctx context.Context, _ *sdk.Host, c extension.ActionCall) (extension.ActionPlan, error) {
			ps := providers(a, c.Target.Scope.ID)
			for _, p := range ps {
				actions, err := p.Actions(ctx, c.Target)
				if err != nil {
					return extension.ActionPlan{}, err
				}
				for _, act := range actions {
					if act.ID == c.Action {
						return p.Invoke(ctx, c)
					}
				}
			}
			if len(ps) == 1 {
				return ps[0].Invoke(ctx, c)
			}
			return extension.ActionPlan{}, &extension.OperationError{Code: "unknown_action", Message: "no such action here"}
		}
	}
	if r, ok := b.x.(extension.RunEnvironment); ok {
		e.RunEnvironment = func(ctx context.Context, _ *sdk.Host, req extension.RunEnvironmentRequest) (map[string]string, error) {
			return r.RunEnvironment(ctx, req)
		}
	}
	if t, ok := b.x.(inproc.Targeter); ok {
		e.TargetResolvers = map[string]func(context.Context, *sdk.Host, domain.ID, string) (domain.CorrelationKey, []string, bool){}
		for kind, f := range t.Targets() {
			e.TargetResolvers[kind] = func(ctx context.Context, _ *sdk.Host, scope domain.ID, name string) (domain.CorrelationKey, []string, bool) {
				return f(ctx, scope, name)
			}
		}
	}
	if s, ok := b.x.(inproc.SegmentEnder); ok {
		e.SegmentEnded = func(ctx context.Context, _ *sdk.Host, execution domain.ID) { s.SegmentEnded(ctx, execution) }
	}
}

// providers are an Actor's providers on a scope, in name order.
func providers(a inproc.Actor, scope domain.ID) []extension.ActionProvider {
	ps := a.Actions(scope)
	var out []extension.ActionProvider
	for _, n := range sortedKeys(ps) {
		out = append(out, ps[n])
	}
	return out
}

func (b *Bridge) registration() extension.Registration {
	if t, ok := b.x.(inproc.Tooled); ok {
		b.mu.Lock()
		started := b.started
		b.mu.Unlock()
		if started {
			return t.Registration()
		}
	}
	return extension.Registration{}
}

// operation is a declared operation whose handler is the started
// extension's, of the declared kind.
func (b *Bridge) operation(d protocol.OperationDecl) extension.Operation {
	find := func() (extension.Operation, error) {
		for _, op := range b.registration().Operations {
			if op.Name == d.Name {
				return op, nil
			}
		}
		return extension.Operation{}, extension.ErrUndeclared
	}
	out := extension.Operation{Name: d.Name, Summary: d.Summary, Input: d.Input, Guidance: d.Guidance, Serves: d.Serves}
	switch d.Kind {
	case protocol.OperationWait:
		out.Wait = func(ctx context.Context, a extension.Authority, in domain.Payload) (extension.WaitPlan, error) {
			op, err := find()
			if err != nil || op.Wait == nil {
				return extension.WaitPlan{}, errors.Join(err, extension.ErrUndeclared)
			}
			return op.Wait(ctx, a, in)
		}
	case protocol.OperationEmit:
		out.Emit = func(ctx context.Context, a extension.Authority, in domain.Payload) (extension.EmitPlan, error) {
			op, err := find()
			if err != nil || op.Emit == nil {
				return extension.EmitPlan{}, errors.Join(err, extension.ErrUndeclared)
			}
			return op.Emit(ctx, a, in)
		}
	default:
		out.Handler = func(ctx context.Context, a extension.Authority, in domain.Payload) (domain.Payload, error) {
			op, err := find()
			if err != nil || op.Handler == nil {
				return domain.Payload{}, errors.Join(err, extension.ErrUndeclared)
			}
			return op.Handler(ctx, a, in)
		}
	}
	return out
}

func (b *Bridge) validate(_ context.Context, p protocol.ValidateParams) error {
	ct, ok := b.types[p.Kind+"/"+p.Type]
	switch {
	case p.Kind == protocol.ValidateExecutor:
		if x, ok := b.ext.Executors[p.Type].(interface {
			ValidateConfig(json.RawMessage, []string) error
		}); ok {
			return x.ValidateConfig(p.Config, p.SecretNames)
		}
		return nil
	case !ok:
		return nil
	case p.Kind == protocol.ValidateRoute && p.Binding != nil && ct.Binding != nil:
		return ct.Binding(p.Binding.Contract())
	case ct.Validate != nil:
		return ct.Validate(p.Config, p.SecretNames)
	}
	return nil
}

// describe is what a valid entry or route config sets up, by its config
// type's Describe.
func (b *Bridge) describe(p protocol.ValidateParams) []protocol.DescribeLine {
	ct, ok := b.types[p.Kind+"/"+p.Type]
	if !ok || ct.Describe == nil {
		return nil
	}
	var out []protocol.DescribeLine
	for _, e := range ct.Describe(p.Config) {
		out = append(out, protocol.DescribeLine{Name: e.Name, Detail: e.Detail})
	}
	return out
}

// configure starts the extension unless it has started (retrying a
// failed Start), hands it the generation, and answers the sources it
// registers and the webhook paths it serves.
func (b *Bridge) configure(ctx context.Context, h *sdk.Host, p protocol.ConfigureParams) (protocol.ConfigureResult, error) {
	b.mu.Lock()
	b.dryRun = p.DryRun
	b.mu.Unlock()
	if err := b.start(ctx, h); err != nil {
		return protocol.ConfigureResult{}, err
	}
	if c, ok := b.x.(inproc.Configurer); ok {
		c.Configure(ctx, p)
	}
	var out protocol.ConfigureResult
	i, ok := b.x.(inproc.Ingester)
	if !ok {
		return out, nil
	}
	w, hooks := b.x.(inproc.Webhooker)
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, s := range i.Sources() {
		if _, known := b.sources[s.Name]; !known {
			b.sources[s.Name] = s
		}
		s = b.sources[s.Name]
		decl := protocol.SourceDecl{Name: s.Name, Source: protocol.FromSource(s.Source), IntervalSeconds: int(s.Interval / time.Second)}
		if s.Ingress != nil {
			for _, f := range s.Ingress.Spec().Families {
				decl.Families = append(decl.Families, protocol.EventFamily{Type: f.Type, ReDerivable: f.ReDerivable, Loss: f.Loss})
			}
		}
		out.Sources = append(out.Sources, decl)
		if rest, ok := strings.CutPrefix(s.Name, b.x.Name()+"/"); hooks && ok && w.Webhook(rest) != nil {
			out.WebhookPaths = append(out.WebhookPaths, "/"+s.Name)
		}
	}
	return out, nil
}

// start starts the extension once: started is set only after Start
// succeeds, so a failed Start is retried by the next configure and the
// extension is never configured, registered or served unstarted.
func (b *Bridge) start(ctx context.Context, h *sdk.Host) error {
	b.startMu.Lock()
	defer b.startMu.Unlock()
	b.mu.Lock()
	started := b.started
	b.mu.Unlock()
	if started {
		return nil
	}
	if s, ok := b.x.(inproc.Starter); ok {
		if err := s.Start(ctx, b.host(h)); err != nil {
			return err
		}
	}
	b.mu.Lock()
	b.started = true
	b.mu.Unlock()
	return nil
}

// host is the inproc.Host the extension starts with, over the host's
// methods.
func (b *Bridge) host(h *sdk.Host) inproc.Host {
	state := ""
	b.mu.Lock()
	if b.srv != nil {
		state = b.srv.StateDir()
	}
	b.mu.Unlock()
	host := inproc.Host{
		Store: sdk.NewStore(h),
		Now:   time.Now,
		Log:   slog.New(slog.NewJSONHandler(os.Stderr, nil)).With("extension", b.x.Name()),
		Secrets: func(ctx context.Context, scope domain.ID, connection string, names []string) (map[string]string, error) {
			if connection == "" {
				return h.Secrets(ctx, scope, names...)
			}
			return h.ConnectionSecrets(ctx, scope, connection, names...)
		},
		Report: func(code string) {
			if code != "" {
				slog.New(slog.NewJSONHandler(os.Stderr, nil)).Warn("extension report", "extension", b.x.Name(), "code", code)
			}
		},
		Nudge:     func() {},
		StateDir:  state,
		DryRun:    b.isDryRun(),
		Artifacts: sdk.NewArtifacts(h),
		Render: func(ctx context.Context, e domain.Event) (string, bool) {
			text, ok, err := h.RenderOwned(ctx, e)
			return text, ok && err == nil
		},
		OpenLog: func(name string) (io.WriteCloser, error) {
			dir := filepath.Join(state, "logs")
			if err := os.MkdirAll(dir, 0o700); err != nil {
				return nil, err
			}
			return os.OpenFile(filepath.Join(dir, name+".jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		},
		Deliver: func(ctx context.Context, source string, d extension.Delivery) error {
			_, err := h.Deliver(ctx, source, protocol.FromDelivery(d))
			return err
		},
		Authorize: func(kind string, from, to domain.ID) error {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			return h.Authorize(ctx, kind, from, to)
		},
		WaitTarget: func(ctx context.Context, scope domain.ID, kind, name string) (domain.CorrelationKey, []string, bool) {
			key, types, ok, err := h.WaitTarget(ctx, scope, kind, name)
			return key, types, ok && err == nil
		},
		WaitProgress: h.WaitProgress,
		SettleHold:   h.SettleHold,
		Act:          h.Act,
		Actions:      h.Actions,
		Capture: func(ctx context.Context, scope, name string, at time.Time, props map[string]any) error {
			return h.Capture(ctx, domain.ID(scope), name, at, props)
		},
	}
	b.services(&host, h)
	return host
}

func (b *Bridge) isDryRun() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.dryRun
}

// taskName is a task's declared name: a package's tasks are named after
// it, so one named otherwise is prefixed with the package's name.
func taskName(pkg, task string) string {
	if task == pkg || strings.HasPrefix(task, pkg+"-") {
		return task
	}
	return pkg + "-" + strings.ReplaceAll(task, "_", "-")
}

func appendType(ts []protocol.TypeDecl, t string) []protocol.TypeDecl {
	for _, x := range ts {
		if x.Type == t {
			return ts
		}
	}
	return append(ts, protocol.TypeDecl{Type: t})
}

func hasExtension(xs []protocol.ExtensionDecl, t string) bool {
	for _, x := range xs {
		if x.Type == t {
			return true
		}
	}
	return false
}

func hasRoute(rs []protocol.RouteDecl, h string) bool {
	for _, r := range rs {
		if r.Handler == h {
			return true
		}
	}
	return false
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
