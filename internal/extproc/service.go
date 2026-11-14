package extproc

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/internal/runtime"
	"github.com/open-conduit/open-conduit/protocol"
	"github.com/open-conduit/open-conduit/protocol/jsonrpc"
	"github.com/open-conduit/open-conduit/storage"
)

// Armer is the timer surface timer.arm and timer.cancel adapt
// (runtime.Timers).
type Armer interface {
	ArmRecurrence(ctx context.Context, scope domain.ID, timer domain.Timer, expected domain.Revision, kind string, spec []byte) error
	Cancel(ctx context.Context, id domain.ID, expected domain.Revision) error
}

// Deps are the trusted host surfaces the service adapts. Now is required;
// a nil optional surface refuses its methods as unsupported. Store is
// optional only for a CLI-time process (setup), which has no coordination
// store: KV, state and event methods then refuse as unsupported.
type Deps struct {
	Store  storage.Store
	Now    func() time.Time
	Limits runtime.CausalLimits
	// Scopes returns the scopes whose current configuration uses the
	// extension. State reads, event.accept, timers, secrets and work.cancel
	// are limited to them.
	Scopes func() []domain.ID
	// Secrets resolves names bound to the extension's entries in scope: an
	// entry's selected secret names, or with connection set the secret names
	// of a connection one of its entries or one of the scope's bindings
	// names, of one of its connection types. The host refuses any other
	// name (B05 isolation).
	Secrets func(ctx context.Context, scope domain.ID, connection string, names []string) (map[string]string, error)
	// CurrentLimits, when set, supplies the causal limits of the current
	// configuration generation instead of Limits.
	CurrentLimits func() runtime.CausalLimits
	// Deliver is the runtime ingress push for one of the extension's own
	// registered sources; it returns after durable acceptance, with whether
	// the delivery added each Event (false for one already accepted).
	Deliver    func(ctx context.Context, name string, d extension.Delivery) ([]domain.AcceptedEvent, []bool, error)
	Artifacts  storage.Artifacts
	Timers     Armer
	CancelWork func(ctx context.Context, work domain.ID, expected domain.Revision, reason, key string) (domain.ID, error)
	// WaitProgress sets the progress line of work's active Wait for owner,
	// the extension whose operation declared it.
	WaitProgress func(ctx context.Context, owner string, work domain.ID, progress string) error
	// Nudge asks the host loop for an early pass after the extension made
	// something the loop acts on (an accepted Event, an armed timer).
	Nudge func()
	// Capture writes a named telemetry event to the host's telemetry events
	// log and hands it to every sink (telemetry.capture, permission
	// telemetry.capture).
	Capture func(ctx context.Context, scope, name string, at time.Time, props map[string]any) error
	// Act invokes an action for a person owner identified on its own
	// surface (action.submit, permission action.submit): the caller's kind
	// is owner.
	Act func(ctx context.Context, owner string, r extension.ActRequest) (extension.ActResult, error)
	// Actions lists what every provider offers on a scope or Work in it,
	// for owner (action.list, permission action.submit).
	Actions func(ctx context.Context, owner string, scope, work domain.ID) ([]extension.ActionOffer, error)
	// Probes are the setup probes the host, its extensions and executors
	// offer, by full name (probe.list and probe.run, permission probes).
	Probes func() map[string]extension.SetupProbe
	// SettleHold settles the hold owner's start gate placed on work
	// (work.settle_hold, permission work.start_gate).
	SettleHold func(ctx context.Context, owner string, work domain.ID, admit bool, reason, key string) (domain.ID, error)
	// Render renders an input Event for an agent's prompt through the
	// extension that owns its family (event.render_owned, for executors).
	Render func(ctx context.Context, e domain.Event) (string, bool)
	// WaitTarget resolves a named wait target through the extensions that
	// resolve its kind (wait.target, for wait operations).
	WaitTarget func(ctx context.Context, scope domain.ID, kind, name string) (domain.CorrelationKey, []string, bool)
	// Authorize answers whether scope from may read ("read") or file in
	// ("file") scope to under the operator's authorization config
	// (scope.authorize); the error says how to allow it.
	Authorize func(kind string, from, to domain.ID) error
	// Config is the host's config service, which keeps config text behind
	// handles and answers only redacted copies (config.base, config.edit,
	// config.validate, config.trace; permission config.read).
	Config ConfigHost
}

// run is the host side of one executor call: the trusted, lease-fenced
// PersistRecovery and resource broker of that RunRequest. Runs are
// addressed by an unguessable ID the host assigns per call; a closed run
// refuses persistence, so a restarted or stale process cannot persist for
// an attempt the host no longer serves through it.
type run struct {
	persist   func(domain.Payload) error
	resources extension.ResourceBroker
	sandbox   extension.SandboxBroker
	container extension.ContainerBroker
	closed    bool
}

// Service serves the extension-to-host methods for one extension. Every
// call is scoped by the identity the host bound at start (the manifest
// name), never by anything the extension sends.
type Service struct {
	name     string
	manifest protocol.Manifest
	enabled  map[string]bool
	deps     Deps
	// inTx reports whether the host is waiting on an in-transaction method
	// of this extension; any callback then is refused (reentrant_call).
	inTx func() bool

	mu         sync.Mutex
	runs       map[string]*run
	writers    map[string]storage.ArtifactWriter
	sources    map[string]bool
	reconciles map[string]extension.ResourceStatusFunc
	tunnels    map[string]func(string)
	setups     map[string]extension.SetupHost
	refused    int64
}

// NewService returns the scoped service for a validated manifest.
func NewService(m protocol.Manifest, deps Deps) (*Service, error) {
	if err := m.Validate(); err != nil {
		return nil, err
	}
	if deps.Now == nil {
		return nil, storage.ErrInvalid
	}
	if deps.Scopes == nil {
		deps.Scopes = func() []domain.ID { return nil }
	}
	s := &Service{name: m.Name, manifest: m, enabled: map[string]bool{}, deps: deps, inTx: func() bool { return false }, runs: map[string]*run{}, writers: map[string]storage.ArtifactWriter{}, sources: map[string]bool{}, reconciles: map[string]extension.ResourceStatusFunc{}, tunnels: map[string]func(string){}, setups: map[string]extension.SetupHost{}}
	for _, method := range m.Enabled() {
		s.enabled[method] = true
	}
	return s, nil
}

// SetSources replaces the source names ingress.deliver may push to (the
// names the last configure registered).
func (s *Service) SetSources(names []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sources = map[string]bool{}
	for _, n := range names {
		s.sources[n] = true
	}
}

func (s *Service) openRun(id string, r *run) {
	s.mu.Lock()
	s.runs[id] = r
	s.mu.Unlock()
}

// closeRun fences a run: later persistence or resource calls under its ID
// are refused.
func (s *Service) closeRun(id string) {
	s.mu.Lock()
	if r := s.runs[id]; r != nil {
		r.closed = true
		delete(s.runs, id)
	}
	s.mu.Unlock()
}

func (s *Service) allowedScopes() map[domain.ID]bool {
	out := map[domain.ID]bool{}
	for _, id := range s.deps.Scopes() {
		out[id] = true
	}
	return out
}

// ownKind says whether a Work kind is one the extension declares. Its own
// Work is readable and cancellable in any scope, so it can retire Work
// left in a scope that no longer uses it (introspection's removal policy).
func (s *Service) ownKind(kind string) bool {
	for _, k := range s.manifest.Provides.WorkKinds {
		if k.Kind == kind {
			return true
		}
	}
	return false
}

func unauthorized(msg string) error { return protocol.Errorf(protocol.CodeUnauthorized, msg) }
func invalid(msg string) error      { return protocol.Errorf(protocol.CodeInvalid, msg) }

// Refused counts authorization refusals (unauthorized answers) served to
// the extension: a certification signal that it reached outside its
// namespaces, scopes or sources.
func (s *Service) Refused() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.refused
}

// Handle serves one extension-to-host request.
func (s *Service) Handle(ctx context.Context, method string, raw json.RawMessage) (any, error) {
	out, err := s.handle(ctx, method, raw)
	if protocol.ErrorCode(err) == protocol.CodeUnauthorized {
		s.mu.Lock()
		s.refused++
		s.mu.Unlock()
	}
	return out, err
}

var storeMethods = map[string]bool{"kv.get": true, "kv.list": true, "kv.commit": true, "state.work": true, "state.works": true, "state.executions": true, "state.event": true, "state.events": true, "state.work_events": true, "state.scopes": true, "state.timers": true, "state.waits": true, "state.pending": true, "state.sequence": true, "event.accept": true, "timer.cancel": true, "work.cancel": true, "wait.progress": true}

func (s *Service) handle(ctx context.Context, method string, raw json.RawMessage) (any, error) {
	m, ok := protocol.Lookup(method)
	if !ok || m.Direction == protocol.HostToExtension {
		return nil, protocol.Errorf(protocol.CodeMethodNotFound, "no such host method")
	}
	if !s.enabled[method] {
		return nil, protocol.Errorf(protocol.CodeUndeclared, "method not enabled by the manifest")
	}
	if s.inTx() {
		return nil, protocol.Errorf(protocol.CodeReentrant, "callback while serving an in-transaction method")
	}
	if s.deps.Store == nil && storeMethods[method] {
		return nil, protocol.Errorf(protocol.CodeUnsupported, "no coordination store in this host process")
	}
	switch method {
	case "kv.get":
		var p protocol.KVGetParams
		if err := jsonrpc.Params(raw, &p); err != nil {
			return nil, err
		}
		return s.kvGet(ctx, p)
	case "kv.list":
		var p protocol.KVListParams
		if err := jsonrpc.Params(raw, &p); err != nil {
			return nil, err
		}
		return s.kvList(ctx, p)
	case "kv.commit":
		var p protocol.KVCommitParams
		if err := jsonrpc.Params(raw, &p); err != nil {
			return nil, err
		}
		return s.kvCommit(ctx, p)
	case "state.work", "state.works", "state.executions", "state.event", "state.events", "state.work_events", "state.scopes", "state.timers", "state.waits", "state.pending", "state.sequence":
		var p protocol.StateParams
		if err := jsonrpc.Params(raw, &p); err != nil {
			return nil, err
		}
		return s.state(ctx, method, p)
	case "event.accept":
		var p protocol.EventAcceptParams
		if err := jsonrpc.Params(raw, &p); err != nil {
			return nil, err
		}
		return s.accept(ctx, p)
	case "timer.arm":
		var p protocol.TimerArmParams
		if err := jsonrpc.Params(raw, &p); err != nil {
			return nil, err
		}
		return struct{}{}, s.arm(ctx, p)
	case "timer.cancel":
		var p protocol.TimerCancelParams
		if err := jsonrpc.Params(raw, &p); err != nil {
			return nil, err
		}
		return struct{}{}, s.cancelTimer(ctx, p)
	case "work.cancel":
		var p protocol.WorkCancelParams
		if err := jsonrpc.Params(raw, &p); err != nil {
			return nil, err
		}
		return s.cancelWork(ctx, p)
	case "wait.progress":
		var p protocol.WaitProgressParams
		if err := jsonrpc.Params(raw, &p); err != nil {
			return nil, err
		}
		if s.deps.WaitProgress == nil {
			return nil, protocol.Errorf(protocol.CodeUnsupported, "wait.progress")
		}
		return struct{}{}, s.deps.WaitProgress(ctx, s.name, domain.ID(p.WorkID), p.Progress)
	case "work.settle_hold":
		var p protocol.WorkSettleHoldParams
		if err := jsonrpc.Params(raw, &p); err != nil {
			return nil, err
		}
		if s.deps.SettleHold == nil {
			return nil, protocol.Errorf(protocol.CodeUnsupported, "work.settle_hold")
		}
		// Only a hold this extension's start gate placed settles.
		id, err := s.deps.SettleHold(ctx, s.name, domain.ID(p.WorkID), p.Admit, p.Reason, p.Key)
		if err != nil {
			return nil, err
		}
		return protocol.WorkSettleHoldResult{EventID: string(id)}, nil
	case "event.render_owned":
		var p protocol.EventRenderOwnedParams
		if err := jsonrpc.Params(raw, &p); err != nil {
			return nil, err
		}
		if s.deps.Render == nil {
			return nil, protocol.Errorf(protocol.CodeUnsupported, "event.render_owned")
		}
		text, ok := s.deps.Render(ctx, p.Event.Domain())
		return protocol.EventRenderResult{Text: text, OK: ok}, nil
	case "wait.target":
		var p protocol.WaitTargetParams
		if err := jsonrpc.Params(raw, &p); err != nil {
			return nil, err
		}
		if !s.allowedScopes()[domain.ID(p.ScopeID)] {
			return nil, unauthorized("scope is not configured for this extension")
		}
		if s.deps.WaitTarget == nil {
			return nil, protocol.Errorf(protocol.CodeUnsupported, "wait.target")
		}
		key, types, ok := s.deps.WaitTarget(ctx, domain.ID(p.ScopeID), p.Kind, p.Name)
		if !ok {
			return protocol.WaitTargetResult{}, nil
		}
		return protocol.WaitTargetResult{Key: protocol.FromKey(key), Types: types, OK: true}, nil
	case "scope.authorize":
		var p protocol.ScopeAuthorizeParams
		if err := jsonrpc.Params(raw, &p); err != nil {
			return nil, err
		}
		// The extension asks on behalf of one of its own scopes only.
		if !s.allowedScopes()[domain.ID(p.From)] {
			return nil, unauthorized("scope is not configured for this extension")
		}
		if p.Kind != "read" && p.Kind != "file" {
			return nil, invalid("kind is read or file")
		}
		if s.deps.Authorize == nil {
			return nil, protocol.Errorf(protocol.CodeUnsupported, "scope.authorize")
		}
		if err := s.deps.Authorize(p.Kind, domain.ID(p.From), domain.ID(p.To)); err != nil {
			return protocol.ScopeAuthorizeResult{Reason: err.Error()}, nil
		}
		return protocol.ScopeAuthorizeResult{Allowed: true}, nil
	case "artifact.create", "artifact.write", "artifact.commit", "artifact.abort":
		return s.artifact(ctx, method, raw)
	case "secret.resolve":
		var p protocol.SecretResolveParams
		if err := jsonrpc.Params(raw, &p); err != nil {
			return nil, err
		}
		if !s.allowedScopes()[domain.ID(p.ScopeID)] {
			return nil, unauthorized("scope is not configured for this extension")
		}
		if s.deps.Secrets == nil {
			return nil, protocol.Errorf(protocol.CodeUnsupported, "secrets")
		}
		values, err := s.deps.Secrets(ctx, domain.ID(p.ScopeID), p.Connection, p.Names)
		if err != nil {
			return nil, err
		}
		return protocol.SecretResolveResult{Values: values}, nil
	case "ingress.deliver":
		var p protocol.IngressDeliverParams
		if err := jsonrpc.Params(raw, &p); err != nil {
			return nil, err
		}
		s.mu.Lock()
		own := s.sources[p.Name]
		s.mu.Unlock()
		if !own || s.deps.Deliver == nil {
			return nil, unauthorized("not a source this extension registered")
		}
		accepted, added, err := s.deps.Deliver(ctx, p.Name, p.Delivery.Extension())
		if err != nil {
			return nil, err
		}
		return protocol.IngressDeliverResult{Accepted: protocol.FromAcceptedList(accepted), Added: added}, nil
	case "execution.persist_recovery":
		var p protocol.PersistRecoveryParams
		if err := jsonrpc.Params(raw, &p); err != nil {
			return nil, err
		}
		s.mu.Lock()
		r := s.runs[p.RunID]
		s.mu.Unlock()
		if r == nil || r.closed || r.persist == nil {
			return nil, protocol.Errorf(protocol.CodeConflict, "run is not served by this process")
		}
		return struct{}{}, r.persist(p.State.Domain())
	case "resource.acquire", "resource.lookup":
		return s.resource(ctx, method, raw)
	case "sandbox.plan", "sandbox.adopt", "container.launch", "container.adopt":
		return s.confine(ctx, method, raw)
	case "provider.owner_status":
		var p protocol.OwnerStatusParams
		if err := jsonrpc.Params(raw, &p); err != nil {
			return nil, err
		}
		s.mu.Lock()
		status := s.reconciles[p.Reconcile]
		s.mu.Unlock()
		if status == nil {
			return nil, protocol.Errorf(protocol.CodeConflict, "no provider.reconcile in progress under that ID")
		}
		st, err := status(ctx, p.Owner.Extension())
		if err != nil {
			return nil, err
		}
		return protocol.FromOwnerStatus(st), nil
	case "tunnel.url":
		var p protocol.TunnelURLParams
		if err := jsonrpc.Params(raw, &p); err != nil {
			return nil, err
		}
		s.mu.Lock()
		publish := s.tunnels[p.Run]
		s.mu.Unlock()
		if publish == nil {
			return nil, protocol.Errorf(protocol.CodeConflict, "no tunnel.run in progress under that ID")
		}
		// Webhooks are pointed at this URL: the host takes only what the
		// operator's own public_url could be.
		if p.URL != "" {
			if err := extension.CheckPublicURL(p.URL); err != nil {
				return nil, invalid(err.Error())
			}
		}
		publish(p.URL)
		return struct{}{}, nil
	case "setup.print", "setup.prompt", "setup.choose", "setup.open_url", "setup.store_secret", "setup.look_path", "setup.run_command":
		return s.setup(ctx, method, raw)
	case "telemetry.capture":
		var p protocol.TelemetryEvent
		if err := jsonrpc.Params(raw, &p); err != nil {
			return nil, err
		}
		return struct{}{}, s.capture(ctx, p)
	case "action.list":
		var p protocol.ActionListParams
		if err := jsonrpc.Params(raw, &p); err != nil {
			return nil, err
		}
		return s.actionList(ctx, p)
	case "action.submit":
		var p protocol.ActionSubmitParams
		if err := jsonrpc.Params(raw, &p); err != nil {
			return nil, err
		}
		return s.actionSubmit(ctx, p)
	case "probe.list":
		return s.probeList()
	case "probe.run":
		var p protocol.ProbeRunParams
		if err := jsonrpc.Params(raw, &p); err != nil {
			return nil, err
		}
		return s.probeRun(ctx, p)
	case "config.base", "config.edit", "config.validate", "config.trace":
		return s.config(ctx, method, raw)
	}
	return nil, protocol.Errorf(protocol.CodeUnsupported, "host method not offered in this release")
}

// KV.

// Version is the KV version token: a digest of the value.
func Version(value []byte) string {
	sum := sha256.Sum256(value)
	return hex.EncodeToString(sum[:16])
}

func (s *Service) ownNamespace(ns, key string) error {
	if key == "" || !protocol.Namespace(s.name, ns) {
		return unauthorized("namespace " + ns + " is not owned by " + s.name)
	}
	return nil
}

func (s *Service) kvGet(ctx context.Context, p protocol.KVGetParams) (protocol.KVGetResult, error) {
	var out protocol.KVGetResult
	if err := s.ownNamespace(p.Namespace, p.Key); err != nil {
		return out, err
	}
	err := s.deps.Store.Transact(ctx, storage.Guard{Now: s.deps.Now()}, func(tx storage.Tx) error {
		v, err := tx.Get(p.Namespace, p.Key)
		if errors.Is(err, storage.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		out = protocol.KVGetResult{Found: true, Entry: protocol.KVEntry{Key: p.Key, Value: v, Version: Version(v)}}
		return nil
	})
	return out, err
}

func (s *Service) kvList(ctx context.Context, p protocol.KVListParams) (protocol.KVListResult, error) {
	out := protocol.KVListResult{Entries: []protocol.KVEntry{}}
	if err := s.ownNamespace(p.Namespace, "-"); err != nil {
		return out, err
	}
	err := s.deps.Store.Transact(ctx, storage.Guard{Now: s.deps.Now()}, func(tx storage.Tx) error {
		rows, err := tx.List(p.Namespace, p.Prefix)
		for _, r := range rows {
			out.Entries = append(out.Entries, protocol.KVEntry{Key: r.Key, Value: r.Value, Version: Version(r.Value)})
		}
		return err
	})
	return out, err
}

// applyKV checks preconditions and applies puts and deletes inside tx.
func (s *Service) applyKV(tx storage.Tx, checks []protocol.KVCheck, puts []protocol.KVPut, deletes []protocol.KVDelete) error {
	for _, c := range checks {
		v, err := tx.Get(c.Namespace, c.Key)
		switch {
		case errors.Is(err, storage.ErrNotFound):
			if !c.Absent {
				return storage.ErrConflict
			}
		case err != nil:
			return err
		case c.Absent || Version(v) != c.Version:
			return storage.ErrConflict
		}
	}
	for _, p := range puts {
		if err := tx.Put(p.Namespace, p.Key, p.Value); err != nil {
			return err
		}
	}
	for _, d := range deletes {
		if err := tx.Delete(d.Namespace, d.Key); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) checkBatch(checks []protocol.KVCheck, puts []protocol.KVPut, deletes []protocol.KVDelete) error {
	for _, c := range checks {
		if err := s.ownNamespace(c.Namespace, c.Key); err != nil {
			return err
		}
		if c.Absent == (c.Version != "") {
			return invalid("a check names a version or absence")
		}
	}
	for _, p := range puts {
		if err := s.ownNamespace(p.Namespace, p.Key); err != nil {
			return err
		}
		if p.Value == nil {
			return invalid("a put needs a value")
		}
	}
	for _, d := range deletes {
		if err := s.ownNamespace(d.Namespace, d.Key); err != nil {
			return err
		}
	}
	return nil
}

// kvCommit applies one batch in one transaction: the checks, then each
// Emission's acceptance, then the writes. An Emission over its chain's
// causal bounds is refused alone and the rest commits; any other refusal or
// failure applies nothing.
func (s *Service) kvCommit(ctx context.Context, p protocol.KVCommitParams) (protocol.KVCommitResult, error) {
	var out protocol.KVCommitResult
	if err := s.checkBatch(p.Checks, p.Puts, p.Deletes); err != nil {
		return out, err
	}
	if len(p.Puts)+len(p.Deletes)+len(p.Emissions) == 0 {
		return out, invalid("empty batch")
	}
	now := s.deps.Now()
	events := make([]domain.Event, len(p.Emissions))
	for i, em := range p.Emissions {
		e, err := s.emitted(em.ScopeID, em.Event, now)
		if err != nil {
			return out, err
		}
		events[i] = e
	}
	err := s.deps.Store.Transact(ctx, storage.Guard{Now: now}, func(tx storage.Tx) error {
		out = protocol.KVCommitResult{}
		if err := s.applyKV(tx, p.Checks, nil, nil); err != nil {
			return err
		}
		for i, em := range p.Emissions {
			scope := domain.ID(em.ScopeID)
			var accepted domain.AcceptedEvent
			var added bool
			var err error
			if em.Parent == "" {
				accepted, added, err = runtime.AcceptInTx(tx, scope, events[i])
			} else {
				var parent domain.AcceptedEvent
				if parent, err = s.parentInTx(tx, scope, em.Parent); err != nil {
					return err
				}
				accepted, added, err = runtime.AcceptDerivedInTx(tx, now, s.limits(), scope, parent.Event.ID, events[i])
				if errors.Is(err, runtime.ErrCausalRejected) {
					// The host has recorded the refusal; the batch commits.
					out.Emissions = append(out.Emissions, protocol.EmissionResult{Refused: protocol.CodeCausalLimit})
					continue
				}
			}
			if err != nil {
				return err
			}
			out.Emissions = append(out.Emissions, protocol.EmissionResult{Event: protocol.FromAccepted(accepted), Added: added})
		}
		return s.applyKV(tx, nil, p.Puts, p.Deletes)
	})
	if err != nil {
		return protocol.KVCommitResult{}, err
	}
	for _, r := range out.Emissions {
		if r.Added {
			s.nudge()
			break
		}
	}
	return out, nil
}

// State reads, limited to configured scopes, and Work of the extension's
// own kinds wherever it is. A record outside them is not_found, so a read
// is never an existence oracle for other scopes.

func (s *Service) state(ctx context.Context, method string, p protocol.StateParams) (any, error) {
	allowed := s.allowedScopes()
	notFound := protocol.Errorf(protocol.CodeNotFound, "not found")
	var out any
	err := s.deps.Store.Transact(ctx, storage.Guard{Now: s.deps.Now()}, func(tx storage.Tx) error {
		switch method {
		case "state.work":
			w, err := tx.Work(domain.ID(p.ID))
			if err != nil {
				return err
			}
			if !allowed[w.ScopeID] && !s.ownKind(w.Kind) {
				return notFound
			}
			out = protocol.FromWork(w)
		case "state.works":
			if p.ScopeID != "" && !allowed[domain.ID(p.ScopeID)] {
				return unauthorized("scope is not configured for this extension")
			}
			works, err := tx.ListWorks()
			if err != nil {
				return err
			}
			list := []protocol.Work{}
			for _, w := range works {
				if (allowed[w.ScopeID] || s.ownKind(w.Kind)) && (p.ScopeID == "" || w.ScopeID == domain.ID(p.ScopeID)) {
					list = append(list, protocol.FromWork(w))
				}
			}
			out = list
		case "state.executions":
			w, err := tx.Work(domain.ID(p.ID))
			if err != nil {
				return err
			}
			if !allowed[w.ScopeID] && !s.ownKind(w.Kind) {
				return notFound
			}
			xs, err := tx.ListExecutions(w.ID)
			if err != nil {
				return err
			}
			list := []protocol.Execution{}
			for _, x := range xs {
				list = append(list, protocol.FromExecution(x))
			}
			out = list
		case "state.sequence":
			seq, err := tx.LatestSequence()
			if err != nil {
				return err
			}
			out = protocol.StateSequenceResult{Sequence: seq}
		case "state.event":
			a, err := tx.Event(domain.ID(p.ID))
			if err != nil {
				return err
			}
			scope, err := runtime.EventScopeInTx(tx, a)
			if err != nil || !allowed[scope] {
				return notFound
			}
			event := protocol.FromAccepted(a)
			event.ScopeID = string(scope)
			out = event
		case "state.events":
			// The allowed scopes' events from storage's scope index, with
			// those whose scope isn't recorded yet ("") resolved one by
			// one, so a page costs the extension's own events, not the log.
			scopes := []domain.ID{""}
			for id := range allowed {
				scopes = append(scopes, id)
			}
			list, err := scopedEvents(tx, allowed, p, func(after uint64, limit int) ([]domain.AcceptedEvent, error) {
				return tx.ScopeEvents(scopes, after, limit)
			})
			if err != nil {
				return err
			}
			out = list
		case "state.work_events":
			// One Work's events from the store's index, so the read follows
			// the Work's own events rather than the retained log. No Work
			// is looked up: an unknown or foreign Work's page is empty,
			// never an existence oracle.
			list := []protocol.AcceptedEvent{}
			if p.ID != "" {
				var err error
				list, err = scopedEvents(tx, allowed, p, func(after uint64, limit int) ([]domain.AcceptedEvent, error) {
					return tx.WorkEvents(domain.ID(p.ID), after, limit)
				})
				if err != nil {
					return err
				}
			}
			out = list
		case "state.scopes":
			if p.ID != "" {
				if !allowed[domain.ID(p.ID)] {
					return notFound
				}
				sc, err := tx.Scope(domain.ID(p.ID))
				if err != nil {
					return err
				}
				out = protocol.FromScope(sc)
				return nil
			}
			scopes, err := tx.ListScopes()
			if err != nil {
				return err
			}
			list := []protocol.Scope{}
			for _, sc := range scopes {
				if allowed[sc.ID] {
					list = append(list, protocol.FromScope(sc))
				}
			}
			out = list
		case "state.waits":
			w, err := tx.Work(domain.ID(p.ID))
			if err != nil {
				return err
			}
			if !allowed[w.ScopeID] {
				return notFound
			}
			waits, err := tx.ListWaits(w.ID)
			if err != nil {
				return err
			}
			list := []protocol.Wait{}
			for _, x := range waits {
				list = append(list, protocol.FromWait(x))
			}
			out = list
		case "state.pending":
			// The input for the Work that no attempt has read, queued or
			// accepted and not yet routed: the read that tells Work that
			// has read everything from Work about to start on something
			// new, both of which are ready.
			w, err := tx.Work(domain.ID(p.ID))
			if err != nil {
				return err
			}
			if !allowed[w.ScopeID] && !s.ownKind(w.Kind) {
				return notFound
			}
			unread, err := runtime.UnreadInputsInTx(tx, w.ID)
			if err != nil {
				return err
			}
			list := []protocol.PendingInput{}
			for _, q := range unread {
				list = append(list, protocol.PendingInput{EventID: string(q.EventID), Sequence: q.Sequence, Type: q.Type, Binding: q.Binding, Unrouted: q.Unrouted})
			}
			out = list
		case "state.timers":
			timers, err := tx.ListTimers()
			if err != nil {
				return err
			}
			list := []protocol.Timer{}
			for _, t := range timers {
				if s.ownFamily(t.Event.Type) {
					list = append(list, protocol.FromTimer(t))
				}
			}
			out = list
		}
		return nil
	})
	return out, err
}

// scopedEvents is one page of read (ScopeEvents or WorkEvents) after p.After,
// at most p.Limit (1000) of the allowed scopes' events. It filters by scope
// before the limit: it reads a batch at a time until the page is full or
// read runs out, so a page comes back short only at the end, however many
// other scopes' events sit between the extension's own.
func scopedEvents(tx storage.Tx, allowed map[domain.ID]bool, p protocol.StateParams, read func(after uint64, limit int) ([]domain.AcceptedEvent, error)) ([]protocol.AcceptedEvent, error) {
	limit := p.Limit
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}
	list := []protocol.AcceptedEvent{}
	for after := p.After; len(list) < limit; {
		events, err := read(after, limit)
		if err != nil {
			return nil, err
		}
		for _, a := range events {
			if len(list) == limit {
				break
			}
			if scope, err := runtime.EventScopeInTx(tx, a); err == nil && allowed[scope] {
				event := protocol.FromAccepted(a)
				event.ScopeID = string(scope)
				list = append(list, event)
			}
		}
		if len(events) < limit {
			break
		}
		after = events[len(events)-1].Sequence
	}
	return list, nil
}

func (s *Service) ownFamily(t string) bool {
	for _, f := range s.manifest.Provides.Families {
		if f.Type == t {
			return true
		}
	}
	return false
}

// Derived events.

func (s *Service) accept(ctx context.Context, p protocol.EventAcceptParams) (protocol.EventAcceptResult, error) {
	var out protocol.EventAcceptResult
	scope := domain.ID(p.ScopeID)
	if p.Parent == "" {
		return out, unauthorized("event.accept derives an Event; a root Event is an Emission of kv.commit")
	}
	now := s.deps.Now()
	e, err := s.emitted(p.ScopeID, p.Event, now)
	if err != nil {
		return out, err
	}
	if err := s.checkBatch(p.Checks, p.Records, p.Deletes); err != nil {
		return out, err
	}
	var refusal error
	err = s.deps.Store.Transact(ctx, storage.Guard{Now: now}, func(tx storage.Tx) error {
		parent, err := s.parentInTx(tx, scope, p.Parent)
		if err != nil {
			return err
		}
		if err := s.applyKV(tx, p.Checks, nil, nil); err != nil {
			return err
		}
		accepted, added, err := runtime.AcceptDerivedInTx(tx, now, s.limits(), scope, parent.Event.ID, e)
		if errors.Is(err, runtime.ErrCausalRejected) {
			refusal = protocol.Errorf(protocol.CodeCausalLimit, "causal depth or rate bound")
			return nil // commit only the redacted diagnostic
		}
		if err != nil {
			return err
		}
		out = protocol.EventAcceptResult{Event: protocol.FromAccepted(accepted), Added: added}
		if !added {
			return nil
		}
		return s.applyKV(tx, nil, p.Records, p.Deletes)
	})
	if err != nil {
		return protocol.EventAcceptResult{}, err
	}
	if refusal != nil {
		return protocol.EventAcceptResult{}, refusal
	}
	if out.Added {
		s.nudge()
	}
	return out, nil
}

// emitted checks an Event the extension emits into scope (event.accept or
// an Emission) and completes it: its own family, source namespace and
// payload schema, no causation or execution of its own, and the host's ID
// and receipt time.
func (s *Service) emitted(scopeID string, ev protocol.Event, now time.Time) (domain.Event, error) {
	if !s.allowedScopes()[domain.ID(scopeID)] {
		return domain.Event{}, unauthorized("scope is not configured for this extension")
	}
	e := ev.Domain()
	if !s.ownFamily(e.Type) {
		return domain.Event{}, protocol.Errorf(protocol.CodeUndeclared, "family not declared")
	}
	if !protocol.SourceNamespace(s.name, e.Source) || e.DeliveryID == "" {
		return domain.Event{}, unauthorized("source is outside the extension's namespace")
	}
	if e.CausationID != "" || e.ExecutionID != "" {
		return domain.Event{}, unauthorized("causation and execution are assigned by the host")
	}
	if err := s.manifest.CheckWrite(e.Data); err != nil {
		return domain.Event{}, protocol.Errorf(protocol.CodeSchema, err.Error())
	}
	if e.ReceivedAt.IsZero() {
		e.ReceivedAt = now
	}
	if e.OccurredAt.IsZero() {
		e.OccurredAt = e.ReceivedAt
	}
	e.ID = domain.ID(protocol.EmittedEventID(e.Source, e.DeliveryID))
	return e, nil
}

// parentInTx is an emitted Event's parent: an accepted Event in scope.
func (s *Service) parentInTx(tx storage.Tx, scope domain.ID, id string) (domain.AcceptedEvent, error) {
	parent, err := tx.Event(domain.ID(id))
	if err != nil {
		return parent, unauthorized("parent is not an accepted event")
	}
	if ps, err := runtime.EventScopeInTx(tx, parent); err != nil || ps != scope {
		return parent, unauthorized("parent is outside the scope")
	}
	return parent, nil
}

func (s *Service) limits() runtime.CausalLimits {
	if s.deps.CurrentLimits != nil {
		return s.deps.CurrentLimits()
	}
	return s.deps.Limits
}

func (s *Service) arm(ctx context.Context, p protocol.TimerArmParams) error {
	if s.deps.Timers == nil {
		return protocol.Errorf(protocol.CodeUnsupported, "timers")
	}
	scope := domain.ID(p.ScopeID)
	if !s.allowedScopes()[scope] {
		return unauthorized("scope is not configured for this extension")
	}
	if !slices.Contains(s.manifest.Provides.Recurrences, p.Kind) {
		return protocol.Errorf(protocol.CodeUndeclared, "recurrence not declared")
	}
	t := p.Timer.Domain()
	if !s.ownFamily(t.Event.Type) || !protocol.SourceNamespace(s.name, t.Event.Source) {
		return unauthorized("timer event is not the extension's")
	}
	if err := s.manifest.CheckWrite(t.Event.Data); err != nil {
		return protocol.Errorf(protocol.CodeSchema, err.Error())
	}
	if err := s.deps.Timers.ArmRecurrence(ctx, scope, t, domain.Revision(p.Expected), p.Kind, p.Spec); err != nil {
		return err
	}
	s.nudge()
	return nil
}

func (s *Service) nudge() {
	if s.deps.Nudge != nil {
		s.deps.Nudge()
	}
}

func (s *Service) cancelTimer(ctx context.Context, p protocol.TimerCancelParams) error {
	if s.deps.Timers == nil {
		return protocol.Errorf(protocol.CodeUnsupported, "timers")
	}
	var own bool
	err := s.deps.Store.Transact(ctx, storage.Guard{Now: s.deps.Now()}, func(tx storage.Tx) error {
		t, err := tx.Timer(domain.ID(p.ID))
		if errors.Is(err, storage.ErrNotFound) {
			own = true // cancelling an absent timer is a no-op
			return nil
		}
		own = err == nil && s.ownFamily(t.Event.Type)
		return err
	})
	if err != nil {
		return err
	}
	if !own {
		return unauthorized("timer is not the extension's")
	}
	return s.deps.Timers.Cancel(ctx, domain.ID(p.ID), domain.Revision(p.Expected))
}

func (s *Service) cancelWork(ctx context.Context, p protocol.WorkCancelParams) (protocol.WorkCancelResult, error) {
	if s.deps.CancelWork == nil {
		return protocol.WorkCancelResult{}, protocol.Errorf(protocol.CodeUnsupported, "work.cancel")
	}
	var w domain.Work
	err := s.deps.Store.Transact(ctx, storage.Guard{Now: s.deps.Now()}, func(tx storage.Tx) error {
		var err error
		w, err = tx.Work(domain.ID(p.WorkID))
		return err
	})
	if err != nil {
		return protocol.WorkCancelResult{}, err
	}
	if !s.ownKind(w.Kind) {
		return protocol.WorkCancelResult{}, unauthorized("work is not of the extension's kinds")
	}
	id, err := s.deps.CancelWork(ctx, w.ID, domain.Revision(p.Expected), p.Reason, s.name+"/"+p.Key)
	return protocol.WorkCancelResult{EventID: string(id)}, err
}

// Artifacts.

func (s *Service) artifact(ctx context.Context, method string, raw json.RawMessage) (any, error) {
	if s.deps.Artifacts == nil {
		return nil, protocol.Errorf(protocol.CodeUnsupported, "artifacts")
	}
	if method == "artifact.create" {
		var p protocol.ArtifactCreateParams
		if err := jsonrpc.Params(raw, &p); err != nil {
			return nil, err
		}
		w, err := s.deps.Artifacts.Create(ctx, p.MediaType)
		if err != nil {
			return nil, err
		}
		id := newID()
		s.mu.Lock()
		s.writers[id] = w
		s.mu.Unlock()
		return protocol.ArtifactCreateResult{Writer: id}, nil
	}
	var target protocol.ArtifactWriteParams
	if err := jsonrpc.Params(raw, &target); err != nil {
		return nil, err
	}
	s.mu.Lock()
	w := s.writers[target.Writer]
	if method != "artifact.write" {
		delete(s.writers, target.Writer)
	}
	s.mu.Unlock()
	if w == nil {
		return nil, protocol.Errorf(protocol.CodeNotFound, "unknown artifact writer")
	}
	switch method {
	case "artifact.write":
		if len(target.Chunk) > protocol.MaxChunkBytes {
			return nil, invalid("chunk exceeds the protocol bound")
		}
		_, err := w.Write(target.Chunk)
		return struct{}{}, err
	case "artifact.commit":
		ref, err := w.Commit(ctx)
		if err != nil {
			return nil, err
		}
		return protocol.ArtifactRef{ID: ref.ID, Size: ref.Size, MediaType: ref.MediaType}, nil
	}
	return struct{}{}, w.Close()
}

// abandonWriters aborts every open artifact writer (the process died).
func (s *Service) abandonWriters() {
	s.mu.Lock()
	writers := s.writers
	s.writers = map[string]storage.ArtifactWriter{}
	s.mu.Unlock()
	for _, w := range writers {
		_ = w.Close()
	}
}

func (s *Service) resource(ctx context.Context, method string, raw json.RawMessage) (any, error) {
	var p struct {
		RunID string `json:"run_id"`
	}
	if err := jsonrpc.Params(raw, &p); err != nil {
		return nil, err
	}
	s.mu.Lock()
	r := s.runs[p.RunID]
	s.mu.Unlock()
	if r == nil || r.closed {
		return nil, protocol.Errorf(protocol.CodeConflict, "run is not served by this process")
	}
	if r.resources == nil {
		return nil, protocol.Errorf(protocol.CodeUnsupported, "the binding declares no resource provider")
	}
	if method == "resource.lookup" {
		var lp protocol.ResourceLookupParams
		if err := jsonrpc.Params(raw, &lp); err != nil {
			return nil, err
		}
		res, err := r.resources.Lookup(ctx, lp.Ref)
		if err != nil {
			return nil, err
		}
		return protocol.ResourceResult{Resource: protocol.FromResource(res)}, nil
	}
	var ap protocol.ResourceAcquireParams
	if err := jsonrpc.Params(raw, &ap); err != nil {
		return nil, err
	}
	var log strings.Builder
	res, err := r.resources.Acquire(ctx, extension.ResourceRequest{Mode: ap.Mode, Params: ap.Params.Domain(), Log: &log})
	out := protocol.ResourceResult{Resource: protocol.FromResource(res), Log: log.String()}
	if errors.Is(err, extension.ErrResourceSetup) {
		out.SetupFailed = true
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	return out, nil
}

// confine serves a run's sandbox and container brokers: the provider the
// host bound to the attempt plans, launches and adopts, never the package.
// A run without one is refused, so a package can't confine itself to a
// provider its binding didn't name.
func (s *Service) confine(ctx context.Context, method string, raw json.RawMessage) (any, error) {
	var p struct {
		RunID string `json:"run_id"`
	}
	if err := jsonrpc.Params(raw, &p); err != nil {
		return nil, err
	}
	s.mu.Lock()
	r := s.runs[p.RunID]
	s.mu.Unlock()
	if r == nil || r.closed {
		return nil, protocol.Errorf(protocol.CodeConflict, "run is not served by this process")
	}
	switch method {
	case "sandbox.plan", "container.launch":
		var lp protocol.LaunchParams
		if err := jsonrpc.Params(raw, &lp); err != nil {
			return nil, err
		}
		var plan extension.SandboxPlan
		var err error
		if method == "sandbox.plan" {
			if r.sandbox == nil {
				return nil, protocol.Errorf(protocol.CodeUnsupported, "the binding declares no sandbox")
			}
			plan, err = r.sandbox.Plan(ctx, lp.Needs.Extension(), lp.Launch.Extension())
		} else {
			if r.container == nil {
				return nil, protocol.Errorf(protocol.CodeUnsupported, "the binding declares no container")
			}
			plan, err = r.container.Launch(ctx, lp.Needs.Extension(), lp.Launch.Extension())
		}
		if err != nil {
			return nil, err
		}
		return protocol.FromSandboxPlan(plan), nil
	case "sandbox.adopt":
		var ap protocol.SandboxAdoptParams
		if err := jsonrpc.Params(raw, &ap); err != nil {
			return nil, err
		}
		if r.sandbox == nil {
			return nil, protocol.Errorf(protocol.CodeUnsupported, "the binding declares no sandbox")
		}
		return struct{}{}, r.sandbox.Adopt(ctx, ap.Parent)
	default:
		var ap protocol.ContainerAdoptParams
		if err := jsonrpc.Params(raw, &ap); err != nil {
			return nil, err
		}
		if r.container == nil {
			return nil, protocol.Errorf(protocol.CodeUnsupported, "the binding declares no container")
		}
		exit, err := r.container.Adopt(ctx, ap.Alive)
		if err != nil {
			return nil, err
		}
		return protocol.ContainerExit{Exited: exit.Exited, ExitCode: exit.ExitCode, Interrupted: exit.Interrupted}, nil
	}
}

// LocalKV is the service's KV surface called in-process, without the wire:
// the in-process side of the shared extension KV suite.
type LocalKV struct{ S *Service }

func (k LocalKV) Get(ctx context.Context, namespace, key string) ([]byte, string, error) {
	out, err := k.S.kvGet(ctx, protocol.KVGetParams{Namespace: namespace, Key: key})
	if err != nil {
		return nil, "", err
	}
	if !out.Found {
		return nil, "", storage.ErrNotFound
	}
	return out.Entry.Value, out.Entry.Version, nil
}
func (k LocalKV) List(ctx context.Context, namespace, prefix string) ([]protocol.KVEntry, error) {
	out, err := k.S.kvList(ctx, protocol.KVListParams{Namespace: namespace, Prefix: prefix})
	return out.Entries, err
}
func (k LocalKV) Commit(ctx context.Context, b protocol.KVCommitParams) error {
	_, err := k.S.kvCommit(ctx, b)
	return err
}

// CommitBatch is Commit answering each Emission's outcome.
func (k LocalKV) CommitBatch(ctx context.Context, b protocol.KVCommitParams) (protocol.KVCommitResult, error) {
	return k.S.kvCommit(ctx, b)
}
