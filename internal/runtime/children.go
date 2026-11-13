package runtime

import (
	"encoding/json"
	"errors"
	"slices"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/storage"
)

// claimLineageInTx gives parent its lineage key (domain.LineageKey) when it
// does not own it yet, so its children's domain.TypeChildEnded Events are
// correlated to it and a Wait on its keys sees them. It returns the Work as
// stored.
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
// outcome and result. It is derived from the child's last trigger (its
// latest attempt's, else its creation), so a chain of Work asking Work stays
// one causal chain and the causal depth limit ends a loop. Work with no
// parent, Work that is not terminal, and a parent that never claimed its
// lineage key (Work an operator made a child of another) get nothing. A
// refusal by the causal limits is recorded and never blocks the child's end.
func childEndedInTx(tx storage.Tx, now time.Time, limits CausalLimits, child domain.Work) error {
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
		CorrelationKey: &key, WorkID: parent.ID, OccurredAt: now, ReceivedAt: now, Data: domain.Payload{Schema: domain.SchemaChildEnded, Data: data}}
	if trigger == "" {
		_, _, err = AcceptInTx(tx, parent.ScopeID, e)
		return err
	}
	_, _, err = AcceptDerivedInTx(tx, now, limits, parent.ScopeID, trigger, e)
	if errors.Is(err, ErrCausalRejected) {
		return nil
	}
	return err
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
