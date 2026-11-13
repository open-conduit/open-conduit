package domain

import "time"

// TypeExecutionTimeout is the Event the host delivers when a turn reaches
// its binding's time limit. An attempt that can be interrupted and resumed
// has its turn ended and gets the Event as its next attempt's input; any
// other attempt fails. SchemaExecutionTimeout is its payload
// (ExecutionTimeout).
const (
	TypeExecutionTimeout   = "core.execution.timeout"
	SchemaExecutionTimeout = "core.execution.timeout.v1"
)

// ExecutionTimeout is a TypeExecutionTimeout Event's payload: the
// execution, the deadline its turn passed and, when known, that turn's time
// limit in seconds.
type ExecutionTimeout struct {
	ExecutionID    ID
	Deadline       time.Time
	TimeoutSeconds int64 `json:",omitempty"`
}
