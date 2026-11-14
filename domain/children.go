package domain

import "encoding/json"

// Child Work is Work another Work's execution asked for (Work.ParentID).
// When it ends, the host tells the parent with a TypeChildEnded Event on
// the parent's lineage key (LineageKey), so the parent can Wait for it like
// any other Event and be resumed with the child's result.
const (
	TypeChildEnded   = "core.work.child.ended"
	SchemaChildEnded = "core.work.child.ended.v1"
	// TypeChildCancel records, on the child, that the Work that asked for
	// it cancelled it (cancel_work), and why.
	TypeChildCancel   = "core.work.child.cancel"
	SchemaChildCancel = "core.work.child.cancel.v1"
	// LineageSource is the correlation source of a Work's lineage key.
	LineageSource = "core.work"
)

// LineageKey is the Work's own correlation key: its ID under LineageSource.
// The host gives it to a Work when it first asks for child Work, so its
// children's TypeChildEnded Events carry it and a Wait on the parent's keys
// sees them, and to Work that owns no key when it first waits, so it has a
// key for what wakes it to arrive on.
func LineageKey(work ID) CorrelationKey {
	return CorrelationKey{Source: LineageSource, ExternalID: string(work)}
}

// ChildEnded is a TypeChildEnded Event's payload: which child ended, in
// which state, and what its last attempt returned. Result is the child's
// declared turn result (TurnDeclaration.Result), absent when it returned
// none (a failed or cancelled child).
type ChildEnded struct {
	Work    string          `json:"work"`
	Kind    string          `json:"kind"`
	State   WorkState       `json:"state"`
	Outcome OutcomeKind     `json:"outcome,omitempty"`
	Summary string          `json:"summary,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
}

// ChildCancel is a TypeChildCancel Event's payload: the parent that
// cancelled the child, the execution of it that did, and the reason it
// gave, if any. The Event's source is LineageSource, which no execution
// can emit from, so the record is always the host's.
type ChildCancel struct {
	Parent    string `json:"parent"`
	Execution string `json:"execution"`
	Reason    string `json:"reason,omitempty"`
}
