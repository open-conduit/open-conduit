package runtime

import (
	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/storage"
)

// auditAccepted adds to the audit ledger, in the transaction that accepted
// it, an Event someone caused: a control or Event an operator submitted, or
// an action a person took. The record points at the Event, which holds the
// rest (its type is the record's Action); a replayed delivery (added false)
// and an Event no one is known to have caused add nothing.
func auditAccepted(tx storage.Tx, scope domain.ID, a domain.AcceptedEvent, added bool) error {
	if !added || a.Event.Actor == "" {
		return nil
	}
	target := "scope/" + string(scope)
	if a.Event.WorkID != "" {
		target = "work/" + string(a.Event.WorkID)
	}
	_, err := tx.AppendAudit(domain.AuditRecord{Actor: a.Event.Actor, Via: a.Event.Via, Action: a.Event.Type, Target: target, Outcome: domain.AuditOK, EventID: a.Event.ID})
	return err
}
