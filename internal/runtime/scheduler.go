package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/storage"
)

const attemptNamespace = "core.scheduler.attempts"

var ErrCapacity = errors.New("execution capacity exhausted")

// GlobalPool names the capacity pool GlobalLimit bounds in a CapacityError
// and in PoolUsage.
const GlobalPool = "global"

// CapacityError says which limit holds a start back. It matches ErrCapacity.
type CapacityError struct {
	// Pool is GlobalPool, a PoolLimits name, or "scope" for a scope limit
	// (Scope then names the scope).
	Pool         string
	Scope        domain.ID
	Running, Max int
}

func (e *CapacityError) Error() string {
	name := e.Pool
	if e.Scope != "" {
		name += " " + string(e.Scope)
	}
	return fmt.Sprintf("%s: %s limit %d reached (%d running)", ErrCapacity, name, e.Max, e.Running)
}
func (e *CapacityError) Is(target error) bool { return target == ErrCapacity }

var ErrResourceHeld = errors.New("previous attempt resource exclusion unresolved")
var ErrExecutorCall = errors.New("executor operation failed")
var ErrSecretUnavailable = errors.New("execution secrets unavailable")

// ErrEnvironmentUnavailable is an extension's run environment layer that
// could not be had (Options.Environment).
var ErrEnvironmentUnavailable = errors.New("run environment unavailable")
var ErrResourceUnavailable = errors.New("execution resource provider unavailable")

type SchedulerOptions struct {
	Owner               string
	LeaseDuration       time.Duration
	Generation          string
	Executors           map[string]extension.Executor
	HistoricalExecutors map[string]map[string]extension.Executor
	// GlobalLimit bounds running attempts whose executor has no pool in
	// ExecutorPools (default 4).
	GlobalLimit int
	// ExecutorPools gives an executor registration its own capacity pool,
	// bounded by PoolLimits[pool] (zero or absent: unbounded) instead of
	// GlobalLimit, so one kind of work cannot starve another (#13).
	ExecutorPools        map[string]string
	PoolLimits           map[string]int
	ScopeLimits          map[domain.ID]int
	Retry                RetryPolicy
	CausalLimits         CausalLimits
	AutoContinueHandoff  bool
	ControlErrorFallback map[string]string
	Timeout              time.Duration
	AuthorizeAlias       func(context.Context, AliasAuthorization) error
	// AliasFilter evaluates a subscription's opaque Filter with the filter its
	// namespace's extension declared (Toolbelt.DependencyFilter). It is pure
	// and runs inside coordination transactions. A subscription with a filter
	// and no AliasFilter never matches.
	AliasFilter func(namespace string, filter domain.Payload, e domain.Event) bool
	// Toolbelt issues a fresh per-epoch credential after claim/recovery/resume
	// for executors advertising a toolbelt transport other than none.
	Toolbelt *Toolbelt
	// ToolbeltEndpoints serves the execution its own toolbelt endpoint and
	// writes the attempt's fresh token into a token directory there, after
	// every claim, recovery and resume. It returns the endpoint and token
	// file the executor receives. It is required whenever Toolbelt is set:
	// the management socket refuses execution tokens, so there is no shared
	// endpoint to fall back to.
	ToolbeltEndpoints func(ctx context.Context, execution domain.ID, token string) (endpoint, tokenFile string, err error)
	// Secrets resolves only the attempt's binding-selected secrets immediately
	// before Run/Recover/Resume, outside coordination transactions. Values are
	// never persisted; failure blocks this attempt with a safe diagnostic.
	Secrets func(context.Context, AttemptState) (map[string]string, error)
	// Environment gathers the run environment layer the extensions of the
	// attempt's scope contribute (extension.RunEnvironment) before Run and
	// Resume, outside coordination transactions; a recovery adopts a
	// process that already has its environment. Values are never persisted;
	// a failure fails the attempt before the executor is called.
	Environment func(context.Context, AttemptState) (map[string]string, error)
	// Resources binds the attempt's optional resource provider immediately
	// before Run/Recover/Resume, outside coordination transactions. A nil
	// broker means the binding declares no resource. Core never inspects it.
	Resources func(context.Context, AttemptState) (extension.ResourceBroker, error)
	// Sandbox binds the attempt's optional process sandbox (B09) after
	// Resources, outside coordination transactions. It returns the broker
	// (nil: the binding declares none) and the resource broker the executor
	// gets in place of the one it was given. pin records a plan with the
	// attempt's open segment, fenced by the lease, before the plan reaches
	// the executor. Core never inspects either broker.
	Sandbox func(ctx context.Context, state AttemptState, resources extension.ResourceBroker, pin func(extension.SandboxPlan) error) (extension.SandboxBroker, extension.ResourceBroker, error)
	// Container binds the attempt's optional container after Resources,
	// as Sandbox does (a binding declares one or the other). pin records a
	// container with the open segment, fenced by the lease, before its
	// argv reaches the executor. Core never inspects the binding.
	Container func(ctx context.Context, state AttemptState, resources extension.ResourceBroker, pin func(extension.ContainerPin) error) (ContainerBinding, error)
	// Signalled is told, before the host asks an executor to interrupt an
	// execution, so a container's records hold every TERM the host sends.
	Signalled func(ctx context.Context, execution domain.ID, kind string)
	// KeptExecutions is the per-Work retention count shared by every executor
	// (B05 kept_executions, default 20). Artifacts receives content deletion
	// for expired attempts; nil keeps content and still records expiration.
	KeptExecutions int
	Artifacts      storage.Artifacts
	// WorkKinds are extension-registered lifecycle policies by Work kind
	// (05-01). Kinds without an entry keep the default lifecycle.
	WorkKinds map[string]WorkKindPolicy
}

// ContainerBinding is what the Container hook binds to an attempt: the
// broker the executor gets, the resource broker in place of the one it was
// given, and Settle, the host's last word on the segment's result (an
// unrequested TERM makes it interrupted). A nil Broker: no container.
type ContainerBinding struct {
	Broker    extension.ContainerBroker
	Resources extension.ResourceBroker
	Settle    func(extension.Result) extension.Result
	// Home is the Work's container home (ExecutionContext.ContainerHome).
	Home string
}

// WorkKindPolicy is the lifecycle an extension declares for the Work kind it
// creates (05-01, first used by scheduled jobs).
type WorkKindPolicy struct {
	// Standing Work is an objective that outlives its attempts: a completed
	// attempt leaves it ready and idle (it waits quietly for its next input,
	// as handed-back Work does) instead of completing it, and a failed attempt
	// arms no automatic retry, because the next routed input is its next
	// attempt. Only cancellation makes standing Work terminal.
	Standing bool
	// Trigger optionally selects which pending input triggers an attempt
	// from the acceptance-ordered batch the attempt takes (the attempt still
	// receives the whole batch as RunRequest.Inputs). It must be pure and
	// deterministic; an index outside the batch selects the first input. A
	// wake winner always takes precedence.
	Trigger func(inputs []domain.Event) int
}
type Scheduler struct {
	store   storage.Store
	now     func() time.Time
	router  *Router
	options SchedulerOptions
}
type Attempt struct {
	Lease   domain.LeaseRef
	Handle  extension.RunHandle
	Request extension.RunRequest
}
type AttemptState struct {
	ExecutionID, WorkID, ScopeID      domain.ID
	Generation, Executor, LaunchState string
	Binding                           domain.Binding
	// Grants are the binding grants the Work opted into that the binding
	// offered when the attempt started; Binding.Config has them applied.
	Grants               []string `json:",omitempty"`
	Recovery             domain.Payload
	RecoveryRecorded     bool
	Excluded             bool
	Epoch                uint64
	Result               *extension.Result
	Retry                RetryPolicy
	ControlErrorFallback string
	AutoContinueHandoff  bool
	Capabilities         domain.Capabilities
	Deadline             time.Time
	CurrentTriggerID     domain.ID
	CausalTriggerID      domain.ID
	WaitID, WakeEventID  domain.ID
	ObservedSequence     uint64
	InputEventIDs        []domain.ID
	RemainingTimeout     time.Duration
	Declaration          *ToolbeltDeclaration
	// Segments are the attempt's active stretches, the time it held a lease
	// as the deadline budget counts it: the first start (with the waking
	// event's ID when a wake started it), each resume after a wake, each
	// recovery by a new owner (no gap: recovery keeps the deadline), each
	// ended by a park. The last stays open while the attempt runs and ends
	// when the execution finishes. Only the last MaxSegments are kept; DroppedActive
	// is the active time of the ones dropped, DroppedSegments their count.
	Segments        []Segment     `json:",omitempty"`
	DroppedSegments int           `json:",omitempty"`
	DroppedActive   time.Duration `json:",omitempty"`
	// Attaches are the containers operators attached to the attempt's
	// session in after it ended (open-conduit container attach), each
	// pinned as a segment's container is. Only the last MaxAttaches are kept.
	Attaches []extension.ContainerPin `json:",omitempty"`
	// EarlierUsage sums the usage of the segments before the current one.
	// Result holds only the latest segment's result, so a resume, or a
	// timeout replacing a parked result, moves its usage here; Usage reports the two together as the attempt's usage.
	EarlierUsage *extension.Usage `json:",omitempty"`
}

// Usage is the attempt's usage across all its segments.
func (state AttemptState) Usage() extension.Usage {
	var u extension.Usage
	if state.EarlierUsage != nil {
		u = *state.EarlierUsage
	}
	if state.Result != nil {
		u = addUsage(u, state.Result.Usage)
	}
	return u
}

// carryUsage moves the finished segment's usage into EarlierUsage, so the
// resumed segment's result adds to it rather than replacing it.
func (state *AttemptState) carryUsage() {
	if state.Result == nil {
		return
	}
	u := state.Usage()
	if !usageEmpty(u) {
		state.EarlierUsage = &u
	}
	state.Result.Usage = extension.Usage{}
}

// addUsage sums two measurements. A count either side lacks is the other
// side's; costs in different currencies cannot be summed, so their sum is
// unknown.
func addUsage(a, b extension.Usage) extension.Usage {
	count := func(x, y *uint64) *uint64 {
		if x == nil {
			return y
		}
		if y == nil {
			return x
		}
		n := *x + *y
		return &n
	}
	out := extension.Usage{InputTokens: count(a.InputTokens, b.InputTokens), OutputTokens: count(a.OutputTokens, b.OutputTokens), CachedTokens: count(a.CachedTokens, b.CachedTokens)}
	switch {
	case a.Cost == nil:
		out.Cost, out.Currency = b.Cost, b.Currency
	case b.Cost == nil:
		out.Cost, out.Currency = a.Cost, a.Currency
	case a.Currency == b.Currency:
		c := *a.Cost + *b.Cost
		out.Cost, out.Currency = &c, a.Currency
	}
	return out
}

func usageEmpty(u extension.Usage) bool {
	return u.InputTokens == nil && u.OutputTokens == nil && u.CachedTokens == nil && u.Cost == nil
}

// MaxSegments bounds the segments an attempt keeps; a standing turn can
// resume hundreds of times.
const MaxSegments = 50

// Segment is one active stretch of an attempt. WakeEventID names the event
// that woke it; Parked says it ended in a park; Recovered says it began
// when a new owner recovered the attempt.
type Segment struct {
	StartedAt   time.Time
	EndedAt     *time.Time `json:",omitempty"`
	WakeEventID domain.ID  `json:",omitempty"`
	Parked      bool       `json:",omitempty"`
	Recovered   bool       `json:",omitempty"`
	// Sandbox is the plan the segment's process was launched under, when
	// its binding declared a sandbox (B09). A recovered segment carries the
	// plan of the process it adopted.
	Sandbox *SegmentSandbox `json:",omitempty"`
	// Container is the container the segment's process ran in, when its
	// binding declared one. A recovered segment carries the pin of the
	// container it adopted.
	Container *extension.ContainerPin `json:",omitempty"`
}

// SegmentSandbox pins a segment's sandbox: the plan's digest, and its
// summary for history.
type SegmentSandbox struct {
	Digest, Summary string
}

// SandboxPin is the sandbox plan the attempt's latest segment was launched
// under, or nil.
func (state AttemptState) SandboxPin() *SegmentSandbox {
	if n := len(state.Segments); n > 0 {
		return state.Segments[n-1].Sandbox
	}
	return nil
}

// ContainerPin is the container the attempt's latest segment was launched
// in, or nil.
func (state AttemptState) ContainerPin() *extension.ContainerPin {
	if n := len(state.Segments); n > 0 {
		return state.Segments[n-1].Container
	}
	return nil
}

// SegmentNumber is the open (or last) segment's number, counting those
// dropped beyond MaxSegments, from 1.
func (state AttemptState) SegmentNumber() int {
	return state.DroppedSegments + len(state.Segments)
}

// endSegment ends the open segment, if any, at end (never before it began).
func (state *AttemptState) endSegment(end time.Time, parked bool) {
	n := len(state.Segments)
	if n == 0 || state.Segments[n-1].EndedAt != nil {
		return
	}
	if end.Before(state.Segments[n-1].StartedAt) {
		end = state.Segments[n-1].StartedAt
	}
	state.Segments[n-1].EndedAt, state.Segments[n-1].Parked = &end, parked
}

// beginSegment appends a segment, dropping the oldest beyond MaxSegments
// into DroppedActive so the active time stays exact.
func (state *AttemptState) beginSegment(g Segment) {
	state.Segments = append(state.Segments, g)
	for len(state.Segments) > MaxSegments {
		if old := state.Segments[0]; old.EndedAt != nil {
			state.DroppedActive += old.EndedAt.Sub(old.StartedAt)
		}
		state.DroppedSegments++
		state.Segments = state.Segments[1:]
	}
}

func attemptState(tx storage.Tx, id domain.ID) (state AttemptState, err error) {
	err = getJSON(tx, attemptNamespace, identity(string(id)), &state)
	normalizePayload(&state.Recovery)
	normalizePayload(&state.Binding.Config)
	if state.Result != nil {
		normalizePayload(&state.Result.Outcome.Data)
	}
	if d := state.Declaration; d != nil {
		if d.Outcome != nil {
			normalizePayload(&d.Outcome.Data)
		}
		if d.Timer != nil {
			normalizePayload(&d.Timer.Event.Data)
		}
	}
	return
}
func normalizePayload(p *domain.Payload) {
	if p.Schema == "" && string(p.Data) == "null" {
		p.Data = nil
	}
}
func putAttemptState(tx storage.Tx, state AttemptState) error {
	return putJSON(tx, attemptNamespace, identity(string(state.ExecutionID)), state)
}
func NewScheduler(store storage.Store, now func() time.Time, router *Router, o SchedulerOptions) (*Scheduler, error) {
	if store == nil || now == nil || router == nil || o.Owner == "" || o.Generation == "" || o.GlobalLimit < 0 {
		return nil, storage.ErrInvalid
	}
	if o.Toolbelt != nil && o.ToolbeltEndpoints == nil {
		return nil, storage.ErrInvalid
	}
	if o.Timeout == 0 {
		o.Timeout = time.Hour
	}
	if o.Timeout < 0 || o.Timeout > time.Duration(2147483647)*time.Second {
		return nil, storage.ErrInvalid
	}
	if o.GlobalLimit == 0 {
		o.GlobalLimit = 4
	}
	if o.KeptExecutions == 0 {
		o.KeptExecutions = DefaultKeptExecutions
	}
	if o.KeptExecutions < 1 || o.KeptExecutions > 2147483647 {
		return nil, storage.ErrInvalid
	}
	if o.LeaseDuration == 0 {
		o.LeaseDuration = time.Minute
	}
	if o.LeaseDuration <= 0 || o.LeaseDuration > 24*time.Hour {
		return nil, storage.ErrInvalid
	}
	retry, err := normalizeRetryPolicy(o.Retry)
	if err != nil {
		return nil, err
	}
	o.Retry = retry
	kinds := map[string]WorkKindPolicy{}
	for kind, policy := range o.WorkKinds {
		if kind == "" {
			return nil, storage.ErrInvalid
		}
		kinds[kind] = policy
	}
	o.WorkKinds = kinds
	if _, err = normalizeCausalLimits(o.CausalLimits); err != nil {
		return nil, err
	}
	copyExecutors := func(src map[string]extension.Executor) (map[string]extension.Executor, error) {
		out := map[string]extension.Executor{}
		for name, e := range src {
			if name == "" || e == nil {
				return nil, extension.ErrInvalid
			}
			if err := extension.ValidateBinding(domain.Binding{Name: name}, e); err != nil {
				return nil, err
			}
			out[name] = e
		}
		return out, nil
	}
	o.Executors, err = copyExecutors(o.Executors)
	if err != nil {
		return nil, err
	}
	history := map[string]map[string]extension.Executor{}
	for generation, executors := range o.HistoricalExecutors {
		if generation == "" || generation == o.Generation {
			return nil, storage.ErrInvalid
		}
		history[generation], err = copyExecutors(executors)
		if err != nil {
			return nil, err
		}
	}
	o.HistoricalExecutors = history
	limits := map[domain.ID]int{}
	for scope, limit := range o.ScopeLimits {
		if scope == "" || limit < 1 {
			return nil, storage.ErrInvalid
		}
		limits[scope] = limit
	}
	o.ScopeLimits = limits
	pools := map[string]string{}
	for executor, pool := range o.ExecutorPools {
		if executor == "" || pool == "" || pool == GlobalPool {
			return nil, storage.ErrInvalid
		}
		pools[executor] = pool
	}
	o.ExecutorPools = pools
	poolLimits := map[string]int{}
	for pool, limit := range o.PoolLimits {
		if pool == "" || pool == GlobalPool || limit < 0 {
			return nil, storage.ErrInvalid
		}
		poolLimits[pool] = limit
	}
	o.PoolLimits = poolLimits
	fallbacks := map[string]string{}
	for name, fallback := range o.ControlErrorFallback {
		if fallback != "queue" && fallback != "drop" {
			return nil, extension.ErrInvalid
		}
		fallbacks[name] = fallback
	}
	o.ControlErrorFallback = fallbacks
	return &Scheduler{store, now, router, o}, nil
}
func (s *Scheduler) transact(ctx context.Context, ref *domain.LeaseRef, fn func(storage.Tx) error) error {
	return s.store.Transact(ctx, storage.Guard{Now: s.now(), Lease: ref}, fn)
}
func (s *Scheduler) executorFor(state AttemptState) (extension.Executor, error) {
	registry := s.options.Executors
	if state.Generation != s.options.Generation {
		registry = s.options.HistoricalExecutors[state.Generation]
	}
	e := registry[state.Executor]
	if e == nil {
		return nil, extension.ErrUnsupported
	}
	return e, nil
}
func (s *Scheduler) InspectAttempt(ctx context.Context, id domain.ID) (state AttemptState, err error) {
	err = s.transact(ctx, nil, func(tx storage.Tx) error { var e error; state, e = attemptState(tx, id); return e })
	return
}
func (s *Scheduler) Renew(ctx context.Context, ref domain.LeaseRef) error {
	return s.transact(ctx, &ref, func(tx storage.Tx) error { return tx.Renew(ref, s.options.LeaseDuration) })
}
func (s *Scheduler) Release(ctx context.Context, ref domain.LeaseRef) error {
	return s.transact(ctx, &ref, func(tx storage.Tx) error { return tx.Release(ref) })
}

func scopeChain(tx storage.Tx, id domain.ID) ([]domain.ID, error) {
	var chain []domain.ID
	seen := map[domain.ID]bool{}
	for id != "" {
		if seen[id] {
			return nil, storage.ErrInvalid
		}
		seen[id] = true
		scope, err := tx.Scope(id)
		if err != nil {
			return nil, err
		}
		chain = append(chain, id)
		id = scope.ParentID
	}
	return chain, nil
}

// pool is the capacity pool an executor's attempts count against.
func (s *Scheduler) pool(executor string) string {
	if p := s.options.ExecutorPools[executor]; p != "" {
		return p
	}
	return GlobalPool
}

// poolLimit is the limit of a pool; zero is unbounded.
func (s *Scheduler) poolLimit(pool string) int {
	if pool == GlobalPool {
		return s.options.GlobalLimit
	}
	return s.options.PoolLimits[pool]
}

// held lists the attempts holding capacity: every nonterminal, unparked
// attempt and every finished one without confirmed exclusion.
func held(tx storage.Tx, fn func(x domain.Execution, executor string) error) error {
	xs, err := tx.ListExecutions("")
	if err != nil {
		return err
	}
	for _, x := range xs {
		state, e := attemptState(tx, x.ID)
		if e != nil && !errors.Is(e, storage.ErrNotFound) {
			return e
		}
		holds := e == nil && !state.Excluded
		if !x.State.Terminal() && x.State != domain.ExecutionParked {
			holds = true
		}
		if !holds {
			continue
		}
		if err = fn(x, state.Executor); err != nil {
			return err
		}
	}
	return nil
}

func (s *Scheduler) checkCapacity(tx storage.Tx, work domain.Work, executor string, recovering domain.ID) error {
	pool := s.pool(executor)
	total := 0
	counts := map[domain.ID]int{}
	err := held(tx, func(x domain.Execution, other string) error {
		if x.ID == recovering {
			return nil
		}
		if x.WorkID == work.ID {
			return ErrResourceHeld
		}
		w, e := tx.Work(x.WorkID)
		if e != nil {
			return e
		}
		chain, e := scopeChain(tx, w.ScopeID)
		if e != nil {
			return e
		}
		if s.pool(other) == pool {
			total++
		}
		for _, scope := range chain {
			counts[scope]++
		}
		return nil
	})
	if err != nil {
		return err
	}
	if limit := s.poolLimit(pool); limit > 0 && total >= limit {
		return &CapacityError{Pool: pool, Running: total, Max: limit}
	}
	chain, err := scopeChain(tx, work.ScopeID)
	if err != nil {
		return err
	}
	for _, scope := range chain {
		if limit := s.options.ScopeLimits[scope]; limit > 0 && counts[scope] >= limit {
			return &CapacityError{Pool: "scope", Scope: scope, Running: counts[scope], Max: limit}
		}
	}
	return nil
}

// PoolUsage is one capacity pool's running attempts against its limit
// (zero: unbounded).
type PoolUsage struct {
	Pool           string
	Running, Limit int
}

// Capacity reports every pool's usage, GlobalPool first, then the
// configured pools by name.
func (s *Scheduler) Capacity(ctx context.Context) ([]PoolUsage, error) {
	counts := map[string]int{}
	err := s.transact(ctx, nil, func(tx storage.Tx) error {
		clear(counts)
		return held(tx, func(_ domain.Execution, executor string) error { counts[s.pool(executor)]++; return nil })
	})
	if err != nil {
		return nil, err
	}
	names := map[string]bool{}
	for _, p := range s.options.ExecutorPools {
		names[p] = true
	}
	for p := range s.options.PoolLimits {
		names[p] = true
	}
	sorted := make([]string, 0, len(names))
	for p := range names {
		sorted = append(sorted, p)
	}
	sort.Strings(sorted)
	out := []PoolUsage{{GlobalPool, counts[GlobalPool], s.options.GlobalLimit}}
	for _, p := range sorted {
		out = append(out, PoolUsage{p, counts[p], s.options.PoolLimits[p]})
	}
	return out, nil
}
func effectiveBinding(tx storage.Tx, scope domain.ID, name string) (domain.Binding, error) {
	seen := map[domain.ID]bool{}
	for scope != "" {
		if seen[scope] {
			return domain.Binding{}, storage.ErrInvalid
		}
		seen[scope] = true
		level, err := tx.Scope(scope)
		if err != nil {
			return domain.Binding{}, err
		}
		var found *domain.Binding
		for _, b := range level.Bindings {
			if b.Name == name {
				if found != nil {
					return domain.Binding{}, extension.ErrInvalid
				}
				copy := b
				found = &copy
			}
		}
		if found != nil {
			return *found, nil
		}
		scope = level.ParentID
	}
	return domain.Binding{}, extension.ErrInvalid
}
func (s *Scheduler) requestInTx(tx storage.Tx, state AttemptState, ref domain.LeaseRef, triggerOverride domain.ID) (req extension.RunRequest, err error) {
	req.Work, err = tx.Work(state.WorkID)
	if err != nil {
		return
	}
	req.Execution, err = tx.Execution(ref.ExecutionID)
	if err != nil {
		return
	}
	trigger := req.Execution.TriggerID
	if state.CurrentTriggerID != "" {
		trigger = state.CurrentTriggerID
	}
	if triggerOverride != "" {
		trigger = triggerOverride
	}
	a, err := tx.Event(trigger)
	if err != nil {
		return req, err
	}
	req.Trigger = a.Event
	for _, eventID := range state.InputEventIDs {
		input, er := tx.Event(eventID)
		if er != nil {
			return req, er
		}
		req.Inputs = append(req.Inputs, input.Event)
	}
	req.Context.Config = state.Binding.Config
	return req, nil
}
func (s *Scheduler) Start(ctx context.Context, id domain.ID) (attempt Attempt, err error) {
	var pinned domain.Binding
	var granted []string
	var scopeRevisions []domain.Scope
	var ready domain.Work
	var input QueuedInput
	var parked domain.ID
	var wake wakeReady
	// Resolve/validate plugin metadata outside the claim transaction.
	err = s.transact(ctx, nil, func(tx storage.Tx) error {
		var e error
		ready, e = tx.Work(id)
		if e != nil {
			return e
		}
		if ready.State != domain.WorkReady {
			return storage.ErrConflict
		}
		inputs, e := PendingInputsInTx(tx, id)
		if e != nil {
			return e
		}
		if len(inputs) == 0 {
			return storage.ErrInvalid
		}
		input = inputs[0]
		if pick := s.options.WorkKinds[ready.Kind].Trigger; pick != nil && len(inputs) > 1 {
			events := make([]domain.Event, 0, len(inputs))
			for _, in := range inputs {
				a, e := tx.Event(in.EventID)
				if e != nil {
					return e
				}
				events = append(events, a.Event)
			}
			if i := pick(events); i >= 0 && i < len(inputs) {
				input = inputs[i]
			}
		}
		wakeErr := getJSON(tx, wakeReadyNamespace, identity(string(id)), &wake)
		if wakeErr != nil && !errors.Is(wakeErr, storage.ErrNotFound) {
			return wakeErr
		}
		if wakeErr == nil {
			found := false
			for _, candidate := range inputs {
				if candidate.EventID == wake.EventID {
					input = candidate
					found = true
					break
				}
			}
			if !found {
				return storage.ErrConflict
			}
		}
		pinned, e = effectiveBinding(tx, ready.ScopeID, input.Binding)
		if e != nil {
			return e
		}
		// The Work's grants change the config it runs on; the toolbelt
		// grant reads the names from the attempt state.
		pinned, granted = pinned.Granted(ready.Grants)
		chain, e := scopeChain(tx, ready.ScopeID)
		if e != nil {
			return e
		}
		for _, scopeID := range chain {
			scope, e := tx.Scope(scopeID)
			if e != nil {
				return e
			}
			scopeRevisions = append(scopeRevisions, scope)
		}
		xs, e := tx.ListExecutions(id)
		if e != nil {
			return e
		}
		for _, x := range xs {
			if x.State == domain.ExecutionParked {
				parked = x.ID
			}
		}
		return nil
	})
	if err != nil {
		return attempt, err
	}
	var state AttemptState
	var executor extension.Executor
	if parked != "" {
		state, err = s.InspectAttempt(ctx, parked)
		if err != nil {
			return attempt, err
		}
		executor, err = s.executorFor(state)
		if err != nil {
			return attempt, err
		}
		if _, ok := executor.(extension.ResumeExecutor); !ok || !executor.Capabilities().Resume {
			return attempt, extension.ErrUnsupported
		}
	} else {
		state = AttemptState{WorkID: id, ScopeID: ready.ScopeID, Generation: s.options.Generation, Executor: pinned.Executor, Binding: pinned, Grants: granted, LaunchState: "launching", Retry: s.options.Retry, ControlErrorFallback: s.options.ControlErrorFallback[pinned.Executor], AutoContinueHandoff: s.options.AutoContinueHandoff}
		executor, err = s.executorFor(state)
		if err != nil {
			return attempt, err
		}
		if err = extension.ValidateBinding(pinned, executor); err != nil {
			return attempt, err
		}
		state.Capabilities = executor.Capabilities()
	}
	err = s.transact(ctx, nil, func(tx storage.Tx) error {
		current, e := tx.Work(id)
		if e != nil {
			return e
		}
		if !reflect.DeepEqual(current, ready) {
			return storage.ErrConflict
		}
		for _, old := range scopeRevisions {
			scope, e := tx.Scope(old.ID)
			if e != nil {
				return e
			}
			if !reflect.DeepEqual(old, scope) {
				return storage.ErrConflict
			}
		}
		if e = retryEligibleInTx(tx, id); e != nil {
			return e
		}
		if e = s.checkCapacity(tx, current, state.Executor, parked); e != nil {
			return e
		}
		inputs, e := PendingInputsInTx(tx, id)
		if e != nil {
			return e
		}
		foundInput := false
		for _, candidate := range inputs {
			if candidate == input {
				foundInput = true
			}
		}
		if !foundInput {
			return storage.ErrConflict
		}
		executionID := parked
		if executionID == "" {
			xs, e := tx.ListExecutions(id)
			if e != nil {
				return e
			}
			var latest uint32
			for _, x := range xs {
				if x.Attempt > latest {
					latest = x.Attempt
				}
			}
			if latest == ^uint32(0) {
				return storage.ErrInvalid
			}
			executionID = domain.ID(fmt.Sprintf("%s/attempt/%d", id, latest+1))
			x := domain.Execution{ID: executionID, WorkID: id, Attempt: latest + 1, State: domain.ExecutionPending, ExecutorBinding: pinned.Name, TriggerID: input.EventID, TraceID: identity(string(id), string(input.EventID))}
			if e = tx.PutExecution(x, 0); e != nil {
				return e
			}
		}
		ref, e := tx.Claim(executionID, s.options.Owner, s.options.LeaseDuration)
		if e != nil {
			return e
		}
		attempt.Lease = ref
		if parked == "" {
			// One deadline model for every executor (05-02): the binding's
			// own timeout when it sets one, else the runtime default.
			timeout := s.options.Timeout
			if seconds := state.Binding.TimeoutSeconds; seconds > 0 {
				timeout = time.Duration(seconds) * time.Second
			}
			state.Deadline = s.now().Add(timeout)
		} else {
			if state.RemainingTimeout <= 0 {
				return storage.ErrInvalid
			}
			state.Deadline = s.now().Add(state.RemainingTimeout)
		}
		state.InputEventIDs = make([]domain.ID, 0, len(inputs))
		for _, pending := range inputs {
			state.InputEventIDs = append(state.InputEventIDs, pending.EventID)
		}
		state.CurrentTriggerID = input.EventID
		if parked == "" {
			state.CausalTriggerID = input.EventID
		}
		if wake.EventID != "" {
			state.WakeEventID = wake.EventID
		}
		if e = tx.Delete(wakeReadyNamespace, identity(string(id))); e != nil {
			return e
		}
		state.ExecutionID, state.Epoch, state.Excluded = executionID, ref.Epoch, false
		state.LaunchState = "launching"
		if parked != "" {
			state.LaunchState = "resuming"
			state.carryUsage()
		}
		// A parked attempt from before segments were recorded stays without
		// them: one resume-to-finish segment would understate its active time.
		if parked == "" || len(state.Segments) > 0 {
			state.beginSegment(Segment{StartedAt: s.now(), WakeEventID: wake.EventID})
		}
		if input.Sequence > state.ObservedSequence {
			state.ObservedSequence = input.Sequence
		}
		if e = putAttemptState(tx, state); e != nil {
			return e
		}
		if e = ArmExecutionDeadlineInTx(tx, s.now(), state, ref); e != nil {
			return e
		}
		if _, e = ConsumeInputsInTx(tx, id, executionID); e != nil {
			return e
		}
		attempt.Request, e = s.requestInTx(tx, state, ref, input.EventID)
		return e
	})
	if err != nil {
		return Attempt{}, err
	}
	return s.launch(ctx, attempt, state, executor, parked != "", false)
}
func validRecovery(p domain.Payload) bool {
	if p.Schema == "" {
		return len(p.Data) == 0
	}
	return strings.Contains(p.Schema, ".") && json.Valid(p.Data) && len(p.Data) <= 8<<20
}
func (s *Scheduler) launch(ctx context.Context, attempt Attempt, state AttemptState, executor extension.Executor, resume, recovering bool) (Attempt, error) {
	if err := s.transact(ctx, &attempt.Lease, func(tx storage.Tx) error { return nil }); err != nil {
		return attempt, err
	}
	var persisted atomic.Bool
	attempt.Request.PersistRecovery = func(payload domain.Payload) error {
		if !validRecovery(payload) {
			return storage.ErrInvalid
		}
		err := s.transact(ctx, &attempt.Lease, func(tx storage.Tx) error {
			current, e := attemptState(tx, state.ExecutionID)
			if e != nil {
				return e
			}
			current.Recovery = payload
			current.RecoveryRecorded = true
			return putAttemptState(tx, current)
		})
		if err == nil {
			persisted.Store(true)
		}
		return err
	}
	var handle extension.RunHandle
	var err error
	if s.options.Toolbelt != nil && state.Capabilities.Toolbelt != "" && state.Capabilities.Toolbelt != extension.ToolbeltNone {
		token, issueErr := s.options.Toolbelt.issueForAttempt(ctx, attempt.Lease, state)
		if issueErr != nil {
			failure := extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeFailed, Summary: "toolbelt authority unavailable"}}
			return attempt, errors.Join(extension.ErrUnauthorized, s.fail(ctx, attempt.Lease, failure, !recovering, nil))
		}
		endpoint, tokenFile, serveErr := s.options.ToolbeltEndpoints(ctx, state.ExecutionID, token)
		if serveErr != nil {
			failure := extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeFailed, Summary: "toolbelt endpoint unavailable"}}
			return attempt, errors.Join(extension.ErrUnauthorized, serveErr, s.fail(ctx, attempt.Lease, failure, !recovering, nil))
		}
		attempt.Request.Context.ToolbeltEndpoint, attempt.Request.Context.ToolbeltToken, attempt.Request.Context.ToolbeltTokenFile = endpoint, token, tokenFile
	}
	if s.options.Secrets != nil {
		secrets, secretErr := s.options.Secrets(ctx, state)
		if secretErr != nil {
			failure := extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeFailed, Summary: "secret resolution failed"}}
			return attempt, errors.Join(ErrSecretUnavailable, s.fail(ctx, attempt.Lease, failure, !recovering, nil))
		}
		attempt.Request.Context.Secrets = secrets
	}
	if s.options.Environment != nil && !recovering {
		env, envErr := s.options.Environment(ctx, state)
		if envErr != nil {
			failure := extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeFailed, Summary: "run environment unavailable"}}
			return attempt, errors.Join(ErrEnvironmentUnavailable, envErr, s.fail(ctx, attempt.Lease, failure, true, nil))
		}
		attempt.Request.Context.Env = env
	}
	if s.options.Resources != nil {
		broker, resourceErr := s.options.Resources(ctx, state)
		if resourceErr != nil {
			failure := extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeFailed, Summary: "resource provider unavailable"}}
			return attempt, errors.Join(ErrResourceUnavailable, s.fail(ctx, attempt.Lease, failure, !recovering, nil))
		}
		attempt.Request.Context.Resources = broker
	}
	if s.options.Sandbox != nil {
		pin := func(plan extension.SandboxPlan) error {
			return s.transact(ctx, &attempt.Lease, func(tx storage.Tx) error {
				current, e := attemptState(tx, state.ExecutionID)
				if e != nil {
					return e
				}
				n := len(current.Segments)
				if n == 0 || current.Segments[n-1].EndedAt != nil {
					return storage.ErrInvalid
				}
				current.Segments[n-1].Sandbox = &SegmentSandbox{Digest: plan.Digest, Summary: plan.Summary}
				return putAttemptState(tx, current)
			})
		}
		// The hook is told a recovery as the state now records it: the
		// recovered segment adopts the process its pin was made for.
		bound := state
		if recovering {
			bound.LaunchState = "recovering"
		}
		sandbox, resources, sandboxErr := s.options.Sandbox(ctx, bound, attempt.Request.Context.Resources, pin)
		if sandboxErr != nil {
			failure := extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeFailed, Summary: "sandbox unavailable"}}
			return attempt, errors.Join(extension.ErrSandboxUnavailable, sandboxErr, s.fail(ctx, attempt.Lease, failure, !recovering, nil))
		}
		attempt.Request.Context.Sandbox, attempt.Request.Context.Resources = sandbox, resources
	}
	var settle func(extension.Result) extension.Result
	if s.options.Container != nil {
		pin := func(c extension.ContainerPin) error {
			return s.transact(ctx, &attempt.Lease, func(tx storage.Tx) error {
				current, e := attemptState(tx, state.ExecutionID)
				if e != nil {
					return e
				}
				n := len(current.Segments)
				if n == 0 || current.Segments[n-1].EndedAt != nil {
					return storage.ErrInvalid
				}
				current.Segments[n-1].Container = &c
				return putAttemptState(tx, current)
			})
		}
		bound := state
		if recovering {
			bound.LaunchState = "recovering"
		}
		c, containerErr := s.options.Container(ctx, bound, attempt.Request.Context.Resources, pin)
		if containerErr != nil {
			failure := extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeFailed, Summary: "container unavailable"}}
			return attempt, errors.Join(extension.ErrSandboxUnavailable, containerErr, s.fail(ctx, attempt.Lease, failure, !recovering, nil))
		}
		if c.Broker != nil {
			attempt.Request.Context.Container, attempt.Request.Context.Resources, settle = c.Broker, c.Resources, c.Settle
			attempt.Request.Context.ContainerHome = c.Home
		}
	}
	if recovering {
		handle, err = executor.Recover(ctx, attempt.Request, state.Recovery)
	} else if resume {
		handle, err = executor.(extension.ResumeExecutor).Resume(ctx, attempt.Request, state.Recovery)
	} else {
		handle, err = executor.Run(ctx, attempt.Request)
	}
	if err != nil {
		failure := extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeFailed, Summary: "executor operation failed"}}
		commitErr := s.fail(ctx, attempt.Lease, failure, false, err)
		if errors.Is(err, extension.ErrNotAdoptable) {
			return attempt, errors.Join(extension.ErrNotAdoptable, commitErr)
		}
		return attempt, errors.Join(ErrExecutorCall, commitErr)
	}
	if handle == nil || !persisted.Load() {
		commitErr := s.fail(ctx, attempt.Lease, extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeFailed, Summary: "executor did not durably acknowledge launch"}}, false, nil)
		return attempt, errors.Join(extension.ErrInvalid, commitErr)
	}
	err = s.transact(ctx, &attempt.Lease, func(tx storage.Tx) error {
		current, e := attemptState(tx, state.ExecutionID)
		if e != nil {
			return e
		}
		current.LaunchState = "running"
		return putAttemptState(tx, current)
	})
	if err != nil {
		return attempt, err
	}
	if settle != nil {
		handle = settledHandle{handle, settle}
	}
	attempt.Handle = handle
	return attempt, nil
}

// settledHandle passes a container segment's result through the host's
// Settle before it is completed.
type settledHandle struct {
	extension.RunHandle
	settle func(extension.Result) extension.Result
}

func (h settledHandle) Wait(ctx context.Context) (extension.Result, error) {
	r, err := h.RunHandle.Wait(ctx)
	if err != nil {
		return r, err
	}
	return h.settle(r), nil
}
func (s *Scheduler) Recover(ctx context.Context, id domain.ID) (attempt Attempt, err error) {
	state, err := s.InspectAttempt(ctx, id)
	if err != nil {
		return attempt, err
	}
	executor, err := s.executorFor(state)
	if err != nil {
		return attempt, err
	}
	err = s.transact(ctx, nil, func(tx storage.Tx) error {
		current, e := attemptState(tx, id)
		if e != nil {
			return e
		}
		if current.Epoch != state.Epoch {
			return storage.ErrConflict
		}
		execution, e := tx.Execution(id)
		if e != nil {
			return e
		}
		if execution.State != domain.ExecutionRunning {
			return storage.ErrInvalid
		}
		// A segment is lease-held time, measured as the deadline budget
		// measures it: recovery keeps the deadline, so the lost-lease gap
		// stays in the segment it interrupted. The split only marks where a
		// new owner took over.
		if n := len(current.Segments); n > 0 && current.Segments[n-1].EndedAt == nil {
			now := s.now()
			pin, container := current.Segments[n-1].Sandbox, current.Segments[n-1].Container
			current.endSegment(now, false)
			current.beginSegment(Segment{StartedAt: now, Recovered: true, Sandbox: pin, Container: container})
		}
		ref, e := tx.Claim(id, s.options.Owner, s.options.LeaseDuration)
		if e != nil {
			return e
		}
		attempt.Lease = ref
		current.Epoch, current.Excluded, current.LaunchState = ref.Epoch, false, "recovering"
		// An unapplied toolbelt declaration belongs to the segment being
		// adopted (Recover never relaunches, and wait registration clears it
		// before any resume). A crash between the declaration and the host's
		// handshake must not drop it, so it moves to the recovering epoch.
		if current.Declaration != nil {
			current.Declaration.Epoch = ref.Epoch
		}
		if e = putAttemptState(tx, current); e != nil {
			return e
		}
		if e = ArmExecutionDeadlineInTx(tx, s.now(), current, ref); e != nil {
			return e
		}
		attempt.Request, e = s.requestInTx(tx, current, ref, "")
		return e
	})
	if err != nil {
		return Attempt{}, err
	}
	return s.launch(ctx, attempt, state, executor, false, true)
}
func (s *Scheduler) Observe(ctx context.Context, attempt Attempt) error {
	if attempt.Handle == nil {
		return storage.ErrInvalid
	}
	result, err := attempt.Handle.Wait(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.Join(ErrExecutorCall, s.fail(ctx, attempt.Lease, extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeFailed, Summary: "executor observation failed"}}, false, err))
	}
	return s.Complete(ctx, attempt.Lease, result)
}
func (s *Scheduler) fail(ctx context.Context, ref domain.LeaseRef, result extension.Result, excluded bool, cause error) error {
	var hint *RetryHint
	var provider RetryHintProvider
	if errors.As(cause, &provider) {
		value := provider.RetryHint()
		hint = &value
		if _, err := RetryDelay(s.now(), value); err != nil {
			return err
		}
	}
	return s.transact(ctx, &ref, func(tx storage.Tx) error {
		x, err := tx.Execution(ref.ExecutionID)
		if err != nil {
			return err
		}
		state, err := attemptState(tx, x.ID)
		if err != nil {
			return err
		}
		if err = CancelExecutionDeadlineInTx(tx, x.ID); err != nil {
			return err
		}
		state.Excluded = excluded
		state.LaunchState = "failed"
		state.Result = &result
		if err = putAttemptState(tx, state); err != nil {
			return err
		}
		// A failed attempt's proved injections were taken in like its
		// claimed inputs: the retry does not see them again either.
		if err = consumeInjectedInTx(tx, x, result.ConsumedInputs); err != nil {
			return err
		}
		if hint != nil {
			return s.failureWithHintInTx(tx, x, result, s.now(), hint)
		}
		return s.failureInTx(tx, x, result, s.now())
	})
}
func (s *Scheduler) Fail(ctx context.Context, ref domain.LeaseRef, result extension.Result) error {
	if result.Outcome.Kind != domain.OutcomeFailed {
		return extension.ErrInvalid
	}
	if err := extension.ValidateResult(result); err != nil {
		return err
	}
	var cause error
	if result.RetryHint != nil {
		cause = resultHint{*result.RetryHint}
	}
	return s.fail(ctx, ref, result, true, cause)
}

// resultHint presents a structured result's retry hint to the failure path
// exactly as an executor error's RetryHintProvider would be.
type resultHint struct{ hint RetryHint }

func (h resultHint) Error() string        { return "executor retry hint" }
func (h resultHint) RetryHint() RetryHint { return h.hint }
func (s *Scheduler) Complete(ctx context.Context, ref domain.LeaseRef, result extension.Result) error {
	if err := extension.ValidateResult(result); err != nil {
		return err
	}
	var declared *ToolbeltDeclaration
	if err := s.transact(ctx, nil, func(tx storage.Tx) error { var e error; declared, e = declarationInTx(tx, ref); return e }); err != nil {
		return err
	}
	if declared != nil && declared.Wait != nil {
		// The finished/suspended segment is the trusted handshake that turns
		// the earlier toolbelt wait request into durable registration.
		result.Outcome = domain.Outcome{Kind: domain.OutcomeWaiting, Summary: result.Outcome.Summary, Data: result.Outcome.Data}
		if err := extension.ValidateResult(result); err != nil {
			return err
		}
		if err := s.registerDeclaredWait(ctx, ref, *declared, result); err != nil {
			return err
		}
		if len(declared.Subscriptions) > 0 {
			// Best effort: a failure leaves the durable pending entry for the
			// next ReconcileSubscriptions pass.
			_ = s.installPendingSubscriptions(ctx, 20)
		}
		return nil
	}
	if declared != nil && declared.Outcome != nil {
		result.Outcome = *declared.Outcome
	}
	if result.Outcome.Kind == domain.OutcomeWaiting {
		return s.acknowledgeWaiting(ctx, ref, result)
	}
	if result.Outcome.Kind == domain.OutcomeFailed {
		return s.Fail(ctx, ref, result)
	}
	return s.transact(ctx, &ref, func(tx storage.Tx) error {
		x, err := tx.Execution(ref.ExecutionID)
		if err != nil {
			return err
		}
		w, err := tx.Work(x.WorkID)
		if err != nil {
			return err
		}
		if result.Outcome.Kind == domain.OutcomeCompleted {
			events, err := tx.UnroutedEvents(0, 0)
			if err != nil {
				return err
			}
			for _, a := range events {
				if a.Routed {
					continue
				}
				target, e := potentiallyTargets(tx, a.Event, w)
				if e != nil {
					return e
				}
				if target {
					return storage.ErrConflict
				}
			}
		}
		if err = CancelExecutionDeadlineInTx(tx, x.ID); err != nil {
			return err
		}
		x.State = domain.ExecutionCompleted
		if result.Outcome.Kind == domain.OutcomeHandoff {
			x.State = domain.ExecutionHandoff
		}
		x.Outcome = &result.Outcome
		x.Artifacts = append([]domain.ArtifactRef(nil), result.Artifacts...)
		if err = tx.PutExecution(x, x.Revision); err != nil {
			return err
		}
		state, err := attemptState(tx, x.ID)
		if err != nil {
			return err
		}
		state.Excluded = true
		state.LaunchState = string(x.State)
		state.Result = &result
		if err = putAttemptState(tx, state); err != nil {
			return err
		}
		if err = consumeInjectedInTx(tx, x, result.ConsumedInputs); err != nil {
			return err
		}
		pending, err := PendingInputsInTx(tx, w.ID)
		if err != nil {
			return err
		}
		w.State = domain.WorkReady
		if result.Outcome.Kind == domain.OutcomeCompleted && len(pending) == 0 && !s.options.WorkKinds[w.Kind].Standing {
			w.State = domain.WorkCompleted
		}
		if err = tx.PutWork(w, w.Revision); err != nil {
			return err
		}
		if err = resetRetryInTx(tx, w.ID); err != nil {
			return err
		}
		if err = s.turnEndedInTx(tx, x, s.now()); err != nil {
			return err
		}
		if result.Outcome.Kind == domain.OutcomeHandoff && state.AutoContinueHandoff {
			return s.handoffContinuationInTx(tx, x, s.now())
		}
		return nil
	})
}
func (s *Scheduler) Handoff(ctx context.Context, ref domain.LeaseRef, outcome domain.Outcome) error {
	if outcome.Kind != domain.OutcomeHandoff {
		return extension.ErrInvalid
	}
	return s.Complete(ctx, ref, extension.Result{Outcome: outcome})
}
func (s *Scheduler) Timeout(ctx context.Context, ref domain.LeaseRef) error {
	return s.transact(ctx, &ref, func(tx storage.Tx) error {
		state, err := attemptState(tx, ref.ExecutionID)
		if err != nil {
			return err
		}
		if state.Deadline.IsZero() || s.now().Before(state.Deadline) {
			return storage.ErrInvalid
		}
		x, err := tx.Execution(ref.ExecutionID)
		if err != nil {
			return err
		}
		if err = CancelExecutionDeadlineInTx(tx, x.ID); err != nil {
			return err
		}
		result := extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeFailed, Summary: "execution timed out"}}
		state.Excluded = false
		state.LaunchState = "timed_out"
		state.carryUsage()
		state.Result = &result
		if err = putAttemptState(tx, state); err != nil {
			return err
		}
		return s.failureInTx(tx, x, result, s.now())
	})
}
func (s *Scheduler) Cancel(ctx context.Context, id domain.ID) error { return s.router.Cancel(ctx, id) }

// ConfirmStopped is trusted host authority after verified process/resource
// exclusion, never an executor-facing acknowledgement. The expected epoch fences
// a delayed confirmation from a process that has since been adopted again.
func (s *Scheduler) ConfirmStopped(ctx context.Context, id domain.ID, expectedEpoch uint64) error {
	return s.transact(ctx, nil, func(tx storage.Tx) error {
		state, err := attemptState(tx, id)
		if err != nil {
			return err
		}
		if state.Epoch != expectedEpoch {
			return storage.ErrConflict
		}
		x, err := tx.Execution(id)
		if err != nil {
			return err
		}
		if !x.State.Terminal() {
			return storage.ErrInvalid
		}
		state.Excluded = true
		return putAttemptState(tx, state)
	})
}
