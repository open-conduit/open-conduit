// Package domain defines the five primitives. Domain-specific data is opaque.
package domain

import (
	"encoding/json"
	"slices"
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
	ID              ID
	Kind            string
	ScopeID         ID
	State           WorkState
	CorrelationKeys []CorrelationKey
	Context         Payload
	ParentID        ID
	// Display is what the Work is, in its source's words, for operator
	// views (extension.ValidateDisplay). Core stores it and never reads it.
	Display *WorkDisplay `json:",omitempty"`
	// Grants are the binding grants the Work opted into when it was
	// created (Binding.Granted); they never change after.
	Grants               []string `json:",omitempty"`
	Revision             Revision
	CreatedAt, UpdatedAt time.Time
}

// WorkDisplay is a source's short description of a Work: a title, a short
// ref ("#136"), who asked for it and links out (an issue, a PR, a thread).
type WorkDisplay struct {
	Title, Ref, Requester string `json:",omitempty"`
	Links                 []Link `json:",omitempty"`
}

// Link is a labelled http or https URL.
type Link struct{ Label, URL string }
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
	// Description is what the Wait is for, in the waiting operation's words
	// ("checks on PR #137"). Core stores it and never reads it.
	Description string `json:",omitempty"`
	// Owner is the extension whose operation declared the Wait (a
	// translated wait); "" for a builtin wait. Only the owner may set
	// Progress, and only while the Wait is active.
	Owner string `json:",omitempty"`
	// Progress is the owner's latest one-line note on how the wait is going
	// ("3 of 7 checks finished"). Core stores it and never reads it; it
	// freezes once the Wait is woken or cancelled.
	Progress             string `json:",omitempty"`
	Revision             Revision
	CreatedAt, UpdatedAt time.Time
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
	// Grants are what the binding offers a Work beyond its own operations
	// and config, by name; a Work opts into some when it is created
	// (Work.Grants). The config file is still the ceiling.
	Grants []BindingGrant `json:",omitempty"`
}

// BindingGrant is one grant a binding offers: more toolbelt operations, and
// executor config whose top-level fields replace the binding's own.
type BindingGrant struct {
	Name, Label string
	Operations  []string        `json:",omitempty"`
	Config      json.RawMessage `json:",omitempty"`
}

// Granted is the binding as a Work that opted into names runs it: each
// grant it offers under one of those names, in the binding's order, has its
// config's top-level fields replace the binding config's own. It returns the
// names it applied; a name the binding does not offer is ignored.
func (b Binding) Granted(names []string) (Binding, []string) {
	var applied []string
	if len(names) == 0 || len(b.Grants) == 0 {
		return b, nil
	}
	cfg := map[string]json.RawMessage{}
	if len(b.Config.Data) > 0 && json.Unmarshal(b.Config.Data, &cfg) != nil {
		return b, nil
	}
	for _, g := range b.Grants {
		if !slices.Contains(names, g.Name) {
			continue
		}
		over := map[string]json.RawMessage{}
		if len(g.Config) > 0 && json.Unmarshal(g.Config, &over) != nil {
			continue
		}
		for k, v := range over {
			cfg[k] = v
		}
		applied = append(applied, g.Name)
	}
	if len(applied) == 0 {
		return b, nil
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		return b, nil
	}
	b.Config.Data = data
	return b, applied
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
