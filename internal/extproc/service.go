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
	// of a connection one of its entries names. The host refuses any other
	// name (B05 isolation).
	Secrets func(ctx context.Context, scope domain.ID, connection string, names []string) (map[string]string, error)
	// CurrentLimits, when set, supplies the causal limits of the current
	// configuration generation instead of Limits.
	CurrentLimits func() runtime.CausalLimits
	// Deliver is the runtime ingress push for one of the extension's own
	// registered sources; it returns after durable acceptance.
	Deliver    func(ctx context.Context, name string, d extension.Delivery) ([]domain.AcceptedEvent, error)
	Artifacts  storage.Artifacts
	Timers     Armer
	CancelWork func(ctx context.Context, work domain.ID, expected domain.Revision, reason, key string) (domain.ID, error)
}

// run is the host side of one executor call: the trusted, lease-fenced
// PersistRecovery and resource broker of that RunRequest. Runs are
// addressed by an unguessable ID the host assigns per call; a closed run
// refuses persistence, so a restarted or stale process cannot persist for
// an attempt the host no longer serves through it.
type run struct {
	persist   func(domain.Payload) error
	resources extension.ResourceBroker
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
	s := &Service{name: m.Name, manifest: m, enabled: map[string]bool{}, deps: deps, inTx: func() bool { return false }, runs: map[string]*run{}, writers: map[string]storage.ArtifactWriter{}, sources: map[string]bool{}, reconciles: map[string]extension.ResourceStatusFunc{}, setups: map[string]extension.SetupHost{}}
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

var storeMethods = map[string]bool{"kv.get": true, "kv.list": true, "kv.commit": true, "state.work": true, "state.works": true, "state.executions": true, "state.event": true, "state.events": true, "state.scopes": true, "state.timers": true, "event.accept": true, "timer.cancel": true, "work.cancel": true}

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
		return struct{}{}, s.kvCommit(ctx, p)
	case "state.work", "state.works", "state.executions", "state.event", "state.events", "state.scopes", "state.timers":
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
		accepted, err := s.deps.Deliver(ctx, p.Name, p.Delivery.Extension())
		if err != nil {
			return nil, err
		}
		return protocol.IngressDeliverResult{Accepted: protocol.FromAcceptedList(accepted)}, nil
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
	case "setup.print", "setup.prompt", "setup.choose", "setup.open_url", "setup.store_secret", "setup.look_path", "setup.run_command":
		return s.setup(ctx, method, raw)
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

func (s *Service) kvCommit(ctx context.Context, p protocol.KVCommitParams) error {
	if err := s.checkBatch(p.Checks, p.Puts, p.Deletes); err != nil {
		return err
	}
	if len(p.Puts)+len(p.Deletes) == 0 {
		return invalid("empty batch")
	}
	return s.deps.Store.Transact(ctx, storage.Guard{Now: s.deps.Now()}, func(tx storage.Tx) error {
		return s.applyKV(tx, p.Checks, p.Puts, p.Deletes)
	})
}

// State reads, limited to configured scopes. A record outside them is
// not_found, so a read is never an existence oracle for other scopes.

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
			if !allowed[w.ScopeID] {
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
				if allowed[w.ScopeID] && (p.ScopeID == "" || w.ScopeID == domain.ID(p.ScopeID)) {
					list = append(list, protocol.FromWork(w))
				}
			}
			out = list
		case "state.executions":
			w, err := tx.Work(domain.ID(p.ID))
			if err != nil {
				return err
			}
			if !allowed[w.ScopeID] {
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
		case "state.event":
			a, err := tx.Event(domain.ID(p.ID))
			if err != nil {
				return err
			}
			scope, err := runtime.EventScopeInTx(tx, a)
			if err != nil || !allowed[scope] {
				return notFound
			}
			out = protocol.FromAccepted(a)
		case "state.events":
			limit := p.Limit
			if limit <= 0 || limit > 1000 {
				limit = 1000
			}
			events, err := tx.Events(p.After, limit)
			if err != nil {
				return err
			}
			list := []protocol.AcceptedEvent{}
			for _, a := range events {
				if scope, err := runtime.EventScopeInTx(tx, a); err == nil && allowed[scope] {
					list = append(list, protocol.FromAccepted(a))
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
	if !s.allowedScopes()[scope] {
		return out, unauthorized("scope is not configured for this extension")
	}
	if p.Parent == "" {
		return out, unauthorized("a subprocess never mints a causal root outside ingress")
	}
	e := p.Event.Domain()
	if !s.ownFamily(e.Type) {
		return out, protocol.Errorf(protocol.CodeUndeclared, "family not declared")
	}
	if !protocol.SourceNamespace(s.name, e.Source) || e.DeliveryID == "" {
		return out, unauthorized("source is outside the extension's namespace")
	}
	if e.CausationID != "" || e.ExecutionID != "" {
		return out, unauthorized("causation and execution are assigned by the host")
	}
	if err := s.manifest.CheckWrite(e.Data); err != nil {
		return out, protocol.Errorf(protocol.CodeSchema, err.Error())
	}
	if err := s.checkBatch(p.Checks, p.Records, p.Deletes); err != nil {
		return out, err
	}
	now := s.deps.Now()
	if e.ReceivedAt.IsZero() {
		e.ReceivedAt = now
	}
	if e.OccurredAt.IsZero() {
		e.OccurredAt = e.ReceivedAt
	}
	sum := sha256.Sum256([]byte(e.Source + "\x00" + e.DeliveryID))
	e.ID = domain.ID("ext/" + hex.EncodeToString(sum[:16]))
	var refusal error
	err := s.deps.Store.Transact(ctx, storage.Guard{Now: now}, func(tx storage.Tx) error {
		parent, err := tx.Event(domain.ID(p.Parent))
		if err != nil {
			return unauthorized("parent is not an accepted event")
		}
		if ps, err := runtime.EventScopeInTx(tx, parent); err != nil || ps != scope {
			return unauthorized("parent is outside the scope")
		}
		if err := s.applyKV(tx, p.Checks, nil, nil); err != nil {
			return err
		}
		limits := s.deps.Limits
		if s.deps.CurrentLimits != nil {
			limits = s.deps.CurrentLimits()
		}
		accepted, added, err := runtime.AcceptDerivedInTx(tx, now, limits, scope, parent.Event.ID, e)
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
	return out, nil
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
	return s.deps.Timers.ArmRecurrence(ctx, scope, t, domain.Revision(p.Expected), p.Kind, p.Spec)
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
	allowed := s.allowedScopes()
	var w domain.Work
	err := s.deps.Store.Transact(ctx, storage.Guard{Now: s.deps.Now()}, func(tx storage.Tx) error {
		var err error
		w, err = tx.Work(domain.ID(p.WorkID))
		return err
	})
	if err != nil {
		return protocol.WorkCancelResult{}, err
	}
	own := false
	for _, k := range s.manifest.Provides.WorkKinds {
		own = own || k.Kind == w.Kind
	}
	if !allowed[w.ScopeID] || !own {
		return protocol.WorkCancelResult{}, unauthorized("work is not of the extension's kinds in its scopes")
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
	return k.S.kvCommit(ctx, b)
}
