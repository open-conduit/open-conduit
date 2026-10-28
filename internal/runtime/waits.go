package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/storage"
)

const waitNamespace = "core.waits"
const wakeReadyNamespace = "core.waits.ready"
const subscriptionNamespace = "core.waits.subscriptions"
const pendingSubscriptionNamespace = "core.waits.pending_subscriptions"

// pendingSubscriptions are a registered Wait's translated dependencies not
// yet installed. Registration persists them atomically with the Wait, so a
// crash before installation only delays it to the next reconcile pass;
// installation backscans source history, so nothing is missed meanwhile.
type pendingSubscriptions struct {
	WaitID   domain.ID
	Requests []SubscriptionRequest
	Refused  []string `json:",omitempty"`
}

type waitMetadata struct {
	Timer   domain.Timer
	Binding string
	Blocked bool
}
type wakeReady struct {
	WaitID, ExecutionID, EventID domain.ID
	Binding                      string
}

// executionTriggerInTx preserves immutable Execution.TriggerID while derived
// activity follows the most recent accepted wake/continuation trigger.
func executionTriggerInTx(tx storage.Tx, x domain.Execution) (domain.ID, error) {
	state, err := attemptState(tx, x.ID)
	if errors.Is(err, storage.ErrNotFound) {
		return x.TriggerID, nil
	}
	if err != nil {
		return "", err
	}
	if state.CausalTriggerID != "" {
		return state.CausalTriggerID, nil
	}
	if state.CurrentTriggerID != "" {
		return state.CurrentTriggerID, nil
	}
	return x.TriggerID, nil
}

// Wait atomically releases authority, registers an owned-key predicate and scans
// accepted history strictly after the acknowledged observation watermark.
// This is trusted host authority after a finished or cooperative suspension
// acknowledgement, not proof derived from an untrusted toolbelt request.
func (s *Scheduler) Wait(ctx context.Context, ref domain.LeaseRef, wait domain.Wait, resume bool) error {
	return s.WaitWithResult(ctx, ref, wait, resume, extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeWaiting}})
}

// WaitWithResult publishes the waiting outcome and artifact references in the
// same transition as registration. Terminal waiting history is immutable.
func (s *Scheduler) WaitWithResult(ctx context.Context, ref domain.LeaseRef, wait domain.Wait, resume bool, result extension.Result) error {
	if result.Outcome.Kind != domain.OutcomeWaiting {
		return extension.ErrInvalid
	}
	if err := extension.ValidateResult(result); err != nil {
		return err
	}
	return s.transact(ctx, &ref, func(tx storage.Tx) error { return s.waitInTx(tx, ref, wait, resume, result) })
}

// registerDeclaredWait applies a toolbelt wait declaration after the trusted
// host observed the segment finish or suspend. The timeout timer is armed in
// the same registration transaction. Resume-capable executors park.
func (s *Scheduler) registerDeclaredWait(ctx context.Context, ref domain.LeaseRef, declared ToolbeltDeclaration, result extension.Result) error {
	return s.transact(ctx, &ref, func(tx storage.Tx) error {
		state, err := attemptState(tx, ref.ExecutionID)
		if err != nil {
			return err
		}
		if declared.Timer != nil {
			if err = armDeclaredTimerInTx(tx, s.now(), state.ScopeID, *declared.Timer); err != nil {
				return err
			}
		}
		if err = s.waitInTx(tx, ref, *declared.Wait, state.Capabilities.Resume, result); err != nil {
			return err
		}
		if len(declared.Subscriptions) == 0 {
			return nil
		}
		if current, err := tx.Wait(declared.Wait.ID); err != nil || current.State != domain.WaitActive {
			return err // an own-key match already woke it: nothing to install
		}
		return putJSON(tx, pendingSubscriptionNamespace, identity(string(declared.Wait.ID)), pendingSubscriptions{WaitID: declared.Wait.ID, Requests: declared.Subscriptions})
	})
}

// installPendingSubscriptions installs registered Waits' translated
// dependencies through Subscribe (authorization, atomic install and
// backscan). Refusals are recorded and never retried; storage errors leave
// the entry for the next pass.
func (s *Scheduler) installPendingSubscriptions(ctx context.Context, limit int) error {
	var rows []storage.Entry
	if err := s.transact(ctx, nil, func(tx storage.Tx) error {
		var er error
		rows, er = tx.List(pendingSubscriptionNamespace, "")
		return er
	}); err != nil {
		return err
	}
	for i, row := range rows {
		if i >= limit {
			break
		}
		var p pendingSubscriptions
		if err := json.Unmarshal(row.Value, &p); err != nil {
			return err
		}
		var active bool
		if err := s.transact(ctx, nil, func(tx storage.Tx) error {
			w, er := tx.Wait(p.WaitID)
			active = er == nil && w.State == domain.WaitActive
			if errors.Is(er, storage.ErrNotFound) {
				return nil
			}
			return er
		}); err != nil {
			return err
		}
		for active && len(p.Requests) > 0 {
			err := s.Subscribe(ctx, p.Requests[0])
			if err != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
				return err
			}
			if err != nil && !errors.Is(err, extension.ErrUnauthorized) && !errors.Is(err, storage.ErrInvalid) && !errors.Is(err, storage.ErrConflict) && !errors.Is(err, ErrCausalRejected) && !errors.Is(err, storage.ErrNotFound) {
				break // transient: keep the remainder for the next pass
			}
			if err != nil {
				p.Refused = append(p.Refused, p.Requests[0].ID)
			}
			p.Requests = p.Requests[1:]
		}
		if err := s.transact(ctx, nil, func(tx storage.Tx) error {
			if !active || len(p.Requests) == 0 {
				return tx.Delete(pendingSubscriptionNamespace, row.Key)
			}
			return putJSON(tx, pendingSubscriptionNamespace, row.Key, p)
		}); err != nil {
			return err
		}
	}
	return nil
}
func (s *Scheduler) waitInTx(tx storage.Tx, ref domain.LeaseRef, wait domain.Wait, resume bool, result extension.Result) error {
	{
		x, err := tx.Execution(ref.ExecutionID)
		if err != nil {
			return err
		}
		work, err := tx.Work(x.WorkID)
		if err != nil {
			return err
		}
		state, err := attemptState(tx, x.ID)
		if err != nil {
			return err
		}
		if wait.ID == "" || wait.WorkID != "" && wait.WorkID != work.ID || wait.ExecutionID != "" && wait.ExecutionID != x.ID || wait.Revision != 0 || wait.WakeEventID != "" || wait.State != "" && wait.State != domain.WaitActive {
			return storage.ErrInvalid
		}
		if resume && !state.Capabilities.Resume {
			return extension.ErrUnsupported
		}
		max, err := tx.LatestSequence()
		if err != nil {
			return err
		}
		if wait.AfterSequence > max || wait.AfterSequence < state.ObservedSequence {
			return storage.ErrInvalid
		}
		active, err := tx.ListWaits(work.ID)
		if err != nil {
			return err
		}
		for _, existing := range active {
			if existing.State == domain.WaitActive {
				return storage.ErrConflict
			}
		}
		wait.WorkID, wait.ExecutionID, wait.State = work.ID, x.ID, domain.WaitActive
		meta := waitMetadata{Binding: x.ExecutorBinding}
		if wait.TimeoutTimerID != "" {
			timer, er := tx.Timer(wait.TimeoutTimerID)
			if er != nil {
				return er
			}
			trigger, er := executionTriggerInTx(tx, x)
			if er != nil {
				return er
			}
			if timer.Event.Type != "core.wait.timeout" || timer.Event.WorkID != work.ID || timer.Event.ExecutionID != "" && timer.Event.ExecutionID != x.ID || timer.Event.CausationID != trigger {
				return storage.ErrInvalid
			}
			if _, duplicate, er := deliveryDuplicate(tx, timer.Event); er != nil {
				return er
			} else if duplicate {
				return storage.ErrConflict
			}
			if _, er = resolveScope(tx, timer.Event, work.ScopeID); er != nil {
				return er
			}
			if _, er = routeOwner(tx, timer.Event); er != nil {
				return er
			}
			meta.Timer = timer
		}
		x.State = domain.ExecutionWaiting
		if resume {
			x.State = domain.ExecutionParked
		}
		x.Outcome = &result.Outcome
		x.Artifacts = append([]domain.ArtifactRef(nil), result.Artifacts...)
		if err = tx.PutExecution(x, x.Revision); err != nil {
			return err
		}
		work.State = domain.WorkWaiting
		if err = tx.PutWork(work, work.Revision); err != nil {
			return err
		}
		if !state.Deadline.IsZero() {
			if !s.now().Before(state.Deadline) {
				return storage.ErrInvalid
			}
			state.RemainingTimeout = state.Deadline.Sub(s.now())
		}
		state.Deadline = time.Time{}
		if err = CancelExecutionDeadlineInTx(tx, x.ID); err != nil {
			return err
		}
		state.ObservedSequence = wait.AfterSequence
		state.LaunchState = "waiting"
		state.WaitID = wait.ID
		state.WakeEventID = ""
		// Parking is a cooperative executor suspension, not an uncertain failed
		// process. It releases scheduling capacity while preserving the same attempt.
		state.Excluded = true
		state.Result = &result
		state.Declaration = nil
		if err = putAttemptState(tx, state); err != nil {
			return err
		}
		if err = putJSON(tx, waitNamespace, identity(string(wait.ID)), meta); err != nil {
			return err
		}
		// Proved injections are consumed before the registration scan, so the
		// parked turn is never woken by (or handed again) what it took in.
		if err = consumeInjectedInTx(tx, x, result.ConsumedInputs); err != nil {
			return err
		}
		if err = tx.PutWait(wait, 0); err != nil {
			return err
		}
		if err = s.turnEndedInTx(tx, x, s.now()); err != nil {
			return err
		}
		_, err = WakeWaitInTx(tx, s.now(), s.options.CausalLimits, wait.ID)
		return err
	}
}

func waitMatches(wait domain.Wait, a domain.AcceptedEvent) bool {
	if a.Sequence <= wait.AfterSequence || a.Event.CorrelationKey == nil {
		return false
	}
	// A Work's own turn ending never wakes it.
	if a.Event.Type == domain.TypeTurnEnded && a.Event.WorkID == wait.WorkID {
		return false
	}
	matched := false
	for _, key := range wait.Match.Keys {
		if key == *a.Event.CorrelationKey {
			matched = true
		}
	}
	if !matched {
		return false
	}
	if len(wait.Match.Types) == 0 {
		return true
	}
	for _, typ := range wait.Match.Types {
		if typ == a.Event.Type {
			return true
		}
	}
	return false
}
func timeoutMatches(meta waitMetadata, a domain.AcceptedEvent) bool {
	t := meta.Timer.Event
	return meta.Timer.ID != "" && a.Event.ID == t.ID && a.Event.Source == t.Source && a.Event.DeliveryID == t.DeliveryID && a.Event.CausationID == t.CausationID
}

// WakeWaitInTx arbitrates accepted history first, including a timeout whose
// timer has already been deleted. A due clock may accept the timeout but never
// wakes directly. The caller commits the entire transition or none of it.
func WakeWaitInTx(tx storage.Tx, now time.Time, limits CausalLimits, id domain.ID) (domain.ID, error) {
	wait, err := tx.Wait(id)
	if err != nil {
		return "", err
	}
	if wait.State != domain.WaitActive {
		return wait.WakeEventID, nil
	}
	work, err := tx.Work(wait.WorkID)
	if err != nil {
		return "", err
	}
	if work.State.Terminal() {
		return "", nil
	}
	var meta waitMetadata
	if err = getJSON(tx, waitNamespace, identity(string(id)), &meta); err != nil {
		return "", err
	}
	if meta.Blocked {
		return "", nil
	}
	all, err := waitCandidatesInTx(tx, wait, meta)
	if err != nil {
		return "", err
	}
	winner, err := waitWinnerInTx(tx, wait, meta, work, all)
	if err != nil {
		return "", err
	}
	if winner.Event.ID == "" && meta.Timer.ID != "" && !now.Before(meta.Timer.FireAt) {
		timer, er := tx.Timer(meta.Timer.ID)
		if er == nil {
			if timer.Revision != meta.Timer.Revision || timer.Event.ID != meta.Timer.Event.ID {
				return "", storage.ErrConflict
			}
			fired, er := FireTimerInTx(tx, now, limits, work.ScopeID, timer.ID, timer.Revision, nil)
			if er != nil {
				return "", er
			}
			if fired.Rejected {
				return "", nil
			}
			winner = fired.Event
		} else if !errors.Is(er, storage.ErrNotFound) {
			return "", er
		}
	}
	if winner.Event.ID == "" {
		return "", nil
	}
	state, err := attemptState(tx, wait.ExecutionID)
	if err != nil {
		return "", err
	}
	x, err := tx.Execution(wait.ExecutionID)
	if err != nil {
		return "", err
	}
	if x.State == domain.ExecutionParked {
		parent, er := executionTriggerInTx(tx, x)
		if er != nil {
			return "", er
		}
		previousInfo, er := ancestry(tx, parent, limits)
		if er != nil {
			return "", er
		}
		winnerInfo, er := ancestry(tx, winner.Event.ID, limits)
		if er != nil {
			return "", er
		}
		if winnerInfo.Root == previousInfo.Root && winnerInfo.Depth >= previousInfo.Depth {
			parent = winner.Event.ID
		}
		data, _ := json.Marshal(struct{ WaitID, WinnerID domain.ID }{wait.ID, winner.Event.ID})
		bridge := domain.Event{ID: domain.ID("wait-woken/" + identity(string(wait.ID))), Type: "core.wait.woken", Source: "core.wait", DeliveryID: "woken/" + string(wait.ID), WorkID: work.ID, ExecutionID: x.ID, CausationID: parent, OccurredAt: now, ReceivedAt: now, Data: domain.Payload{Schema: "core.wait.woken.v1", Data: data}}
		accepted, _, er := AcceptDerivedInTx(tx, now, limits, work.ScopeID, parent, bridge)
		if errors.Is(er, ErrCausalRejected) {
			meta.Blocked = true
			if er = disableWaitSubscriptionsInTx(tx, wait.ID); er != nil {
				return "", er
			}
			if er = putJSON(tx, waitNamespace, identity(string(wait.ID)), meta); er != nil {
				return "", er
			}
			if timer, er := tx.Timer(wait.TimeoutTimerID); er == nil {
				if er = tx.DeleteTimer(timer.ID, timer.Revision); er != nil {
					return "", er
				}
				if er = tx.Delete(timerNamespace, timerKey(timer.ID)); er != nil {
					return "", er
				}
			} else if !errors.Is(er, storage.ErrNotFound) {
				return "", er
			}
			return "", nil
		}
		if er != nil {
			return "", er
		}
		// This is a causal/audit bridge, not another route input. Its only
		// effect is the atomic wake transition, so checkpoint it independently
		// of ordinary Work input ordering.
		if er = tx.MarkRouted(accepted.Event.ID, "", "wake_causation"); er != nil {
			return "", er
		}
		state.CausalTriggerID = accepted.Event.ID
	}
	// Earlier ordinary input is durable context, not a reason to miss the wake.
	// Join its route checkpoints in acceptance order before the winner checkpoint.
	for _, a := range all {
		if a.Sequence >= winner.Sequence {
			break
		}
		if a.Routed {
			continue
		}
		target, er := potentiallyTargets(tx, a.Event, work)
		if er != nil {
			return "", er
		}
		if !target {
			continue
		}
		scope, er := scopeForEvent(tx, a)
		if er != nil {
			return "", er
		}
		if scope != "" && scope != work.ScopeID {
			return "", storage.ErrConflict
		}
		owner, er := routeOwner(tx, a.Event)
		if er != nil {
			return "", er
		}
		if owner.ID != work.ID {
			return "", storage.ErrInvalid
		}
		if er = queueWaitInput(tx, work.ID, meta.Binding, a, "wait_context"); er != nil {
			return "", er
		}
	}
	if err = queueWaitInput(tx, work.ID, meta.Binding, winner, "wake"); err != nil {
		return "", err
	}
	if wait.TimeoutTimerID != "" {
		timer, er := tx.Timer(wait.TimeoutTimerID)
		if er == nil {
			if er = tx.DeleteTimer(timer.ID, timer.Revision); er != nil {
				return "", er
			}
			if er = tx.Delete(timerNamespace, timerKey(timer.ID)); er != nil {
				return "", er
			}
		} else if !errors.Is(er, storage.ErrNotFound) {
			return "", er
		}
	}
	wait.State, wait.WakeEventID = domain.WaitWoken, winner.Event.ID
	if err = tx.PutWait(wait, wait.Revision); err != nil {
		return "", err
	}
	work.State = domain.WorkReady
	if err = tx.PutWork(work, work.Revision); err != nil {
		return "", err
	}
	state.WakeEventID = winner.Event.ID
	if err = putAttemptState(tx, state); err != nil {
		return "", err
	}
	if err = putJSON(tx, wakeReadyNamespace, identity(string(work.ID)), wakeReady{wait.ID, wait.ExecutionID, winner.Event.ID, meta.Binding}); err != nil {
		return "", err
	}
	if err = disableWaitSubscriptionsInTx(tx, wait.ID); err != nil {
		return "", err
	}
	return winner.Event.ID, nil
}

// waitCandidatesInTx is the accepted input that may wake wait, in acceptance
// order: everything after its boundary, and its timeout Event should that
// precede the boundary.
func waitCandidatesInTx(tx storage.Tx, wait domain.Wait, meta waitMetadata) ([]domain.AcceptedEvent, error) {
	all, err := tx.Events(wait.AfterSequence, 0)
	if err != nil {
		return nil, err
	}
	if meta.Timer.ID != "" {
		if t, er := tx.Event(meta.Timer.Event.ID); er == nil && t.Sequence <= wait.AfterSequence {
			all = append([]domain.AcceptedEvent{t}, all...)
		} else if er != nil && !errors.Is(er, storage.ErrNotFound) {
			return nil, er
		}
	}
	return all, nil
}

// waitWinnerInTx picks, without writing, the first candidate that wakes
// wait: a matching input the Work has not already consumed, or its timeout.
// It is empty when none does.
func waitWinnerInTx(tx storage.Tx, wait domain.Wait, meta waitMetadata, work domain.Work, all []domain.AcceptedEvent) (domain.AcceptedEvent, error) {
	for _, a := range all {
		if !waitMatches(wait, a) && !timeoutMatches(meta, a) {
			continue
		}
		if !timeoutMatches(meta, a) {
			// Input this Work already consumed (an injection the parked
			// attempt proved it took in) is history, not news.
			consumed, er := consumedInTx(tx, work.ID, a.Sequence)
			if er != nil {
				return domain.AcceptedEvent{}, er
			}
			if consumed {
				continue
			}
		}
		scope, er := scopeForEvent(tx, a)
		if er != nil {
			return domain.AcceptedEvent{}, er
		}
		if scope != "" && scope != work.ScopeID {
			return domain.AcceptedEvent{}, storage.ErrConflict
		}
		owner, er := routeOwner(tx, a.Event)
		if er != nil {
			return domain.AcceptedEvent{}, er
		}
		if owner.ID != work.ID {
			return domain.AcceptedEvent{}, storage.ErrInvalid
		}
		return a, nil
	}
	return domain.AcceptedEvent{}, nil
}
func queueWaitInput(tx storage.Tx, work domain.ID, binding string, a domain.AcceptedEvent, disposition string) error {
	if err := putJSON(tx, queueNamespace, queueKey(work, a.Sequence), QueuedInput{EventID: a.Event.ID, Sequence: a.Sequence, Binding: binding}); err != nil {
		return err
	}
	if !a.Routed {
		return tx.MarkRouted(a.Event.ID, work, disposition)
	}
	return nil
}
func (s *Scheduler) Wake(ctx context.Context, id domain.ID) error {
	return s.transact(ctx, nil, func(tx storage.Tx) error { _, err := WakeWaitInTx(tx, s.now(), s.options.CausalLimits, id); return err })
}

// AliasAuthorization is supplied to trusted composition policy. The callback is
// outside transactions and is rechecked against Scope revisions before publish.
type AliasAuthorization struct {
	SourceScopeID, TargetScopeID domain.ID
	Generation                   string
}
type SubscriptionRequest struct {
	Namespace     string
	ID            string
	WaitID        domain.ID
	SourceScopeID domain.ID
	SourceKey     domain.CorrelationKey
	Types         []string
	TargetKey     domain.CorrelationKey
	AliasType     string
	// After is the exclusive acceptance sequence the source scan starts
	// from: zero scans all accepted history (an already-met state counts).
	After uint64 `json:",omitempty"`
	// Filter is opaque; SchedulerOptions.AliasFilter evaluates it for the
	// namespace. Label is a short secret-free description for the alias.
	Filter *domain.Payload `json:",omitempty"`
	Label  string          `json:",omitempty"`
}
type subscription struct {
	Request               SubscriptionRequest
	WorkID, TargetScopeID domain.ID
	Checkpoint            uint64
	Active                bool
}

func disableWaitSubscriptionsInTx(tx storage.Tx, id domain.ID) error {
	rows, err := tx.List(subscriptionNamespace, "")
	if err != nil {
		return err
	}
	for _, row := range rows {
		var sub subscription
		if err = json.Unmarshal(row.Value, &sub); err != nil {
			return err
		}
		if sub.Request.WaitID == id && sub.Active {
			sub.Active = false
			if err = putJSON(tx, subscriptionNamespace, row.Key, sub); err != nil {
				return err
			}
		}
	}
	return nil
}
func (s *Scheduler) Subscribe(ctx context.Context, request SubscriptionRequest) error {
	request.Types = append([]string(nil), request.Types...)
	if request.Namespace == "" {
		request.Namespace = "core.waits"
	}
	if len(request.Namespace) > 1024 {
		return storage.ErrInvalid
	}
	if len(request.Label) > 200 || request.ID == "" || len(request.ID) > 1024 || request.SourceKey.Source == "" || request.SourceKey.ExternalID == "" || request.WaitID == "" || !validType(request.AliasType) || strings.HasPrefix(request.AliasType, "core.") {
		return storage.ErrInvalid
	}
	for _, typ := range request.Types {
		if !validType(typ) {
			return storage.ErrInvalid
		}
	}
	var sub subscription
	var scopes []domain.Scope
	var duplicate bool
	err := s.transact(ctx, nil, func(tx storage.Tx) error {
		var old subscription
		er := getJSON(tx, subscriptionNamespace, identity(request.Namespace)+"/"+identity(request.ID), &old)
		if er == nil {
			candidate := request
			if candidate.SourceScopeID == "" {
				candidate.SourceScopeID = old.Request.SourceScopeID
			}
			if !reflect.DeepEqual(candidate, old.Request) {
				return storage.ErrConflict
			}
			duplicate = true
			sub = old
			return nil
		}
		if !errors.Is(er, storage.ErrNotFound) {
			return er
		}
		return nil
	})
	if err != nil {
		return err
	}
	if duplicate {
		return s.authorizeAlias(ctx, sub)
	}
	err = s.transact(ctx, nil, func(tx storage.Tx) error {
		wait, er := tx.Wait(request.WaitID)
		if er != nil {
			return er
		}
		if wait.State != domain.WaitActive {
			return storage.ErrInvalid
		}
		var meta waitMetadata
		if er = getJSON(tx, waitNamespace, identity(string(wait.ID)), &meta); er != nil {
			return er
		}
		if meta.Blocked {
			return ErrCausalRejected
		}
		work, er := tx.Work(wait.WorkID)
		if er != nil {
			return er
		}
		if !containsKey(work, request.TargetKey) {
			return storage.ErrInvalid
		}
		found := false
		for _, key := range wait.Match.Keys {
			if key == request.TargetKey {
				found = true
			}
		}
		if !found {
			return storage.ErrInvalid
		}
		if len(wait.Match.Types) > 0 {
			allowed := false
			for _, typ := range wait.Match.Types {
				if typ == request.AliasType {
					allowed = true
				}
			}
			if !allowed {
				return storage.ErrInvalid
			}
		}
		if owner, er := tx.WorkByCorrelation(request.SourceKey); er == nil {
			if request.SourceScopeID != "" && request.SourceScopeID != owner.ScopeID {
				return storage.ErrConflict
			}
			request.SourceScopeID = owner.ScopeID
		} else if !errors.Is(er, storage.ErrNotFound) {
			return er
		}
		if request.SourceScopeID == "" {
			owner, er := tx.WorkByCorrelation(request.SourceKey)
			if er != nil {
				return er
			}
			request.SourceScopeID = owner.ScopeID
		}
		scopes, er = captureAliasScopesInTx(tx, request.SourceScopeID, work.ScopeID)
		if er != nil {
			return er
		}
		sub = subscription{Request: request, WorkID: work.ID, TargetScopeID: work.ScopeID, Checkpoint: request.After, Active: true}
		return nil
	})
	if err != nil {
		return err
	}
	if err = s.authorizeAlias(ctx, sub); err != nil {
		return err
	}
	return s.transact(ctx, nil, func(tx storage.Tx) error {
		for _, old := range scopes {
			now, er := tx.Scope(old.ID)
			if er != nil {
				return er
			}
			if !reflect.DeepEqual(old, now) {
				return storage.ErrConflict
			}
		}
		key := identity(request.Namespace) + "/" + identity(request.ID)
		var old subscription
		er := getJSON(tx, subscriptionNamespace, key, &old)
		if er == nil {
			if !reflect.DeepEqual(old.Request, request) {
				return storage.ErrConflict
			}
			return nil
		}
		if !errors.Is(er, storage.ErrNotFound) {
			return er
		}
		wait, er := tx.Wait(request.WaitID)
		if er != nil {
			return er
		}
		if wait.State != domain.WaitActive {
			return storage.ErrInvalid
		}
		if er = putJSON(tx, subscriptionNamespace, key, sub); er != nil {
			return er
		}
		return s.reconcileSubscriptionInTx(tx, key, &sub)
	})
}
func (s *Scheduler) authorizeAlias(ctx context.Context, sub subscription) error {
	if sub.Request.SourceScopeID == sub.TargetScopeID {
		return nil
	}
	if s.options.AuthorizeAlias == nil {
		return extension.ErrUnauthorized
	}
	return s.options.AuthorizeAlias(ctx, AliasAuthorization{SourceScopeID: sub.Request.SourceScopeID, TargetScopeID: sub.TargetScopeID, Generation: s.options.Generation})
}
func (s *Scheduler) ReconcileSubscriptions(ctx context.Context, limit int) error {
	if limit < 1 || limit > 200 {
		return storage.ErrInvalid
	}
	if err := s.installPendingSubscriptions(ctx, limit); err != nil {
		return err
	}
	var rows []storage.Entry
	var cursor string
	if err := s.transact(ctx, nil, func(tx storage.Tx) error {
		var er error
		rows, er = tx.List(subscriptionNamespace, "")
		if er != nil {
			return er
		}
		er = getJSON(tx, waitNamespace, "subscription_cursor", &cursor)
		if errors.Is(er, storage.ErrNotFound) {
			return nil
		}
		return er
	}); err != nil {
		return err
	}
	ordered := make([]storage.Entry, 0, len(rows))
	for _, row := range rows {
		if row.Key > cursor {
			ordered = append(ordered, row)
		}
	}
	for _, row := range rows {
		if row.Key <= cursor {
			ordered = append(ordered, row)
		}
	}
	rows = ordered
	count := 0
	for _, row := range rows {
		var sub subscription
		if err := json.Unmarshal(row.Value, &sub); err != nil {
			return err
		}
		if !sub.Active {
			continue
		}
		if count >= limit {
			break
		}
		count++
		var scopes []domain.Scope
		if err := s.transact(ctx, nil, func(tx storage.Tx) error {
			var er error
			scopes, er = captureAliasScopesInTx(tx, sub.Request.SourceScopeID, sub.TargetScopeID)
			if er != nil {
				return er
			}
			return putJSON(tx, waitNamespace, "subscription_cursor", row.Key)
		}); err != nil {
			return err
		}
		if err := s.authorizeAlias(ctx, sub); err != nil {
			return err
		}
		if err := s.transact(ctx, nil, func(tx storage.Tx) error {
			for _, old := range scopes {
				current, er := tx.Scope(old.ID)
				if er != nil {
					return er
				}
				if !reflect.DeepEqual(old, current) {
					return storage.ErrConflict
				}
			}
			var current subscription
			if er := getJSON(tx, subscriptionNamespace, row.Key, &current); er != nil {
				return er
			}
			if !current.Active {
				return nil
			}
			return s.reconcileSubscriptionInTx(tx, row.Key, &current)
		}); err != nil {
			return err
		}
	}
	return nil
}
func (s *Scheduler) reconcileSubscriptionInTx(tx storage.Tx, key string, sub *subscription) error {
	wait, err := tx.Wait(sub.Request.WaitID)
	if err != nil {
		return err
	}
	work, err := tx.Work(sub.WorkID)
	if err != nil {
		return err
	}
	if wait.State != domain.WaitActive || work.State.Terminal() {
		sub.Active = false
		return putJSON(tx, subscriptionNamespace, key, sub)
	}
	all, err := tx.Events(sub.Checkpoint, 0)
	if err != nil {
		return err
	}
	for _, source := range all {
		sub.Checkpoint = source.Sequence
		if source.Event.CorrelationKey == nil || *source.Event.CorrelationKey != sub.Request.SourceKey {
			continue
		}
		typeMatch := len(sub.Request.Types) == 0
		for _, typ := range sub.Request.Types {
			if source.Event.Type == typ {
				typeMatch = true
			}
		}
		if !typeMatch {
			continue
		}
		if sub.Request.Filter != nil {
			if s.options.AliasFilter == nil || !s.options.AliasFilter(sub.Request.Namespace, *sub.Request.Filter, source.Event) {
				continue
			}
		}
		scope, er := scopeForEvent(tx, source)
		if er != nil {
			return er
		}
		if scope != sub.Request.SourceScopeID {
			return storage.ErrConflict
		}
		data, _ := json.Marshal(struct {
			Subscription string `json:"subscription"`
			Event        string `json:"event"`
			SourceType   string `json:"source_type"`
			Label        string `json:"label,omitempty"`
		}{sub.Request.ID, string(source.Event.ID), source.Event.Type, sub.Request.Label})
		event := domain.Event{ID: domain.ID("alias/" + identity(sub.Request.Namespace, sub.Request.ID, string(source.Event.ID))), Source: "core.subscription", DeliveryID: identity(sub.Request.Namespace, sub.Request.ID, string(source.Event.ID)), Type: sub.Request.AliasType, WorkID: sub.WorkID, CorrelationKey: &sub.Request.TargetKey, CausationID: source.Event.ID, OccurredAt: source.Event.OccurredAt, ReceivedAt: s.now(), Data: domain.Payload{Schema: "core.subscription.alias.v1", Data: data}}
		_, _, er = AcceptDerivedInTx(tx, s.now(), s.options.CausalLimits, work.ScopeID, source.Event.ID, event)
		if er != nil && !errors.Is(er, ErrCausalRejected) {
			return er
		}
		// A refused branch is diagnosed and checkpointed, never retried in a tight loop.
		if er = putJSON(tx, subscriptionNamespace, key, sub); er != nil {
			return er
		}
		if _, er = WakeWaitInTx(tx, s.now(), s.options.CausalLimits, wait.ID); er != nil {
			return er
		}
		current, er := tx.Wait(wait.ID)
		if er != nil {
			return er
		}
		if current.State != domain.WaitActive {
			sub.Active = false
			break
		}
	}
	return putJSON(tx, subscriptionNamespace, key, sub)
}

// routeWaitingInTx handles predicates before ordinary route policy. It queues
// unmatched context without making a waiting Work ready.
func routeWaitingInTx(tx storage.Tx, now time.Time, limits CausalLimits, id domain.ID) (bool, error) {
	a, err := tx.Event(id)
	if err != nil {
		return false, err
	}
	if a.Routed {
		return true, nil
	}
	work, err := routeOwner(tx, a.Event)
	if errors.Is(err, storage.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if work.State != domain.WorkWaiting {
		return false, nil
	}
	waits, err := tx.ListWaits(work.ID)
	if err != nil {
		return false, err
	}
	var active *domain.Wait
	for i := range waits {
		if waits[i].State == domain.WaitActive {
			active = &waits[i]
			break
		}
	}
	if active == nil {
		return false, storage.ErrInvalid
	}
	if _, err = WakeWaitInTx(tx, now, limits, active.ID); err != nil {
		return false, err
	}
	a, err = tx.Event(id)
	if err != nil {
		return false, err
	}
	if a.Routed {
		return true, nil
	}
	current, err := tx.Work(work.ID)
	if err != nil {
		return false, err
	}
	if current.State != domain.WorkWaiting {
		return false, nil
	}
	var meta waitMetadata
	if err = getJSON(tx, waitNamespace, identity(string(active.ID)), &meta); err != nil {
		return false, err
	}
	all, err := tx.UnroutedEvents(0, 0)
	if err != nil {
		return false, err
	}
	for _, prior := range all {
		if prior.Sequence > a.Sequence {
			break
		}
		if prior.Routed {
			continue
		}
		target, er := potentiallyTargets(tx, prior.Event, work)
		if er != nil {
			return false, er
		}
		if !target {
			continue
		}
		scope, er := scopeForEvent(tx, prior)
		if er != nil {
			return false, er
		}
		if scope != "" && scope != work.ScopeID {
			return false, storage.ErrConflict
		}
		owner, er := routeOwner(tx, prior.Event)
		if er != nil {
			return false, er
		}
		if owner.ID != work.ID {
			return false, storage.ErrInvalid
		}
		if er = queueWaitInput(tx, work.ID, meta.Binding, prior, "wait_context"); er != nil {
			return false, er
		}
	}
	return true, nil
}

func captureAliasScopesInTx(tx storage.Tx, source, target domain.ID) ([]domain.Scope, error) {
	var scopes []domain.Scope
	seen := map[domain.ID]bool{}
	for _, id := range []domain.ID{source, target} {
		chain, err := scopeChain(tx, id)
		if err != nil {
			return nil, err
		}
		for _, scopeID := range chain {
			if seen[scopeID] {
				continue
			}
			scope, err := tx.Scope(scopeID)
			if err != nil {
				return nil, err
			}
			seen[scopeID] = true
			scopes = append(scopes, scope)
		}
	}
	return scopes, nil
}

// acknowledgeWaiting records a finished/suspended executor observation after the
// registration transaction has already released its lease. It cannot register
// predicates, wake Work or acknowledge a later resumed epoch.
func (s *Scheduler) acknowledgeWaiting(ctx context.Context, ref domain.LeaseRef, result extension.Result) error {
	return s.transact(ctx, nil, func(tx storage.Tx) error {
		state, err := attemptState(tx, ref.ExecutionID)
		if err != nil {
			return err
		}
		x, err := tx.Execution(ref.ExecutionID)
		if err != nil {
			return err
		}
		if state.Epoch != ref.Epoch || x.Lease.Epoch != ref.Epoch {
			return storage.ErrLeaseLost
		}
		if state.WaitID == "" || x.State != domain.ExecutionParked && x.State != domain.ExecutionWaiting {
			return extension.ErrUnsupported
		}
		wait, err := tx.Wait(state.WaitID)
		if err != nil {
			return err
		}
		if wait.ExecutionID != x.ID {
			return storage.ErrInvalid
		}
		if x.State == domain.ExecutionParked {
			x.Outcome = &result.Outcome
			x.Artifacts = append([]domain.ArtifactRef(nil), result.Artifacts...)
			if err = tx.PutExecution(x, x.Revision); err != nil {
				return err
			}
			if err = s.turnEndedInTx(tx, x, s.now()); err != nil {
				return err
			}
		} else if x.Outcome == nil || !reflect.DeepEqual(*x.Outcome, result.Outcome) || !(len(x.Artifacts) == 0 && len(result.Artifacts) == 0 || reflect.DeepEqual(x.Artifacts, result.Artifacts)) {
			return storage.ErrConflict
		}
		state.Result = &result
		return putAttemptState(tx, state)
	})
}
