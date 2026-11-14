package runtime

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/storage"
)

// claimLineageInTx gives parent its lineage key (domain.LineageKey) when it
// does not own it yet, so its children's domain.TypeChildEnded Events are
// correlated to it and a Wait on its keys sees them. Work that owns no key
// claims it too when it first waits (declareWaitInTx). It returns the Work
// as stored.
func claimLineageInTx(tx storage.Tx, parent domain.Work) (domain.Work, error) {
	key := domain.LineageKey(parent.ID)
	if slices.Contains(parent.CorrelationKeys, key) {
		return parent, nil
	}
	parent.CorrelationKeys = append(append([]domain.CorrelationKey(nil), parent.CorrelationKeys...), key)
	if err := tx.PutWork(parent, parent.Revision); err != nil {
		return domain.Work{}, err
	}
	return tx.Work(parent.ID)
}

// childEndedInTx tells a child's parent that the child ended, in the
// transaction that made it terminal: a domain.TypeChildEnded Event on the
// parent's lineage key, carrying the child's state and its last attempt's
// outcome and result. Its causation is the child's last trigger (its
// latest attempt's, else its creation), so a chain of Work asking Work stays
// one causal chain. Work with no parent, Work that is not terminal, and a
// parent that never claimed its lineage key (Work an operator made a child
// of another) get nothing.
//
// The child's end is a fact, recorded whatever the causal limits say: it is
// produced once, and refusing it would leave the parent waiting for an
// answer that never comes. It is accepted with its causation but charges no
// budget; the limits apply to what it causes. A loop is refused where Work
// asks for Work (create_work's derived creation), and a parent woken by an
// end too deep for its wake bridge is held with core.wait.blocked, which an
// operator can release (blockWaitInTx).
func childEndedInTx(tx storage.Tx, now time.Time, child domain.Work) error {
	if child.ParentID == "" || !child.State.Terminal() {
		return nil
	}
	parent, err := tx.Work(child.ParentID)
	if errors.Is(err, storage.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	key := domain.LineageKey(parent.ID)
	if !slices.Contains(parent.CorrelationKeys, key) {
		return nil
	}
	ended := domain.ChildEnded{Work: string(child.ID), Kind: child.Kind, State: child.State}
	trigger := domain.ID("")
	latest, err := latestAttemptInTx(tx, child.ID)
	if err != nil {
		return err
	}
	if latest != nil {
		if trigger, err = executionTriggerInTx(tx, *latest); err != nil {
			return err
		}
		if o := latest.Outcome; o != nil {
			ended.Outcome, ended.Summary = o.Kind, o.Summary
			if d, ok := domain.TurnOf(o); ok && d.HasResult() {
				ended.Result = d.Result
			}
		}
	}
	if trigger == "" {
		created, er := tx.EventByDelivery("core.work", "created/"+string(child.ID))
		if er != nil && !errors.Is(er, storage.ErrNotFound) {
			return er
		}
		trigger = created.Event.ID
	}
	data, err := json.Marshal(ended)
	if err != nil {
		return err
	}
	delivery := "child-ended/" + string(child.ID)
	e := domain.Event{ID: domain.ID("child-ended/" + identity(string(child.ID))), Type: domain.TypeChildEnded, Source: domain.LineageSource, DeliveryID: delivery,
		CorrelationKey: &key, WorkID: parent.ID, CausationID: trigger, OccurredAt: now, ReceivedAt: now, Data: domain.Payload{Schema: domain.SchemaChildEnded, Data: data}}
	_, _, err = AcceptInTx(tx, parent.ScopeID, e)
	return err
}

// childReportedInTx tells a child's parent what a turn of the child
// handed back when the turn leaves the child open, in the transition that
// stores the turn's outcome: a domain.TypeChildReported Event on the
// parent's lineage key, with the child's state, the turn's status and
// summary, and its result. It is how child Work that keeps going (follow-up it
// still answers for) gives its asker an answer without ending;
// its end is still told with domain.TypeChildEnded (childEndedInTx). The
// core reads no meaning into the status: any turn that carries a result
// and leaves the child nonterminal reports, and a failed attempt, a turn
// without a result, Work that has ended (its end tells the parent), Work
// with no parent, and a parent that has ended or never claimed its
// lineage key get nothing. The delivery identity is the execution's lease
// epoch, so a repeated transition is a no-op while a resumed attempt's
// next turn reports again. Like a child's end it is a fact, accepted with
// the turn's trigger as its causation and charging no budget.
func childReportedInTx(tx storage.Tx, now time.Time, x domain.Execution) error {
	d, ok := domain.TurnOf(x.Outcome)
	if !ok || !d.HasResult() || x.Outcome.Kind == domain.OutcomeFailed {
		return nil
	}
	child, err := tx.Work(x.WorkID)
	if err != nil {
		return err
	}
	if child.ParentID == "" || child.State.Terminal() {
		return nil
	}
	parent, err := tx.Work(child.ParentID)
	if errors.Is(err, storage.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	key := domain.LineageKey(parent.ID)
	if parent.State.Terminal() || !slices.Contains(parent.CorrelationKeys, key) {
		return nil
	}
	trigger, err := executionTriggerInTx(tx, x)
	if err != nil {
		return err
	}
	data, err := json.Marshal(domain.ChildReported{Work: string(child.ID), Kind: child.Kind, State: child.State, Status: d.Status, Summary: x.Outcome.Summary, Result: d.Result})
	if err != nil {
		return err
	}
	delivery := fmt.Sprintf("child-reported/%s/%d", x.ID, x.Lease.Epoch)
	e := domain.Event{ID: domain.ID("child-reported/" + identity(delivery)), Type: domain.TypeChildReported, Source: domain.LineageSource, DeliveryID: delivery,
		CorrelationKey: &key, WorkID: parent.ID, CausationID: trigger, OccurredAt: now, ReceivedAt: now, Data: domain.Payload{Schema: domain.SchemaChildReported, Data: data}}
	_, _, err = AcceptInTx(tx, parent.ScopeID, e)
	return err
}

// cancelledChild is what cancel_work returns: the child as it ended and
// the Event recording why.
type cancelledChild struct {
	Work  domain.Work
	Event domain.AcceptedEvent
}

// cancelChildInTx is cancel_work: the execution x of parent cancels its
// child Work. Only Work whose parent is parent, in parent's scope, may be
// cancelled; anything else (another Work's child, Work with no parent, an
// unknown ID) is refused as unauthorized, fail closed and without saying
// whether it exists. A child that has already ended is refused with
// extension.CodeWorkEnded.
//
// The cancel is recorded on the child as a domain.TypeChildCancel Event
// from the host (domain.LineageSource, a source no execution can emit
// from, so the caller can neither pre-empt nor replace the record), naming
// the cancelling execution in its payload and caused by its trigger, so the
// child's history says who ended it and why. It carries no ExecutionID,
// which would have to be an attempt of the child's own Work. It is a
// control fact, never routed. The child then
// ends the way an operator's cancel ends it (cancelWorkInTx), so the
// parent hears of it with domain.TypeChildEnded in state cancelled.
func cancelChildInTx(tx storage.Tx, now time.Time, limits CausalLimits, parent domain.Work, x domain.Execution, trigger domain.ID, r extension.CancelRequest) (cancelledChild, error) {
	child, err := tx.Work(r.Work)
	if errors.Is(err, storage.ErrNotFound) {
		return cancelledChild{}, unauthorized(RefusalNotGranted)
	}
	if err != nil {
		return cancelledChild{}, err
	}
	if child.ParentID != parent.ID || child.ScopeID != parent.ScopeID {
		return cancelledChild{}, unauthorized(RefusalNotGranted)
	}
	if child.State.Terminal() {
		return cancelledChild{}, &extension.OperationError{Code: extension.CodeWorkEnded, Message: "that Work has already ended (" + string(child.State) + ")"}
	}
	data, err := json.Marshal(domain.ChildCancel{Parent: string(parent.ID), Execution: string(x.ID), Reason: r.Reason})
	if err != nil {
		return cancelledChild{}, err
	}
	delivery := "child-cancel/" + string(child.ID)
	e := domain.Event{ID: domain.ID("child-cancel/" + identity(string(child.ID))), Type: domain.TypeChildCancel, Source: domain.LineageSource, DeliveryID: delivery,
		WorkID: child.ID, CausationID: trigger, OccurredAt: now, ReceivedAt: now, Data: domain.Payload{Schema: domain.SchemaChildCancel, Data: data}}
	accepted, added, err := AcceptInTx(tx, parent.ScopeID, e)
	if err != nil {
		return cancelledChild{}, err
	}
	if !added {
		// A child that is still open has never been cancelled, so its
		// record can't exist yet: fail rather than take another Event's
		// place in the trail.
		return cancelledChild{}, storage.ErrConflict
	}
	if err = tx.MarkRouted(accepted.Event.ID, "", "control"); err != nil {
		return cancelledChild{}, err
	}
	if err = cancelWorkInTx(tx, now, limits, child.ID); err != nil {
		return cancelledChild{}, err
	}
	if child, err = tx.Work(child.ID); err != nil {
		return cancelledChild{}, err
	}
	return cancelledChild{Work: child, Event: accepted}, nil
}

// MissingResultSummary is the outcome summary of an attempt of child Work
// that completed without a result (Scheduler.Complete fails it).
const MissingResultSummary = "the turn completed without a result; Work another Work asked for must return one (core.turn.v1 result)"

// resultMissingInTx reports whether completing the attempt with outcome
// would end child Work its parent is waiting to hear from without a result
// (domain.TurnDeclaration.Result). Work that would not end now (queued
// input, a standing kind) and Work whose parent never asked for it through
// create_work are never refused.
func (s *Scheduler) resultMissingInTx(tx storage.Tx, ref domain.LeaseRef, outcome domain.Outcome) (bool, error) {
	if d, ok := domain.TurnOf(&outcome); ok && d.HasResult() {
		return false, nil
	}
	x, err := tx.Execution(ref.ExecutionID)
	if err != nil {
		return false, err
	}
	w, err := tx.Work(x.WorkID)
	if err != nil || w.ParentID == "" || s.options.WorkKinds[w.Kind].Standing {
		return false, err
	}
	parent, err := tx.Work(w.ParentID)
	if errors.Is(err, storage.ErrNotFound) {
		return false, nil
	}
	if err != nil || !slices.Contains(parent.CorrelationKeys, domain.LineageKey(parent.ID)) {
		return false, err
	}
	pending, err := PendingInputsInTx(tx, w.ID)
	return len(pending) == 0, err
}

// CodeNoChildInProgress is the refusal code of a wait that only a child's
// end could satisfy, declared by Work with no child that can still end.
const CodeNoChildInProgress = "no_child_in_progress"

// childEndReachableInTx refuses a wait that can never be satisfied because
// it waits only for news of a child (every type it names is
// domain.TypeChildEnded or domain.TypeChildReported) and no such Event can
// come: the Work has no child that hasn't ended, and no child's end or
// report of a type it names has arrived after the wait's
// watermark for it to wake on; or the wait's keys leave out the Work's
// lineage key, the only key a child's end arrives on. Declaring such a wait
// would release the execution's authority and park the Work for good (a
// wait declared before the create_work it was for), so it is refused
// before it is declared, and the caller keeps its authority to ask for the
// child first. A wait that names any other type (a person's message) or no
// type at all may still be satisfied, and is left alone.
func childEndReachableInTx(tx storage.Tx, w domain.Work, match domain.EventMatch, after uint64) error {
	if len(match.Types) == 0 {
		return nil
	}
	for _, name := range match.Types {
		if !domain.IsChildNews(name) {
			return nil
		}
	}
	lineage := domain.LineageKey(w.ID)
	if !slices.Contains(match.Keys, lineage) {
		if !slices.Contains(w.CorrelationKeys, lineage) {
			return noChildInProgress()
		}
		return &extension.OperationError{Code: CodeNoChildInProgress, Message: "a child Work's end or report arrives only on this Work's own key, " + lineage.Source + "/" + lineage.ExternalID + ", which this wait leaves out: give no keys, or include it"}
	}
	live, err := tx.CountWorks(storage.WorkQuery{ParentID: w.ID, States: []domain.WorkState{domain.WorkReady, domain.WorkRunning, domain.WorkWaiting}})
	if err != nil || live > 0 {
		return err
	}
	arrived, err := tx.WorkEvents(w.ID, after, 0)
	if err != nil {
		return err
	}
	for _, a := range arrived {
		if domain.IsChildNews(a.Event.Type) && a.Event.Source == domain.LineageSource && slices.Contains(match.Types, a.Event.Type) {
			return nil
		}
	}
	return noChildInProgress()
}

func noChildInProgress() error {
	return &extension.OperationError{Code: CodeNoChildInProgress, Message: "you have no child Work in progress, so this wait could never end: call create_work first, then wait. Nothing was declared, so you can still make calls"}
}
