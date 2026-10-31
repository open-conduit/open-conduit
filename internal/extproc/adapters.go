package extproc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/internal/runtime"
	"github.com/open-conduit/open-conduit/protocol"
)

// Remote presents one supervised extension through the public contracts.
type Remote struct {
	p *Process
	s *Service
	m protocol.Manifest
	// skipped counts decisions answered by the host without a call because
	// the payload's schema is unreadable by the running manifest.
	skipped atomic.Int64
}

// NewRemote adapts a started process and its service.
func NewRemote(p *Process, s *Service) *Remote {
	return &Remote{p: p, s: s, m: p.o.Found.Manifest}
}

// Process returns the supervised process.
func (r *Remote) Process() *Process { return r.p }

// Manifest returns the extension's manifest.
func (r *Remote) Manifest() protocol.Manifest { return r.m }

// Service returns the extension's scoped host service.
func (r *Remote) Service() *Service { return r.s }

// SchemaSkips counts host-side deterministic answers for unreadable
// payload schemas (route no-match, filter false, generic render).
func (r *Remote) SchemaSkips() int64 { return r.skipped.Load() }

func (r *Remote) readable(p domain.Payload) bool {
	if r.m.PayloadStatus(p).Readable() {
		return true
	}
	r.skipped.Add(1)
	return false
}

func schemaError(err error) error { return protocol.Errorf(protocol.CodeSchema, err.Error()) }

// Ingress.

// Ingress returns the adapter of a source the extension registered at
// configure (protocol.SourceDecl).
func (r *Remote) Ingress(decl protocol.SourceDecl) extension.Ingress {
	spec := extension.IngressSpec{}
	for _, f := range decl.Families {
		spec.Families = append(spec.Families, extension.EventFamily{Type: f.Type, ReDerivable: f.ReDerivable, Loss: f.Loss})
	}
	return &remoteIngress{r: r, name: decl.Name, spec: spec}
}

type remoteIngress struct {
	r    *Remote
	name string
	spec extension.IngressSpec
}

func (i *remoteIngress) Spec() extension.IngressSpec { return i.spec }

// Push carries one delivery. A crash mid-call is protocol.ErrTransport:
// nothing was accepted, so the delivery is not acknowledged and the service
// retries (reconcile is the backstop).
func (i *remoteIngress) Push(ctx context.Context, s extension.Source, d extension.Delivery) (extension.PushResult, error) {
	var out protocol.PushResult
	if err := i.r.p.Call(ctx, "ingress.push", protocol.IngressPushParams{Name: i.name, Source: protocol.FromSource(s), Delivery: protocol.FromDelivery(d)}, &out); err != nil {
		return extension.PushResult{}, err
	}
	res := out.Extension()
	for _, o := range res.Observations {
		if err := i.r.m.CheckWrite(o.Event.Data); err != nil {
			return extension.PushResult{}, schemaError(err)
		}
	}
	for _, e := range res.Events {
		if err := i.r.m.CheckWrite(e.Data); err != nil {
			return extension.PushResult{}, schemaError(err)
		}
	}
	return res, nil
}

// Reconcile carries one page. A crash mid-page is a failed page: no absence
// is inferred and no checkpoint advances.
func (i *remoteIngress) Reconcile(ctx context.Context, s extension.Scan) (extension.Snapshot, error) {
	var out protocol.Snapshot
	if err := i.r.p.Call(ctx, "ingress.reconcile", protocol.IngressReconcileParams{Name: i.name, Scan: protocol.FromScan(s)}, &out); err != nil {
		return extension.Snapshot{}, err
	}
	snap := out.Extension()
	for _, o := range snap.Observations {
		if err := i.r.m.CheckWrite(o.Event.Data); err != nil {
			return extension.Snapshot{}, schemaError(err)
		}
	}
	return snap, nil
}

// Routing and in-transaction callbacks.

// RouteHandler returns the route handler adapter. An event whose payload
// schema the running manifest cannot read is answered "no match" by the
// host without a call, so the outcome is deterministic. A handler the
// manifest declares with samples is also an extension.RouteSampler.
func (r *Remote) RouteHandler(handler string) extension.RouteHandler {
	match := r.routeMatch(handler)
	for _, d := range r.m.Provides.Routes {
		if d.Handler == handler && d.Samples {
			return remoteSampler{routeFunc: match, r: r, handler: handler}
		}
	}
	return match
}

func (r *Remote) routeMatch(handler string) routeFunc {
	return routeFunc(func(ctx context.Context, in extension.RouteInput) (extension.RouteDecision, error) {
		if !r.readable(in.Event.Event.Data) {
			return extension.Skip("the extension does not read this event's data schema"), nil
		}
		var out protocol.RouteDecision
		if err := r.p.Call(ctx, "route.match", protocol.RouteMatchParams{Handler: handler, Input: protocol.FromRouteInput(in)}, &out); err != nil {
			return extension.RouteDecision{}, err
		}
		d := out.Extension()
		if d.Match && d.Kind != "" && !r.ownKind(d.Kind) {
			return extension.RouteDecision{}, protocol.Errorf(protocol.CodeUndeclared, "work kind not declared")
		}
		if err := extension.ValidateDisplay(d.Display); err != nil {
			// A display is optional: drop it rather than stop routing.
			// Certification refuses it (routes.decisions).
			r.p.o.Log.Warn("extension route display dropped", "extension", r.m.Name, "handler", handler, "reason", err.Error())
			d.Display = nil
		}
		return d, nil
	})
}

type routeFunc func(context.Context, extension.RouteInput) (extension.RouteDecision, error)

// remoteSampler carries route.samples. Samples are an aid to a route trace,
// so a failed call or a sample whose payload the extension may not write is
// logged and left out rather than failing the list.
type remoteSampler struct {
	routeFunc
	r       *Remote
	handler string
}

func (s remoteSampler) Samples(ctx context.Context, in extension.SampleInput) []extension.RouteSample {
	var out protocol.RouteSamplesResult
	if err := s.r.p.Call(ctx, "route.samples", protocol.RouteSamplesParams{Handler: s.handler, Scope: protocol.FromScope(in.Scope), Route: protocol.FromRoute(in.Route), Now: in.Now}, &out); err != nil {
		s.r.p.o.Log.Warn("extension route samples failed", "extension", s.r.m.Name, "handler", s.handler, "reason", err.Error())
		return nil
	}
	var samples []extension.RouteSample
	for _, x := range out.Samples {
		if err := s.r.m.CheckWrite(x.Event.Data.Domain()); err != nil {
			s.r.p.o.Log.Warn("extension route sample dropped", "extension", s.r.m.Name, "handler", s.handler, "sample", x.ID, "reason", err.Error())
			continue
		}
		samples = append(samples, x.Extension())
	}
	return samples
}

func (f routeFunc) Match(ctx context.Context, in extension.RouteInput) (extension.RouteDecision, error) {
	return f(ctx, in)
}

func (r *Remote) ownKind(kind string) bool {
	for _, k := range r.m.Provides.WorkKinds {
		if k.Kind == kind {
			return true
		}
	}
	return false
}

// Recurrence returns the bounded in-transaction recurrence callback. A
// timeout or crash returns an error, which aborts the timer's
// fire-and-rearm transaction; the timer stays due and is retried.
func (r *Remote) Recurrence(kind string) runtime.RecurrenceFunc {
	return func(spec []byte, fired domain.Timer, now time.Time) (*domain.Event, *domain.Timer, error) {
		var out protocol.RecurrenceNextResult
		if err := r.p.CallInTx(context.Background(), "recurrence.next", protocol.RecurrenceNextParams{Kind: kind, Spec: spec, Fired: protocol.FromTimer(fired), Now: now}, &out); err != nil {
			return nil, nil, err
		}
		var occ *domain.Event
		var next *domain.Timer
		if out.Occurrence != nil {
			e := out.Occurrence.Domain()
			if !r.s.ownFamily(e.Type) || !protocol.SourceNamespace(r.m.Name, e.Source) {
				return nil, nil, protocol.Errorf(protocol.CodeUnauthorized, "occurrence is not the extension's")
			}
			if err := r.m.CheckWrite(e.Data); err != nil {
				return nil, nil, schemaError(err)
			}
			occ = &e
		}
		if out.Next != nil {
			t := out.Next.Domain()
			next = &t
		}
		return occ, next, nil
	}
}

// WorkKind returns the declared lifecycle policy of an own Work kind. Its
// trigger selection is a bounded in-transaction call; on a timeout or crash
// it returns -1, the runtime's documented fallback (the first input).
func (r *Remote) WorkKind(kind string) (runtime.WorkKindPolicy, bool) {
	for _, k := range r.m.Provides.WorkKinds {
		if k.Kind != kind {
			continue
		}
		policy := runtime.WorkKindPolicy{Standing: k.Standing}
		if k.Trigger {
			policy.Trigger = func(inputs []domain.Event) int {
				var out protocol.WorkKindTriggerResult
				if err := r.p.CallInTx(context.Background(), "work_kind.trigger", protocol.WorkKindTriggerParams{Kind: kind, Inputs: protocol.FromEvents(inputs)}, &out); err != nil {
					return -1
				}
				return out.Index
			}
		}
		return policy, true
	}
	return runtime.WorkKindPolicy{}, false
}

// Toolbelt registration.

// Registration returns the toolbelt registration of the extension's
// declared operations; the toolbelt applies the same authority, family,
// causation and limit checks as for an in-process registration.
func (r *Remote) Registration() extension.Registration {
	reg := extension.Registration{Version: extension.ContractVersion, Name: r.m.Name}
	for _, f := range r.m.Provides.Families {
		reg.EventTypes = append(reg.EventTypes, f.Type)
	}
	for _, decl := range r.m.Provides.Operations {
		name := decl.Name
		op := extension.Operation{Name: name, Guidance: decl.Guidance}
		params := func(ctx context.Context, a extension.Authority, in domain.Payload) protocol.OperationParams {
			return protocol.OperationParams{Operation: name, Authority: protocol.FromAuthority(a), Input: protocol.FromPayload(in), RequestID: extension.CallRequestID(ctx)}
		}
		switch decl.Kind {
		case protocol.OperationHandler:
			op.Handler = func(ctx context.Context, a extension.Authority, in domain.Payload) (domain.Payload, error) {
				var out protocol.OperationResult
				if err := r.p.Call(ctx, "operation.call", params(ctx, a, in), &out); err != nil {
					return domain.Payload{}, err
				}
				return out.Output.Domain(), nil
			}
		case protocol.OperationWait:
			op.Wait = func(ctx context.Context, a extension.Authority, in domain.Payload) (extension.WaitPlan, error) {
				var out protocol.WaitPlan
				if err := r.p.Call(ctx, "operation.wait", params(ctx, a, in), &out); err != nil {
					return extension.WaitPlan{}, err
				}
				return out.Extension(), nil
			}
		case protocol.OperationEmit:
			op.Emit = func(ctx context.Context, a extension.Authority, in domain.Payload) (extension.EmitPlan, error) {
				var out protocol.EmitPlan
				if err := r.p.Call(ctx, "operation.emit", params(ctx, a, in), &out); err != nil {
					return extension.EmitPlan{}, err
				}
				plan := extension.EmitPlan{Event: out.Event.Domain(), Cause: domain.ID(out.Cause)}
				if err := r.m.CheckWrite(plan.Event.Data); err != nil {
					return extension.EmitPlan{}, schemaError(err)
				}
				if out.CommitID != "" {
					commit := out.CommitID
					plan.Commit = func(em extension.Emitted) (extension.EmitCommit, error) {
						var c protocol.EmitCommit
						if err := r.p.CallInTx(context.Background(), "emit.commit", protocol.EmitCommitParams{CommitID: commit, Emitted: protocol.FromEmitted(em)}, &c); err != nil {
							return extension.EmitCommit{}, err
						}
						return c.Extension(), nil
					}
				}
				return plan, nil
			}
		}
		reg.Operations = append(reg.Operations, op)
	}
	if r.m.Provides.DependencyFilter {
		// A bool callback has no error path: an unreadable payload, a
		// timeout or a crash counts as "does not match" (provisional; the
		// event is not lost, a later matching event still wakes the wait).
		reg.DependencyFilter = func(filter domain.Payload, e domain.Event) bool {
			if !r.readable(e.Data) {
				return false
			}
			var out protocol.DependencyFilterResult
			if err := r.p.CallInTx(context.Background(), "dependency.filter", protocol.DependencyFilterParams{Filter: protocol.FromPayload(filter), Event: protocol.FromEvent(e)}, &out); err != nil {
				return false
			}
			return out.Match
		}
	}
	return reg
}

// Render renders an own event for an agent prompt; an unreadable payload
// or a failed call falls back to the host's generic rendering (false).
func (r *Remote) Render(ctx context.Context, e domain.Event) (string, bool) {
	if !r.m.Provides.Render || !r.readable(e.Data) {
		return "", false
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var out protocol.EventRenderResult
	if err := r.p.Call(ctx, "event.render", protocol.EventRenderParams{Event: protocol.FromEvent(e)}, &out); err != nil {
		return "", false
	}
	return out.Text, out.OK
}

// Task runs one declared background pass.
func (r *Remote) Task(ctx context.Context, task string) error {
	return r.p.Call(ctx, "task.run", protocol.TaskRunParams{Task: task}, nil)
}

// Check runs one declared doctor check on the host's view of the
// configuration.
func (r *Remote) Check(ctx context.Context, check string, deep bool, cfg protocol.CheckConfig) (protocol.CheckRunResult, error) {
	var out protocol.CheckRunResult
	err := r.p.Call(ctx, "check.run", protocol.CheckRunParams{Check: check, Deep: deep, Config: cfg}, &out)
	return out, err
}

// Executors.

// Executor returns the adapter of a declared executor. Every optional
// control is implemented; one the manifest does not declare returns
// extension.ErrUnsupported without a call (so a stop is never confirmed and
// replacement stays blocked, exactly as for an executor without Stop).
func (r *Remote) Executor(name string) (extension.Executor, bool) {
	for _, x := range r.m.Provides.Executors {
		if x.Name == name {
			return &remoteExecutor{r: r, decl: x}, true
		}
	}
	return nil, false
}

type remoteExecutor struct {
	r    *Remote
	decl protocol.ExecutorDecl
}

func (e *remoteExecutor) Capabilities() domain.Capabilities { return e.decl.Capabilities.Domain() }

// ValidateConfig checks a binding's executor config through validate
// (config.ConfigValidator), so a binding is refused at load or apply time,
// exactly as for an in-process executor.
func (e *remoteExecutor) ValidateConfig(raw json.RawMessage, secretNames []string) error {
	return e.r.Validator(protocol.ValidateExecutor, e.decl.Name)(raw, secretNames)
}

// open binds the RunRequest's trusted callbacks to a fresh run ID. The run
// remembers the last metadata persisted through it, so the host can
// re-adopt the execution after the extension crashes.
func (e *remoteExecutor) open(req extension.RunRequest) (string, *trackedRun) {
	id := newID()
	t := &trackedRun{}
	persist := req.PersistRecovery
	e.r.s.openRun(id, &run{resources: req.Context.Resources, persist: func(p domain.Payload) error {
		if persist == nil {
			return protocol.Errorf(protocol.CodeUnsupported, "no persistence")
		}
		err := persist(p)
		t.mu.Lock()
		if err != nil {
			t.persistErr = err
		} else {
			t.state, t.persisted = p, true
		}
		t.mu.Unlock()
		return err
	}})
	return id, t
}

type trackedRun struct {
	mu         sync.Mutex
	state      domain.Payload
	persisted  bool
	persistErr error
}

func (e *remoteExecutor) start(ctx context.Context, method string, req extension.RunRequest, state *domain.Payload) (extension.RunHandle, error) {
	id, t := e.open(req)
	params := protocol.ExecutorRunParams{Executor: e.decl.Name, RunID: id, Request: protocol.FromRunRequest(req)}
	if state != nil {
		s := protocol.FromPayload(*state)
		params.State = &s
		t.state, t.persisted = *state, true
	}
	gen := e.r.p.Generation()
	var out protocol.ExecutorRunResult
	if err := e.r.p.Call(ctx, method, params, &out); err != nil {
		e.r.s.closeRun(id)
		t.mu.Lock()
		perr := t.persistErr
		t.mu.Unlock()
		if perr != nil {
			// The extension failed because the host refused persistence:
			// report the host's own error, not its echo.
			return nil, perr
		}
		return nil, err
	}
	return &remoteHandle{e: e, run: id, handle: out.Handle, gen: gen, req: req, t: t}, nil
}

func (e *remoteExecutor) Run(ctx context.Context, req extension.RunRequest) (extension.RunHandle, error) {
	return e.start(ctx, "executor.run", req, nil)
}

func (e *remoteExecutor) Recover(ctx context.Context, req extension.RunRequest, state domain.Payload) (extension.RunHandle, error) {
	return e.start(ctx, "executor.recover", req, &state)
}

func (e *remoteExecutor) Resume(ctx context.Context, req extension.RunRequest, state domain.Payload) (extension.RunHandle, error) {
	if !e.decl.Capabilities.Resume {
		return nil, extension.ErrUnsupported
	}
	return e.start(ctx, "executor.resume", req, &state)
}

func (e *remoteExecutor) target(ctx context.Context, method string, declared bool, p protocol.ExecutorTargetParams, out any) error {
	if !declared {
		return extension.ErrUnsupported
	}
	p.Executor = e.decl.Name
	return e.r.p.Call(ctx, method, p, out)
}

func (e *remoteExecutor) Interrupt(ctx context.Context, id domain.ID) error {
	return e.target(ctx, "executor.interrupt", e.decl.Capabilities.Interrupt, protocol.ExecutorTargetParams{ExecutionID: string(id)}, nil)
}

// Inject keeps the contract's three answers: nil (accepted for delivery),
// ErrNotAccepting (definitely not delivered) and anything else, including a
// crash mid-call, ambiguous.
func (e *remoteExecutor) Inject(ctx context.Context, id domain.ID, ev domain.Event) error {
	w := protocol.FromEvent(ev)
	return e.target(ctx, "executor.inject", e.decl.Capabilities.Inject, protocol.ExecutorTargetParams{ExecutionID: string(id), Event: &w}, nil)
}

func (e *remoteExecutor) Stop(ctx context.Context, id domain.ID, epoch uint64) error {
	return e.target(ctx, "executor.stop", e.decl.Controls.Stop, protocol.ExecutorTargetParams{ExecutionID: string(id), Epoch: epoch}, nil)
}

func (e *remoteExecutor) Forget(ctx context.Context, id domain.ID) error {
	return e.target(ctx, "executor.forget", e.decl.Controls.Forget, protocol.ExecutorTargetParams{ExecutionID: string(id)}, nil)
}

func (e *remoteExecutor) Attach(ctx context.Context, work domain.ID) (extension.AttachSpec, error) {
	var out protocol.AttachSpec
	if err := e.target(ctx, "executor.attach", e.decl.Controls.Attach, protocol.ExecutorTargetParams{WorkID: string(work)}, &out); err != nil {
		return extension.AttachSpec{}, err
	}
	return extension.AttachSpec{Argv: out.Argv, Dir: out.Dir}, nil
}

type remoteHandle struct {
	e      *remoteExecutor
	run    string
	handle string
	// gen is the process generation that issued handle.
	gen uint64
	req extension.RunRequest
	t   *trackedRun
}

// Wait observes the execution. The extension's crash is not the
// execution's failure: once the process has restarted, the host re-adopts
// the execution with executor.recover and the last metadata persisted
// through this run (never by reattaching to the old process), under the
// same lease-fenced persistence, so a superseded observer is fenced by the
// scheduler. ErrNotAdoptable, or a crash before anything was persisted, is
// returned for the scheduler's stop-and-retry policy.
func (h *remoteHandle) Wait(ctx context.Context) (extension.Result, error) {
	fail := func(err error) (extension.Result, error) {
		if ctx.Err() == nil {
			h.e.r.s.closeRun(h.run)
		}
		h.t.mu.Lock()
		perr := h.t.persistErr
		h.t.mu.Unlock()
		if perr != nil {
			return extension.Result{}, perr
		}
		return extension.Result{}, err
	}
	for {
		if gen := h.e.r.p.Generation(); gen != h.gen {
			h.t.mu.Lock()
			state, persisted := h.t.state, h.t.persisted
			h.t.mu.Unlock()
			if !persisted {
				return fail(fmt.Errorf("extproc: executor process failed before recovery metadata: %w", protocol.ErrTransport))
			}
			params := protocol.ExecutorRunParams{Executor: h.e.decl.Name, RunID: h.run, Request: protocol.FromRunRequest(h.req), State: ptr(protocol.FromPayload(state))}
			var re protocol.ExecutorRunResult
			if err := h.e.r.p.Call(ctx, "executor.recover", params, &re); err != nil {
				if errors.Is(err, protocol.ErrTransport) {
					if err := h.e.r.p.AwaitGeneration(ctx, gen); err != nil {
						return extension.Result{}, err
					}
					continue
				}
				return fail(err)
			}
			h.handle, h.gen = re.Handle, gen
		}
		gen := h.gen
		var out protocol.Result
		err := h.e.r.p.Call(ctx, "executor.wait", protocol.ExecutorWaitParams{Executor: h.e.decl.Name, Handle: h.handle}, &out)
		switch {
		case err == nil:
			h.e.r.s.closeRun(h.run)
			res := out.Extension()
			if verr := extension.ValidateResult(res); verr != nil {
				return extension.Result{}, verr
			}
			return res, nil
		case errors.Is(err, protocol.ErrTransport) || protocol.ErrorCode(err) == protocol.CodeShuttingDown:
			if err := h.e.r.p.AwaitGeneration(ctx, gen); err != nil {
				return extension.Result{}, err
			}
		case protocol.ErrorCode(err) == protocol.CodeNotFound && h.e.r.p.Generation() != gen:
			// The handle belonged to a process that has since restarted.
		default:
			return fail(err)
		}
	}
}

func ptr[T any](v T) *T { return &v }
