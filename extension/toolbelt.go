package extension

import (
	"context"
	"errors"
	"time"

	"github.com/open-conduit/open-conduit/domain"
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
	// Description says what the Wait is for, for operator views
	// (ValidateWaitDescription). Optional.
	Description string
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
	// Wait, set instead of Handler, translates domain wait syntax into a
	// generic plan for the current Work. The toolbelt declares the returned
	// request exactly like the builtin wait (same fence, watermark, authority
	// release and host handshake) and installs the dependencies once the Wait
	// is registered. Domain conditions never enter core: only event types,
	// correlation keys, a timeout and opaque dependency filters do.
	Wait func(context.Context, Authority, domain.Payload) (WaitPlan, error)
	// Emit, set instead of Handler or Wait, translates domain input into one
	// Event the toolbelt accepts exactly like the builtin emit (declared
	// family, authority allowlist, trusted execution source and trigger
	// causation, core chain-depth and per-chain rate limits), together with
	// the registration's own coordination records in the same transaction
	// (03-08). It grants no trusted control-plane authority.
	Emit func(context.Context, Authority, domain.Payload) (EmitPlan, error)
	// Guidance optionally tells an agent when and how to use the operation,
	// in Markdown (at most MaxGuidance bytes). The toolbelt serves it to an
	// execution only while the execution's grant includes the operation, so
	// a playbook need not describe another extension's operations.
	Guidance string
}

// MaxGuidance bounds one operation's Guidance.
const MaxGuidance = 4096

// EmitPlan is a translated emission. Event supplies only Type, DeliveryID,
// optional OccurredAt, CorrelationKey and Data; the toolbelt assigns its
// identity, source, causation and target. Type must be a family the same
// registration declared.
//
// Without a CorrelationKey, or with one the current Work owns, the Event is
// the current Work's (like the builtin emit). A key the current Work does not
// own must lie in the registration's own source namespace (Source equal to
// the registration name or beginning with name + "/"): the Event is then
// addressed to that key rather than to the emitting Work, so the
// registration's own route can give it Work of its own, while trusted source
// and causation still record which execution emitted it.
type EmitPlan struct {
	Event domain.Event
	// Cause optionally cites an accepted Event the emission derives from
	// instead of the attempt's trigger (05-03 cited causation): an Event of
	// the emitting Work's scope whose correlation key lies in the
	// registration's own source namespace (anything else is unauthorized).
	// The new Event's causation is Cause and it joins Cause's chain, but its
	// depth is one more than the deeper of Cause and the trigger, and it is
	// charged to both chains' per-minute budgets, so citing never shortens
	// a chain or escapes the trigger's limits.
	Cause domain.ID
	// Commit runs inside the accepting transaction once the Event is
	// accepted. It must be pure and deterministic. Its Records are written
	// only when the Event was newly added (a replayed delivery writes none);
	// its Output is returned to the caller either way.
	Commit func(Emitted) (EmitCommit, error)
}

// Emitted describes the accepted Event of a translated emission: Added is
// false for a delivery already accepted; Root and Depth are its causal chain
// position.
type Emitted struct {
	Event domain.AcceptedEvent
	Added bool
	Root  domain.ID
	Depth int
}

// EmitCommit is what a translated emission writes and returns.
type EmitCommit struct {
	Records []Record
	Output  domain.Payload
}

// Record is a coordination KV write. Namespace must be the registration name
// or begin with name + ".".
type Record struct {
	Namespace, Key string
	Value          []byte
}

// WaitPlan is a translated wait. Wait matches the current Work's own keys
// (empty keys mean every key the Work owns); a dependency's AliasType must
// pass its type filter. Output is returned to the caller.
type WaitPlan struct {
	Wait         WaitRequest
	Dependencies []Dependency
	Output       domain.Payload
}

// Dependency is a B04 authorized dependency subscription: Events accepted on
// SourceKey (another Work's correlation, or one no Work owns yet) are aliased
// into the waiting Work as AliasType Events with the original Event as
// causation. It never attaches the foreign key to the waiting Work.
type Dependency struct {
	// ID is stable within the plan (for example the condition text).
	ID string
	// SourceScopeID names the scope owning SourceKey; empty means the waiting
	// Work's scope. Another scope requires explicit alias authorization.
	SourceScopeID domain.ID
	SourceKey     domain.CorrelationKey
	Types         []string
	AliasType     string
	// Filter is opaque to core. When set, the registering extension's
	// DependencyFilter decides whether a type-matching source Event counts.
	Filter domain.Payload
	// Label is a short, secret-free description carried in the alias payload
	// (for example "pr:12:merged").
	Label string
	// FromNow scans only Events accepted after the wait is declared (an
	// occurrence such as "the next run ends"). Otherwise all accepted history
	// counts, so an already-met state wakes the wait at once.
	FromNow bool
}

// ContractVersion versions the in-process API. The subprocess wire protocol
// is versioned separately (protocol.Version, 07-01).
const ContractVersion uint32 = 1

type Registration struct {
	Version    uint32
	Name       string
	EventTypes []string
	Operations []Operation
	// DependencyFilter evaluates the opaque Filter of this registration's
	// dependencies against a type-matching source Event. It must be pure and
	// deterministic: core calls it inside coordination transactions.
	DependencyFilter func(filter domain.Payload, event domain.Event) bool
}

// OperationError is a refusal an extension handler authored for its caller:
// a stable code ([a-z][a-z0-9_]*) and a short message the agent can act on
// ("PR #12 is already merged: carry on now"). Both must be secret-free; the
// transport returns them verbatim instead of a generic failure.
type OperationError struct{ Code, Message string }

func (e *OperationError) Error() string { return e.Code + ": " + e.Message }

type requestIDKey struct{}

// WithCallRequestID records the caller's toolbelt request identity for an
// extension handler. Trusted toolbelt code sets it; it is empty when the
// caller supplied none.
func WithCallRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey{}, id)
}

// CallRequestID is the toolbelt request identity of the call a handler is
// serving (empty when none was supplied). Handlers making non-re-derivable
// external writes derive stable intent identities from it.
func CallRequestID(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}

// Registration is atomic. Duplicate names, reserved operations and undeclared
// operation calls fail. Builtins are reserved even when no extension is loaded.
type Registrar interface{ Register(Registration) error }
