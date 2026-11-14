package domain

import "encoding/json"

// TurnSchema is the outcome data schema an execution uses to declare the
// status its turn ended in and, optionally, one comment to publish. The core
// knows no status vocabulary: what a status means (a label, a hold, a
// workflow transition) belongs to whoever reads it, and an executor may end a
// turn in any status of the right shape (ValidTurnStatus).
const TurnSchema = "core.turn.v1"

// TypeTurnEnded is the Event the host publishes, on each of the Work's
// correlation keys, when an execution ends with a TurnSchema outcome.
// SchemaTurnEnded is its payload (TurnEnded).
const (
	TypeTurnEnded   = "core.execution.turn"
	SchemaTurnEnded = "core.execution.turn.v1"
)

// TurnDeclaration is the TurnSchema payload (unknown fields, such as an
// executor's usage, are ignored).
type TurnDeclaration struct {
	Status  string `json:"status,omitempty"`
	Comment string `json:"comment,omitempty"`
	// Result is what the turn hands back to whoever asked for the Work:
	// for child Work, the parent receives it in its TypeChildEnded Event
	// when the turn ends the Work, else in a TypeChildReported Event.
	// Any JSON value (a string answer, an object of the caller's shape).
	Result json.RawMessage `json:"result,omitempty"`
}

// HasResult reports whether the declaration carries a result: present, not
// null and not an empty string.
func (d TurnDeclaration) HasResult() bool {
	r := string(d.Result)
	return r != "" && r != "null" && r != `""`
}

// TurnEnded is a TypeTurnEnded Event's payload.
type TurnEnded struct {
	Status    string `json:"status"`
	Work      string `json:"work"`
	Execution string `json:"execution"`
}

// TurnOf returns an outcome's turn declaration, if it declares one.
func TurnOf(o *Outcome) (TurnDeclaration, bool) {
	var d TurnDeclaration
	if o == nil || o.Data.Schema != TurnSchema || json.Unmarshal(o.Data.Data, &d) != nil {
		return d, false
	}
	return d, true
}

// MaxTurnStatus bounds a turn status's length.
const MaxTurnStatus = 64

// ValidTurnStatus reports whether s has the shape of a turn status: a
// lower-case letter, then up to MaxTurnStatus-1 lower-case letters, digits,
// '-' or '_'. It checks the shape only; any such word is a status.
func ValidTurnStatus(s string) bool {
	if len(s) == 0 || len(s) > MaxTurnStatus || s[0] < 'a' || s[0] > 'z' {
		return false
	}
	for i := 1; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}
