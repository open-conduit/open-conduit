package domain

import "time"

// TypeExecutionUsage is the Event the host accepts for each landed result
// that used something, in the transaction that stores the result.
// SchemaExecutionUsage is its payload (UsageRecord). Its sequence is the
// cursor a collector follows (GET /v1/events?type=core.execution.usage).
const (
	TypeExecutionUsage   = "core.execution.usage"
	SchemaExecutionUsage = "core.execution.usage.v1"
)

// UsageContainer is the container a usage record's segment ran in.
type UsageContainer struct {
	Provider string `json:"provider,omitempty"`
	Image    string `json:"image,omitempty"`
	Digest   string `json:"digest,omitempty"`
	ID       string `json:"id,omitempty"`
}

// UsageRecord is what one landed result added to its attempt's usage. The
// records of an attempt sum to its usage. Amounts are as the executor
// reported them, by unit: a cost is the executor's figure, which may not be
// what anyone is billed. Attribution is the executor's opaque description of
// what did the work (a model, a provider); the core reads no meaning into it.
type UsageRecord struct {
	Work        string             `json:"work"`
	Execution   string             `json:"execution"`
	Scope       string             `json:"scope"`
	Binding     string             `json:"binding,omitempty"`
	Executor    string             `json:"executor,omitempty"`
	Segment     int                `json:"segment"`
	StartedAt   *time.Time         `json:"started_at,omitempty"`
	EndedAt     *time.Time         `json:"ended_at,omitempty"`
	Container   *UsageContainer    `json:"container,omitempty"`
	Amounts     map[string]float64 `json:"amounts"`
	Attribution map[string]string  `json:"attribution,omitempty"`
}
