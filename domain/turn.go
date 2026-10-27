package domain

import (
	"encoding/json"
	"strings"
)

// TurnSchema is the outcome data schema an execution uses to declare the
// state an agent's turn ended in and, optionally, one comment to publish.
// Every projector reads it, whatever source the Work came from.
const TurnSchema = "core.turn.v1"

// TypeTurnEnded is the Event the host publishes, on each of the Work's
// correlation keys, when an execution ends with a TurnSchema outcome.
// SchemaTurnEnded is its payload (TurnEnded).
const (
	TypeTurnEnded   = "core.execution.turn"
	SchemaTurnEnded = "core.execution.turn.v1"
)

// TurnStates are the states a turn ends in.
var TurnStates = []string{"building", "review", "help", "done", "finished"}

// TurnDeclaration is the TurnSchema payload (unknown fields, such as an
// executor's usage, are ignored).
type TurnDeclaration struct {
	Status  string `json:"status,omitempty"`
	Comment string `json:"comment,omitempty"`
}

// TurnEnded is a TypeTurnEnded Event's payload.
type TurnEnded struct {
	Status    string `json:"status"`
	Work      string `json:"work"`
	Execution string `json:"execution"`
}

// IsTurnSchema reports whether an outcome schema declares a turn: TurnSchema,
// or for one release the source-named "<source>.status.v1" outcomes already
// stored under it.
func IsTurnSchema(schema string) bool {
	return schema == TurnSchema || strings.HasSuffix(schema, ".status.v1") && len(schema) > len(".status.v1")
}

// TurnOf returns an outcome's turn declaration, if it declares one.
func TurnOf(o *Outcome) (TurnDeclaration, bool) {
	var d TurnDeclaration
	if o == nil || !IsTurnSchema(o.Data.Schema) || json.Unmarshal(o.Data.Data, &d) != nil {
		return d, false
	}
	return d, true
}

// ValidTurnState reports whether s is one of TurnStates.
func ValidTurnState(s string) bool {
	for _, t := range TurnStates {
		if s == t {
			return true
		}
	}
	return false
}
