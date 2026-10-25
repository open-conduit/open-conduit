// Package domain defines the five primitives. Domain-specific data is opaque.
package domain

import (
	"encoding/json"
	"time"
)

type ID string
type Revision uint64

// Payload.Schema is an opaque namespaced, versioned identifier. Data is JSON.
type Payload struct {
	Schema string
	Data   json.RawMessage
}
type CorrelationKey struct{ Source, ExternalID string }
type WorkState string

const (
	WorkReady     WorkState = "ready"
	WorkRunning   WorkState = "running"
	WorkWaiting   WorkState = "waiting"
	WorkCompleted WorkState = "completed"
	WorkFailed    WorkState = "failed"
	WorkCancelled WorkState = "cancelled"
)

func (s WorkState) Terminal() bool {
	return s == WorkCompleted || s == WorkFailed || s == WorkCancelled
}
func WorkTransition(from, to WorkState) bool {
	if from.Terminal() {
		return false
	}
	switch from {
	case WorkReady:
		return to == WorkRunning || to == WorkCancelled || to == WorkFailed
	case WorkRunning:
		return to == WorkReady || to == WorkWaiting || to == WorkCompleted || to == WorkFailed || to == WorkCancelled
	case WorkWaiting:
		return to == WorkReady || to == WorkRunning || to == WorkFailed || to == WorkCancelled
	}
	return false
}

type Work struct {
	ID                   ID
	Kind                 string
	ScopeID              ID
	State                WorkState
	CorrelationKeys      []CorrelationKey
	Context              Payload
	ParentID             ID
	Revision             Revision
	CreatedAt, UpdatedAt time.Time
}
type Event struct {
	ID                               ID
	Type, Source, DeliveryID         string
	OccurredAt, ReceivedAt           time.Time
	CorrelationKey                   *CorrelationKey
	CausationID, WorkID, ExecutionID ID
	Data                             Payload
}

// AcceptedEvent adds storage metadata without changing the immutable envelope.
// Sequence orders committed acceptance; routing may associate a work later.
type AcceptedEvent struct {
	Event        Event
	Sequence     uint64
	Routed       bool
	RoutedWorkID ID
	Disposition  string
}
type ExecutionState string

const (
	ExecutionPending   ExecutionState = "pending"
	ExecutionRunning   ExecutionState = "running"
	ExecutionParked    ExecutionState = "parked"
	ExecutionCompleted ExecutionState = "completed"
	ExecutionFailed    ExecutionState = "failed"
	ExecutionWaiting   ExecutionState = "waiting"
	ExecutionHandoff   ExecutionState = "handoff"
	ExecutionCancelled ExecutionState = "cancelled"
)

func (s ExecutionState) Terminal() bool {
	return s == ExecutionCompleted || s == ExecutionFailed || s == ExecutionWaiting || s == ExecutionHandoff || s == ExecutionCancelled
}
func ExecutionTransition(from, to ExecutionState) bool {
	switch from {
	case ExecutionPending:
		return to == ExecutionRunning || to == ExecutionCancelled
	case ExecutionRunning:
		return to == ExecutionParked || to.Terminal()
	case ExecutionParked:
		return to == ExecutionRunning || to == ExecutionCancelled || to == ExecutionFailed
	}
	return false
}

type OutcomeKind string

const (
	OutcomeCompleted OutcomeKind = "completed"
	OutcomeFailed    OutcomeKind = "failed"
	OutcomeWaiting   OutcomeKind = "waiting"
	OutcomeHandoff   OutcomeKind = "handoff"
)

type Outcome struct {
	Kind    OutcomeKind
	Summary string
	Data    Payload
}
type Lease struct {
	Owner     string
	Epoch     uint64
	ExpiresAt time.Time
}
type LeaseRef struct {
	ExecutionID ID
	Owner       string
	Epoch       uint64
}
type Execution struct {
	Artifacts             []ArtifactRef
	StartedAt, FinishedAt *time.Time
	ID, WorkID            ID
	ExecutorBinding       string
	Attempt               uint32
	State                 ExecutionState
	TriggerID             ID
	Lease                 Lease
	Outcome               *Outcome
	TraceID               string
	Revision              Revision
	CreatedAt, UpdatedAt  time.Time
}

// ArtifactRef points to separately published immutable content; it is not a primitive.
type ArtifactRef struct {
	ID        string
	Size      int64
	MediaType string
}
type Timer struct {
	ID                   ID
	FireAt               time.Time
	Event                Event
	Revision             Revision
	CreatedAt, UpdatedAt time.Time
}

// EventMatch contains no domain condition language. Keys must belong to Work.
type EventMatch struct {
	Keys  []CorrelationKey
	Types []string
}
type WaitState string

const (
	WaitActive    WaitState = "active"
	WaitWoken     WaitState = "woken"
	WaitCancelled WaitState = "cancelled"
)

type Wait struct {
	State                   WaitState
	ID, WorkID, ExecutionID ID
	Match                   EventMatch
	TimeoutTimerID          ID
	AfterSequence           uint64
	WakeEventID             ID
	Revision                Revision
	CreatedAt, UpdatedAt    time.Time
}
type Capabilities struct {
	Interrupt, Inject, Resume bool
	Toolbelt                  string
	Workspace                 bool
}
type Binding struct {
	Name, Executor string
	Requires       Capabilities
	// TimeoutSeconds, when positive, replaces the runtime's default execution
	// deadline for attempts of this binding. Zero inherits the default.
	TimeoutSeconds int `json:",omitempty"`
	Config         Payload
}
type Route struct {
	Name, Handler, Binding, ActivePolicy string
	Config                               Payload
}
type Scope struct {
	ID                   ID
	Name                 string
	ParentID             ID
	Bindings             []Binding
	Routes               []Route
	Config               Payload
	SecretRefs           map[string]string
	Revision             Revision
	CreatedAt, UpdatedAt time.Time
}
