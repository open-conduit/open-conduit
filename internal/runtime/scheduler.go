package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync/atomic"
	"time"

	"github.com/alsiola/open-conduit/domain"
	"github.com/alsiola/open-conduit/extension"
	"github.com/alsiola/open-conduit/storage"
)

const attemptNamespace = "core.scheduler.attempts"

var ErrCapacity = errors.New("execution capacity exhausted")
var ErrResourceHeld = errors.New("previous attempt resource exclusion unresolved")
var ErrExecutorCall = errors.New("executor operation failed")
var ErrSecretUnavailable = errors.New("execution secrets unavailable")

type SchedulerOptions struct {
	Owner                string
	LeaseDuration        time.Duration
	Generation           string
	Executors            map[string]extension.Executor
	HistoricalExecutors  map[string]map[string]extension.Executor
	GlobalLimit          int
	ScopeLimits          map[domain.ID]int
	Retry                RetryPolicy
	CausalLimits         CausalLimits
	AutoContinueHandoff  bool
	ControlErrorFallback map[string]string
	Timeout              time.Duration
	AuthorizeAlias       func(context.Context, AliasAuthorization) error
	// Toolbelt issues a fresh per-epoch credential after claim/recovery/resume
	// for executors advertising a toolbelt transport other than none.
	Toolbelt         *Toolbelt
	ToolbeltEndpoint string
	// Secrets resolves only the attempt's binding-selected secrets immediately
	// before Run/Recover/Resume, outside coordination transactions. Values are
	// never persisted; failure blocks this attempt with a safe diagnostic.
	Secrets func(context.Context, AttemptState) (map[string]string, error)
	// KeptExecutions is the per-Work retention count shared by every executor
	// (B05 kept_executions, default 20). Artifacts receives content deletion
	// for expired attempts; nil keeps content and still records expiration.
	KeptExecutions int
	Artifacts      storage.Artifacts
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
	Recovery                          domain.Payload
	RecoveryRecorded                  bool
	Excluded                          bool
	Epoch                             uint64
	Result                            *extension.Result
	Retry                             RetryPolicy
	ControlErrorFallback              string
	AutoContinueHandoff               bool
	Capabilities                      domain.Capabilities
	Deadline                          time.Time
	CurrentTriggerID                  domain.ID
	CausalTriggerID                   domain.ID
	WaitID, WakeEventID               domain.ID
	ObservedSequence                  uint64
	InputEventIDs                     []domain.ID
	RemainingTimeout                  time.Duration
	Declaration                       *ToolbeltDeclaration
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
func (s *Scheduler) checkCapacity(tx storage.Tx, work domain.Work, recovering domain.ID) error {
	xs, err := tx.ListExecutions("")
	if err != nil {
		return err
	}
	total := 0
	counts := map[domain.ID]int{}
	for _, x := range xs {
		state, e := attemptState(tx, x.ID)
		if e != nil && !errors.Is(e, storage.ErrNotFound) {
			return e
		}
		held := e == nil && !state.Excluded
		if !x.State.Terminal() && x.State != domain.ExecutionParked {
			held = true
		}
		if !held || x.ID == recovering {
			continue
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
		total++
		for _, scope := range chain {
			counts[scope]++
		}
	}
	if s.options.GlobalLimit > 0 && total >= s.options.GlobalLimit {
		return ErrCapacity
	}
	chain, err := scopeChain(tx, work.ScopeID)
	if err != nil {
		return err
	}
	for _, scope := range chain {
		if limit := s.options.ScopeLimits[scope]; limit > 0 && counts[scope] >= limit {
			return ErrCapacity
		}
	}
	return nil
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
		state = AttemptState{WorkID: id, ScopeID: ready.ScopeID, Generation: s.options.Generation, Executor: pinned.Executor, Binding: pinned, LaunchState: "launching", Retry: s.options.Retry, ControlErrorFallback: s.options.ControlErrorFallback[pinned.Executor], AutoContinueHandoff: s.options.AutoContinueHandoff}
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
		if e = s.checkCapacity(tx, current, parked); e != nil {
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
			state.Deadline = s.now().Add(s.options.Timeout)
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
		attempt.Request.Context.ToolbeltEndpoint, attempt.Request.Context.ToolbeltToken = s.options.ToolbeltEndpoint, token
	}
	if s.options.Secrets != nil {
		secrets, secretErr := s.options.Secrets(ctx, state)
		if secretErr != nil {
			failure := extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeFailed, Summary: "secret resolution failed"}}
			return attempt, errors.Join(ErrSecretUnavailable, s.fail(ctx, attempt.Lease, failure, !recovering, nil))
		}
		attempt.Request.Context.Secrets = secrets
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
	attempt.Handle = handle
	return attempt, nil
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
	return s.fail(ctx, ref, result, true, nil)
}
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
		return s.registerDeclaredWait(ctx, ref, *declared, result)
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
			events, err := tx.Events(0, 0)
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
		pending, err := PendingInputsInTx(tx, w.ID)
		if err != nil {
			return err
		}
		w.State = domain.WorkReady
		if result.Outcome.Kind == domain.OutcomeCompleted && len(pending) == 0 {
			w.State = domain.WorkCompleted
		}
		if err = tx.PutWork(w, w.Revision); err != nil {
			return err
		}
		if err = resetRetryInTx(tx, w.ID); err != nil {
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
