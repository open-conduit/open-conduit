package extension

import (
	"context"
	"errors"
	"time"

	"github.com/alsiola/open-conduit/domain"
)

var (
	ErrInvalid      = errors.New("invalid extension contract")
	ErrUnsupported  = errors.New("unsupported capability")
	ErrUnauthorized = errors.New("execution authority rejected")
	ErrUndeclared   = errors.New("undeclared operation or event")
	ErrConflict     = errors.New("registration conflict")
	ErrNotAdoptable = errors.New("execution cannot be adopted")
)

const (
	Emit           = "emit"
	CreateWork     = "create_work"
	Wait           = "wait"
	DeclareOutcome = "declare_outcome"
	ReadContext    = "read_context"
)

// Authority is resolved by trusted code from the token, never client-supplied.
// Every call, including reads, checks expiry/revocation and the current lease.
type Authority struct {
	ScopeID, WorkID, ExecutionID domain.ID
	Lease                        domain.LeaseRef
	ExpiresAt                    time.Time
	Operations, EventTypes       []string
	TriggerID                    domain.ID
}
type ToolCall struct {
	Operation string
	// Optional assertions may narrow authority, never select another target.
	ScopeID, WorkID, ExecutionID domain.ID
	Event                        *domain.Event
	Child                        *domain.Work
	Wait                         *WaitRequest
	Outcome                      *domain.Outcome
	Input                        domain.Payload
}
type WaitRequest struct {
	Match         domain.EventMatch
	Timeout       *time.Time
	AfterSequence uint64
}
type ToolResult struct {
	Event   *domain.AcceptedEvent
	Work    *domain.Work
	Wait    *domain.Wait
	Context domain.Payload
	Output  domain.Payload
}
type Toolbelt interface {
	Call(context.Context, string, ToolCall) (ToolResult, error)
}

// TokenIssuer is a trusted runtime surface, never exposed to executions.
// Issue uses fresh unpredictable tokens. Revoke is idempotent. A new lease epoch
// requires a new token; parked/terminal executions immediately lose authority.
type TokenIssuer interface {
	Issue(context.Context, Authority) (string, error)
	Revoke(context.Context, string) error
}
type Operation struct {
	Name string
	// Handler receives validated authority, not a bearer token or secrets.
	Handler func(context.Context, Authority, domain.Payload) (domain.Payload, error)
}

// ContractVersion versions the in-process API, not the deferred subprocess protocol.
const ContractVersion uint32 = 1

type Registration struct {
	Version    uint32
	Name       string
	EventTypes []string
	Operations []Operation
}

// Registration is atomic. Duplicate names, reserved operations and undeclared
// operation calls fail. Builtins are reserved even when no extension is loaded.
type Registrar interface{ Register(Registration) error }
