// Package sdk is the minimal extension-side runtime of protocol version 1
// (07-02): it serves the public Go contracts (extension.Ingress, Executor,
// RouteHandler, Operation and the optional executor controls) over the
// protocol and gives the extension a Host client for the scoped
// extension-to-host methods. An external extension written against it passes
// the same conformance suites as an in-process one, because the host adapts
// the wire back onto the same interfaces.
//
// 07-03 gave it its shape (provisional): one Extension field per manifest
// surface, Host for the scoped extension-to-host methods, Store
// (storage.Store over the host) and Artifacts (storage.Artifacts over the
// host) so in-process code runs unchanged, and PackageCommand for packaging.
// New and its registrations (declare.go) fill those fields and derive the
// manifest from them, so each surface is declared once.
// The guide is site/src/content/docs/architecture/extension-packaging.md.
package sdk

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/observability"
	"github.com/open-conduit/open-conduit/protocol"
	"github.com/open-conduit/open-conduit/protocol/jsonrpc"
)

// RecurrenceFunc mirrors the host's calendar recurrence callback.
type RecurrenceFunc func(spec []byte, fired domain.Timer, now time.Time) (*domain.Event, *domain.Timer, error)

// Upcaster upgrades a payload of one schema base from version `from` to
// from+1. The SDK chains upcasters up to the manifest's current version
// before handing an older persisted payload to extension code.
type Upcaster func(from int, data json.RawMessage) (json.RawMessage, error)

// Extension is what an extension process serves. Only the surfaces its
// manifest declares are reachable; the host refuses the rest.
type Extension struct {
	Manifest protocol.Manifest
	// Migrate upgrades the extension's own KV namespaces from the stored
	// layout version to Manifest.KVVersion with precondition-guarded
	// kv.commit batches. It must be idempotent: a crash mid-migration is
	// retried at the next start.
	Migrate func(ctx context.Context, h *Host, from, to int) error
	// Configure applies a configuration generation and returns the ingress
	// sources and webhook paths the extension registers.
	Configure func(ctx context.Context, h *Host, p protocol.ConfigureParams) (protocol.ConfigureResult, error)
	// Ingress returns the adapter of a registered source by name.
	Ingress     func(name string) (extension.Ingress, bool)
	Routes      map[string]extension.RouteHandler
	Operations  []extension.Operation
	Filter      func(filter domain.Payload, event domain.Event) bool
	Executors   map[string]extension.Executor
	Recurrences map[string]RecurrenceFunc
	Triggers    map[string]func([]domain.Event) int
	Tasks       map[string]func(ctx context.Context, h *Host) error
	Checks      map[string]func(ctx context.Context, h *Host, deep bool) (string, error)
	// CheckRuns are checks that read the host's view of the configuration
	// (p.Config) or report a status, code and items of their own. A name in
	// both maps runs from CheckRuns.
	CheckRuns map[string]func(ctx context.Context, h *Host, p protocol.CheckRunParams) (protocol.CheckRunResult, error)
	Render    func(domain.Event) (string, bool)
	// Upcasters by schema base name.
	Upcasters map[string]Upcaster

	// Surfaces adapted in 07-03.

	// Validate checks one opaque config object of a declared connection,
	// extension, route, executor or resource type (the config.Registry
	// validators). A nil Validate accepts every object of a declared type.
	// Return an error to refuse; the host reports only that the object is
	// invalid, never the message.
	Validate func(ctx context.Context, p protocol.ValidateParams) error
	// Webhooks returns the handler of a webhook path the last configure
	// registered. The handler verifies the request and pushes through
	// Host.Deliver before writing a 2xx status.
	Webhooks func(path string) (http.Handler, bool)
	// Providers are resource providers by declared type.
	Providers map[string]extension.ResourceProvider
	// Exporter receives telemetry batches (manifest exporters) and is shut
	// down with the extension.
	Exporter observability.Exporter
	// Sink receives every named telemetry event the host records, from
	// every scope (permission telemetry.sink). Write them with
	// Host.Capture (permission telemetry.capture).
	Sink func(ctx context.Context, h *Host, e protocol.TelemetryEvent) error
	// RunEnvironment contributes an environment layer to a run of a Work in
	// a scope that uses the extension (manifest run_environment).
	RunEnvironment func(ctx context.Context, h *Host, r extension.RunEnvironmentRequest) (map[string]string, error)
	// Annotate contributes Work annotations (manifest annotate).
	Annotate func(ctx context.Context, h *Host, w domain.Work) ([]protocol.Annotation, error)
	// Actions lists the actions available on a target now and Act
	// authorises one invocation (manifest actions; extension.ActionProvider).
	// Act refuses with *extension.OperationError, which reaches the caller
	// verbatim.
	Actions func(ctx context.Context, h *Host, t extension.ActionTarget) ([]extension.Action, error)
	Act     func(ctx context.Context, h *Host, c extension.ActionCall) (extension.ActionPlan, error)
	// Panels returns the panels to show on a target now (manifest panels;
	// extension.PanelProvider). Their buttons name Actions' IDs.
	Panels func(ctx context.Context, h *Host, t extension.ActionTarget) ([]extension.Panel, error)
	// ConfigCandidate says what a Work proposes for the operator to review
	// and apply (manifest config_candidate, permission config.read;
	// extension.ConfigCandidate): storage.ErrNotFound for a Work with
	// nothing proposed. Its handles are ones Host.ConfigBase and
	// Host.ConfigEdit returned.
	ConfigCandidate func(ctx context.Context, h *Host, w domain.Work) (extension.Candidate, error)
	// Setups are `open-conduit init <type>` flows by declared setup type.
	Setups map[string]extension.Setup
}

// Server is one served connection.
type Server struct {
	ext   *Extension
	conn  *jsonrpc.Conn
	host  *Host
	state string

	mu       sync.Mutex
	ready    bool
	draining bool
	handles  map[string]extension.RunHandle
	commits  map[string]func(extension.Emitted) (extension.EmitCommit, error)
	order    []string
}

// Serve runs ext on the process's stdin and stdout until the host closes
// stdin (a host crash also ends here: an extension never outlives its host).
// os.Stdout is moved to stderr first, so a stray print in extension code
// cannot corrupt the protocol stream.
func Serve(ext *Extension) error {
	out := os.Stdout
	os.Stdout = os.Stderr
	s := NewServer(ext, os.Stdin, out)
	<-s.Done()
	return nil
}

// NewServer serves ext on r/w (used by Serve and by in-process pipe tests).
func NewServer(ext *Extension, r io.Reader, w io.Writer) *Server {
	s := &Server{ext: ext, handles: map[string]extension.RunHandle{}, commits: map[string]func(extension.Emitted) (extension.EmitCommit, error){}}
	s.conn = jsonrpc.NewConn(r, w, s.handle, jsonrpc.Options{})
	s.host = &Host{conn: s.conn}
	return s
}

// Done is closed when the connection ends.
func (s *Server) Done() <-chan struct{} { return s.conn.Done() }

// Host returns the client for extension-to-host calls.
func (s *Server) Host() *Host { return s.host }

func newID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func notFound(method string) error {
	return protocol.Errorf(protocol.CodeMethodNotFound, "no such method "+method)
}

type hostKey struct{}

// HostFrom returns the Host of the connection serving ctx's request, so an
// operation handler or adapter can call the host while it serves.
func HostFrom(ctx context.Context) *Host {
	h, _ := ctx.Value(hostKey{}).(*Host)
	return h
}

func (s *Server) handle(ctx context.Context, method string, raw json.RawMessage) (any, error) {
	ctx = context.WithValue(ctx, hostKey{}, s.host)
	if method == "initialize" {
		return s.initialize(raw)
	}
	s.mu.Lock()
	ready, draining := s.ready, s.draining
	s.mu.Unlock()
	if !ready {
		return nil, protocol.Errorf(protocol.CodeInvalid, "initialize first")
	}
	if draining {
		return nil, protocol.Errorf(protocol.CodeShuttingDown, "shutting down")
	}
	switch method {
	case "configure":
		return s.configure(ctx, raw)
	case "shutdown":
		s.mu.Lock()
		s.draining = true
		s.mu.Unlock()
		if s.ext.Exporter != nil {
			_ = s.ext.Exporter.Shutdown(ctx)
		}
		return struct{}{}, nil
	case "validate", "webhook.handle", "provider.acquire", "provider.lookup", "provider.reconcile", "telemetry.export", "run.environment", "work.annotate", "work.actions", "work.act", "view.render", "config.candidate", "setup.run":
		return s.surface(ctx, method, raw)
	case "ingress.push", "ingress.reconcile":
		return s.ingress(ctx, method, raw)
	case "telemetry.sink":
		var p protocol.TelemetryEvent
		if err := jsonrpc.Params(raw, &p); err != nil {
			return nil, err
		}
		if s.ext.Sink == nil {
			return nil, notFound(method)
		}
		return struct{}{}, s.ext.Sink(ctx, s.host, p)
	case "route.match":
		var p protocol.RouteMatchParams
		if err := jsonrpc.Params(raw, &p); err != nil {
			return nil, err
		}
		h, ok := s.ext.Routes[p.Handler]
		if !ok {
			return nil, notFound(method)
		}
		in := p.Input.Extension()
		var err error
		if in.Event.Event.Data, err = s.Upcast(in.Event.Event.Data); err != nil {
			return nil, err
		}
		d, err := h.Match(ctx, in)
		if err != nil {
			return nil, err
		}
		return protocol.FromRouteDecision(d), nil
	case "route.samples":
		var p protocol.RouteSamplesParams
		if err := jsonrpc.Params(raw, &p); err != nil {
			return nil, err
		}
		h, ok := s.ext.Routes[p.Handler]
		if !ok {
			return nil, notFound(method)
		}
		out := protocol.RouteSamplesResult{Samples: []protocol.RouteSample{}}
		if sampler, ok := h.(extension.RouteSampler); ok {
			for _, x := range sampler.Samples(ctx, extension.SampleInput{Scope: p.Scope.Domain(), Route: p.Route.Domain(), Now: p.Now}) {
				out.Samples = append(out.Samples, protocol.FromRouteSample(x))
			}
		}
		return out, nil
	case "work_kind.trigger":
		var p protocol.WorkKindTriggerParams
		if err := jsonrpc.Params(raw, &p); err != nil {
			return nil, err
		}
		f, ok := s.ext.Triggers[p.Kind]
		if !ok {
			return nil, notFound(method)
		}
		events, err := s.upcastEvents(protocol.ToEvents(p.Inputs))
		if err != nil {
			return nil, err
		}
		return protocol.WorkKindTriggerResult{Index: f(events)}, nil
	case "recurrence.next":
		var p protocol.RecurrenceNextParams
		if err := jsonrpc.Params(raw, &p); err != nil {
			return nil, err
		}
		f, ok := s.ext.Recurrences[p.Kind]
		if !ok {
			return nil, notFound(method)
		}
		occ, next, err := f(p.Spec, p.Fired.Domain(), p.Now)
		if err != nil {
			return nil, err
		}
		var out protocol.RecurrenceNextResult
		if occ != nil {
			e := protocol.FromEvent(*occ)
			out.Occurrence = &e
		}
		if next != nil {
			t := protocol.FromTimer(*next)
			out.Next = &t
		}
		return out, nil
	case "dependency.filter":
		var p protocol.DependencyFilterParams
		if err := jsonrpc.Params(raw, &p); err != nil {
			return nil, err
		}
		if s.ext.Filter == nil {
			return nil, notFound(method)
		}
		e := p.Event.Domain()
		var err error
		if e.Data, err = s.Upcast(e.Data); err != nil {
			return nil, err
		}
		return protocol.DependencyFilterResult{Match: s.ext.Filter(p.Filter.Domain(), e)}, nil
	case "operation.call", "operation.wait", "operation.emit":
		return s.operation(ctx, method, raw)
	case "emit.commit":
		var p protocol.EmitCommitParams
		if err := jsonrpc.Params(raw, &p); err != nil {
			return nil, err
		}
		s.mu.Lock()
		f := s.commits[p.CommitID]
		delete(s.commits, p.CommitID)
		s.mu.Unlock()
		if f == nil {
			return nil, protocol.Errorf(protocol.CodeNotFound, "unknown commit")
		}
		c, err := f(p.Emitted.Extension())
		if err != nil {
			return nil, err
		}
		return protocol.FromEmitCommit(c), nil
	case "event.render":
		var p protocol.EventRenderParams
		if err := jsonrpc.Params(raw, &p); err != nil {
			return nil, err
		}
		if s.ext.Render == nil {
			return nil, notFound(method)
		}
		e := p.Event.Domain()
		var err error
		if e.Data, err = s.Upcast(e.Data); err != nil {
			return nil, err
		}
		text, ok := s.ext.Render(e)
		return protocol.EventRenderResult{Text: text, OK: ok}, nil
	case "task.run":
		var p protocol.TaskRunParams
		if err := jsonrpc.Params(raw, &p); err != nil {
			return nil, err
		}
		f, ok := s.ext.Tasks[p.Task]
		if !ok {
			return nil, notFound(method)
		}
		return struct{}{}, f(ctx, s.host)
	case "check.run":
		var p protocol.CheckRunParams
		if err := jsonrpc.Params(raw, &p); err != nil {
			return nil, err
		}
		if run, ok := s.ext.CheckRuns[p.Check]; ok {
			return run(ctx, s.host, p)
		}
		f, ok := s.ext.Checks[p.Check]
		if !ok {
			return nil, notFound(method)
		}
		detail, err := f(ctx, s.host, p.Deep)
		if err != nil {
			return protocol.CheckRunResult{OK: false, Detail: detail}, nil
		}
		return protocol.CheckRunResult{OK: true, Detail: detail}, nil
	}
	if len(method) > 9 && method[:9] == "executor." {
		return s.executor(ctx, method, raw)
	}
	return nil, notFound(method)
}

func (s *Server) initialize(raw json.RawMessage) (any, error) {
	var p protocol.InitializeParams
	if err := jsonrpc.Params(raw, &p); err != nil {
		return nil, err
	}
	v, err := protocol.Negotiate(p.ProtocolVersions, s.ext.Manifest.Protocol)
	if err != nil {
		return nil, protocol.Errorf(protocol.CodeUnsupportedVersion, "no common protocol version")
	}
	manifest, err := json.Marshal(s.ext.Manifest)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.ready, s.state = true, p.StateDir
	s.mu.Unlock()
	return protocol.InitializeResult{ProtocolVersion: v, Manifest: manifest}, nil
}

// StateDir is the private state directory the host gave at initialize.
func (s *Server) StateDir() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

func (s *Server) configure(ctx context.Context, raw json.RawMessage) (any, error) {
	var p protocol.ConfigureParams
	if err := jsonrpc.Params(raw, &p); err != nil {
		return nil, err
	}
	if p.KV.Stored < p.KV.Declared {
		if s.ext.Migrate == nil && p.KV.Stored != 0 {
			return nil, protocol.Errorf(protocol.CodeUnsupported, "no KV migration")
		}
		if s.ext.Migrate != nil {
			if err := s.ext.Migrate(ctx, s.host, p.KV.Stored, p.KV.Declared); err != nil {
				return nil, err
			}
		}
	}
	out := protocol.ConfigureResult{}
	if s.ext.Configure != nil {
		var err error
		if out, err = s.ext.Configure(ctx, s.host, p); err != nil {
			return nil, err
		}
	}
	out.KVVersion = s.ext.Manifest.KVVersion
	return out, nil
}

func (s *Server) ingress(ctx context.Context, method string, raw json.RawMessage) (any, error) {
	if s.ext.Ingress == nil {
		return nil, notFound(method)
	}
	if method == "ingress.push" {
		var p protocol.IngressPushParams
		if err := jsonrpc.Params(raw, &p); err != nil {
			return nil, err
		}
		in, ok := s.ext.Ingress(p.Name)
		if !ok {
			return nil, protocol.Errorf(protocol.CodeNotFound, "unknown source")
		}
		r, err := in.Push(ctx, p.Source.Extension(), p.Delivery.Extension())
		if err != nil {
			return nil, err
		}
		return protocol.FromPushResult(r), nil
	}
	var p protocol.IngressReconcileParams
	if err := jsonrpc.Params(raw, &p); err != nil {
		return nil, err
	}
	in, ok := s.ext.Ingress(p.Name)
	if !ok {
		return nil, protocol.Errorf(protocol.CodeNotFound, "unknown source")
	}
	snap, err := in.Reconcile(ctx, p.Scan.Extension())
	if err != nil {
		return nil, err
	}
	return protocol.FromSnapshot(snap), nil
}

func (s *Server) operation(ctx context.Context, method string, raw json.RawMessage) (any, error) {
	var p protocol.OperationParams
	if err := jsonrpc.Params(raw, &p); err != nil {
		return nil, err
	}
	var op *extension.Operation
	for i := range s.ext.Operations {
		if s.ext.Operations[i].Name == p.Operation {
			op = &s.ext.Operations[i]
		}
	}
	if op == nil {
		return nil, protocol.Errorf(protocol.CodeUndeclared, "unknown operation")
	}
	ctx = extension.WithCallRequestID(ctx, p.RequestID)
	a, in := p.Authority.Extension(), p.Input.Domain()
	switch {
	case method == "operation.call" && op.Handler != nil:
		out, err := op.Handler(ctx, a, in)
		if err != nil {
			return nil, err
		}
		return protocol.OperationResult{Output: protocol.FromPayload(out)}, nil
	case method == "operation.wait" && op.Wait != nil:
		plan, err := op.Wait(ctx, a, in)
		if err != nil {
			return nil, err
		}
		return protocol.FromWaitPlan(plan), nil
	case method == "operation.emit" && op.Emit != nil:
		plan, err := op.Emit(ctx, a, in)
		if err != nil {
			return nil, err
		}
		out := protocol.EmitPlan{Event: protocol.FromEvent(plan.Event), Cause: string(plan.Cause)}
		if plan.Commit != nil {
			out.CommitID = newID()
			s.mu.Lock()
			s.commits[out.CommitID] = plan.Commit
			s.order = append(s.order, out.CommitID)
			// Commits the host never called (its transaction failed before
			// acceptance) are dropped oldest first.
			for len(s.order) > 1024 {
				delete(s.commits, s.order[0])
				s.order = s.order[1:]
			}
			s.mu.Unlock()
		}
		return out, nil
	}
	return nil, protocol.Errorf(protocol.CodeUndeclared, "operation kind mismatch")
}

func (s *Server) executor(ctx context.Context, method string, raw json.RawMessage) (any, error) {
	switch method {
	case "executor.run", "executor.recover", "executor.resume":
		var p protocol.ExecutorRunParams
		if err := jsonrpc.Params(raw, &p); err != nil {
			return nil, err
		}
		e, ok := s.ext.Executors[p.Executor]
		if !ok {
			return nil, notFound(method)
		}
		r := p.Request.Extension()
		var err error
		if r.Trigger.Data, err = s.Upcast(r.Trigger.Data); err != nil {
			return nil, err
		}
		if r.Inputs, err = s.upcastEvents(r.Inputs); err != nil {
			return nil, err
		}
		run := p.RunID
		r.PersistRecovery = func(state domain.Payload) error {
			// Not the request context: an executor may re-acknowledge
			// metadata after Run returned (adoption under a new lease).
			return s.conn.Call(context.Background(), "execution.persist_recovery", protocol.PersistRecoveryParams{RunID: run, State: protocol.FromPayload(state)}, nil)
		}
		if p.Request.Context.Resources {
			r.Context.Resources = &broker{conn: s.conn, run: run}
		}
		var h extension.RunHandle
		switch method {
		case "executor.run":
			h, err = e.Run(ctx, r)
		case "executor.recover":
			if p.State == nil {
				return nil, protocol.Errorf(protocol.CodeInvalid, "missing state")
			}
			h, err = e.Recover(ctx, r, p.State.Domain())
		default:
			re, ok := e.(extension.ResumeExecutor)
			if !ok || p.State == nil {
				return nil, protocol.Errorf(protocol.CodeUnsupported, "resume")
			}
			h, err = re.Resume(ctx, r, p.State.Domain())
		}
		if err != nil {
			return nil, err
		}
		if h == nil {
			return nil, protocol.Errorf(protocol.CodeInvalid, "no handle")
		}
		id := newID()
		s.mu.Lock()
		s.handles[id] = h
		s.mu.Unlock()
		return protocol.ExecutorRunResult{Handle: id}, nil
	case "executor.wait":
		var p protocol.ExecutorWaitParams
		if err := jsonrpc.Params(raw, &p); err != nil {
			return nil, err
		}
		s.mu.Lock()
		h := s.handles[p.Handle]
		s.mu.Unlock()
		if h == nil {
			return nil, protocol.Errorf(protocol.CodeNotFound, "unknown handle")
		}
		r, err := h.Wait(ctx)
		if err != nil {
			return nil, err
		}
		s.mu.Lock()
		delete(s.handles, p.Handle)
		s.mu.Unlock()
		return protocol.FromResult(r), nil
	}
	var p protocol.ExecutorTargetParams
	if err := jsonrpc.Params(raw, &p); err != nil {
		return nil, err
	}
	e, ok := s.ext.Executors[p.Executor]
	if !ok {
		return nil, notFound(method)
	}
	unsupported := protocol.Errorf(protocol.CodeUnsupported, method)
	switch method {
	case "executor.interrupt":
		if x, ok := e.(extension.InterruptExecutor); ok {
			return struct{}{}, x.Interrupt(ctx, domain.ID(p.ExecutionID))
		}
	case "executor.inject":
		if x, ok := e.(extension.InjectExecutor); ok && p.Event != nil {
			ev := p.Event.Domain()
			return struct{}{}, x.Inject(ctx, domain.ID(p.ExecutionID), ev)
		}
	case "executor.stop":
		if x, ok := e.(extension.StopExecutor); ok {
			return struct{}{}, x.Stop(ctx, domain.ID(p.ExecutionID), p.Epoch)
		}
	case "executor.forget":
		if x, ok := e.(extension.ForgetExecutor); ok {
			return struct{}{}, x.Forget(ctx, domain.ID(p.ExecutionID))
		}
	case "executor.attach":
		if x, ok := e.(extension.AttachExecutor); ok {
			spec, err := x.Attach(ctx, domain.ID(p.WorkID))
			if err != nil {
				return nil, err
			}
			return protocol.AttachSpec{Argv: spec.Argv, Dir: spec.Dir}, nil
		}
	default:
		return nil, notFound(method)
	}
	return nil, unsupported
}

// Upcast chains the registered upcasters from p's version to the manifest's
// current version. Foreign and current payloads pass unchanged; a retired,
// newer or undeclared own payload is CodeSchema (the host normally filters
// those before calling).
func (s *Server) Upcast(p domain.Payload) (domain.Payload, error) {
	return Upcast(s.ext.Manifest, s.ext.Upcasters, p)
}

// Upcast is Server.Upcast for a given manifest and upcaster set.
func Upcast(m protocol.Manifest, ups map[string]Upcaster, p domain.Payload) (domain.Payload, error) {
	switch m.PayloadStatus(p) {
	case protocol.SchemaForeign, protocol.SchemaCurrent:
		return p, nil
	case protocol.SchemaUpcast:
	default:
		return p, protocol.Errorf(protocol.CodeSchema, "unreadable payload schema")
	}
	base, v, _ := protocol.ParseSchemaID(p.Schema)
	var current int
	for _, d := range m.Provides.Schemas {
		if d.Name == base {
			current = d.Versions[len(d.Versions)-1]
		}
	}
	up := ups[base]
	if up == nil {
		return p, protocol.Errorf(protocol.CodeSchema, "no upcaster for "+base)
	}
	data := p.Data
	for ; v < current; v++ {
		var err error
		if data, err = up(v, data); err != nil {
			return p, protocol.Errorf(protocol.CodeSchema, fmt.Sprintf("upcast %s from v%d failed", base, v))
		}
	}
	return domain.Payload{Schema: protocol.SchemaID(base, current), Data: data}, nil
}

func (s *Server) upcastEvents(es []domain.Event) ([]domain.Event, error) {
	for i := range es {
		var err error
		if es[i].Data, err = s.Upcast(es[i].Data); err != nil {
			return nil, err
		}
	}
	return es, nil
}

// broker is the resource broker an executor sees: resource.* on the host,
// bound by the host to the run's trusted owner.
type broker struct {
	conn *jsonrpc.Conn
	run  string
}

func (b *broker) Acquire(ctx context.Context, r extension.ResourceRequest) (extension.Resource, error) {
	var out protocol.ResourceResult
	err := b.conn.Call(ctx, "resource.acquire", protocol.ResourceAcquireParams{RunID: b.run, Mode: r.Mode, Params: protocol.FromPayload(r.Params)}, &out)
	if r.Log != nil && out.Log != "" {
		_, _ = io.WriteString(r.Log, out.Log)
	}
	if err != nil {
		return extension.Resource{}, err
	}
	if out.SetupFailed {
		return out.Resource.Extension(), extension.ErrResourceSetup
	}
	return out.Resource.Extension(), nil
}
func (b *broker) Lookup(ctx context.Context, ref string) (extension.Resource, error) {
	var out protocol.ResourceResult
	if err := b.conn.Call(ctx, "resource.lookup", protocol.ResourceLookupParams{RunID: b.run, Ref: ref}, &out); err != nil {
		return extension.Resource{}, err
	}
	return out.Resource.Extension(), nil
}

// PackageCommand answers the packaging subcommands every packaged binary
// serves (07-03): `package-manifest` prints the manifest file's JSON and
// `package-certification` prints the package's certification.json content
// (certification is marshaled as is; nil prints nothing). It reports
// whether args named one, so main returns after it.
func PackageCommand(args []string, m protocol.Manifest, certification any, out io.Writer) bool {
	if len(args) != 1 {
		return false
	}
	var v any
	switch args[0] {
	case "package-manifest":
		v = m
	case "package-certification":
		if certification == nil {
			return true
		}
		v = certification
	default:
		return false
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	_, _ = out.Write(append(b, '\n'))
	return true
}
