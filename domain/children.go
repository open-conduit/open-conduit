package domain

import "encoding/json"

// Child Work is Work another Work's execution asked for (Work.ParentID).
// When it ends, the host tells the parent with a TypeChildEnded Event on
// the parent's lineage key (LineageKey), so the parent can Wait for it like
// any other Event and be resumed with the child's result.
const (
	TypeChildEnded   = "core.work.child.ended"
	SchemaChildEnded = "core.work.child.ended.v1"
	// TypeChildReported tells the parent that a turn of its child handed
	// back a result while the child stays open (follow-up it still
	// answers for): an answer now, with more news or its end to come.
	TypeChildReported   = "core.work.child.reported"
	SchemaChildReported = "core.work.child.reported.v1"
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

// SubjectKey is the first of a Work's keys that says what the Work is about
// (what an outside source keyed it by): any key but its lineage key, which the host adds
// to the Work's keys partway through its life and which names only the
// Work itself. Without one, ok is false. Whatever reads a Work's first key
// as its subject (a playbook's {number}, where the Work came from) reads
// this, so a Work's subject doesn't change when it first waits or asks
// for child Work.
func SubjectKey(keys []CorrelationKey) (key CorrelationKey, ok bool) {
	for _, k := range keys {
		if k.Source != LineageSource {
			return k, true
		}
	}
	return CorrelationKey{}, false
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

// ChildReported is a TypeChildReported Event's payload: which child
// reported, the state it is in (never terminal: it is still open), the
// status and summary its turn ended with, and the turn's result.
type ChildReported struct {
	Work    string          `json:"work"`
	Kind    string          `json:"kind"`
	State   WorkState       `json:"state"`
	Status  string          `json:"status,omitempty"`
	Summary string          `json:"summary,omitempty"`
	Result  json.RawMessage `json:"result"`
}

// IsChildNews reports whether an Event type is one the host sends a parent
// about its child: its end or a report.
func IsChildNews(t string) bool { return t == TypeChildEnded || t == TypeChildReported }

// ChildCancel is a TypeChildCancel Event's payload: the parent that
// cancelled the child, the execution of it that did, and the reason it
// gave, if any. The Event's source is LineageSource, which no execution
// can emit from, so the record is always the host's.
type ChildCancel struct {
	Parent    string `json:"parent"`
	Execution string `json:"execution"`
	Reason    string `json:"reason,omitempty"`
}
