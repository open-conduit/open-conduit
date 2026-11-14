package extension

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/open-conduit/open-conduit/domain"
)

var (
	ErrInvalid      = errors.New("invalid extension contract")
	ErrUnsupported  = errors.New("unsupported capability")
	ErrUnauthorized = errors.New("execution authority rejected")
	ErrUndeclared   = errors.New("undeclared operation or event")
	ErrConflict     = errors.New("registration conflict")
	ErrNotAdoptable = errors.New("execution cannot be adopted")
	// ErrAuthorityReleased refuses a call made with a genuine token after
	// its execution gave up authority for the segment by declaring a wait
	// (directly, through an operation that waits, or a gate that parked
	// the call) or its outcome. It wraps ErrUnauthorized, so everything that
	// refuses an unauthorized call refuses it too; it only lets the caller
	// tell "already parked" apart from "not authorised".
	ErrAuthorityReleased = fmt.Errorf("%w: authority released by a declared wait or outcome", ErrUnauthorized)
	// ErrAuthorityRecovering refuses a call, before it runs, made with a
	// genuine token whose execution is still running but whose lease
	// lapsed under it (the host slept or stalled past the lease) or which
	// recovery has already superseded with a newer epoch's token. Recovery
	// adopts the same process with fresh authority, so the same call made
	// again with the execution's current credential is accepted. It wraps
	// ErrUnauthorized, so the call is refused all the same; it only lets
	// the caller tell "retry shortly" apart from "not authorised".
	ErrAuthorityRecovering = fmt.Errorf("%w: lease lapsed, recovery is restoring authority", ErrUnauthorized)
)

const (
	Emit           = "emit"
	CreateWork     = "create_work"
	CancelWork     = "cancel_work"
	Wait           = "wait"
	DeclareOutcome = "declare_outcome"
	ReadContext    = "read_context"
)

// BuiltinOperation reports whether name is one of the toolbelt's builtin
// operations, which no extension may declare and no gate decides on.
func BuiltinOperation(name string) bool {
	switch name {
	case Emit, CreateWork, CancelWork, Wait, DeclareOutcome, ReadContext:
		return true
	}
	return false
}

// Authority is resolved by trusted code from the token, never client-supplied.
// Every call, including reads, checks expiry/revocation and the current lease.
type Authority struct {
	ScopeID, WorkID, ExecutionID domain.ID
	Lease                        domain.LeaseRef
	ExpiresAt                    time.Time
	Operations, EventTypes       []string
	TriggerID                    domain.ID
	// Connection is the connection ID the Execution's binding names, or ""
	// when it names none. The host lets the extension that owns the
	// connection's type resolve its config and secrets in the scope (see
	// the extension protocol's configure and secret.resolve).
	Connection string
	// WorkKinds are the kinds of child Work the execution may ask for with
	// create_work, each with what to ask it for; the host refuses any
	// other kind. Empty grants none.
	WorkKinds []WorkKind `json:",omitempty"`
}

// WorkKind is one kind of child Work an execution may ask for, and the
// operator's description of what to ask it for.
type WorkKind struct {
	Kind, Description string
}

// GrantsWorkKind reports whether a lets its execution ask for child Work of
// kind.
func (a Authority) GrantsWorkKind(kind string) bool {
	for _, k := range a.WorkKinds {
		if k.Kind == kind {
			return true
		}
	}
	return false
}

type ToolCall struct {
	Operation string
	// Optional assertions may narrow authority, never select another target.
	ScopeID, WorkID, ExecutionID domain.ID
	Event                        *domain.Event
	Child                        *domain.Work
	Cancel                       *CancelRequest
	Wait                         *WaitRequest
	Outcome                      *domain.Outcome
	Input                        domain.Payload
	// Capability says the call is made for a capability's client (git's
	// credential helper asks with CapabilityGitCredential), not by the
	// agent. An operation that serves a capability (Operation.Serves) is
	// served only to a call made for that capability, so the agent's own
	// toolbelt calls (the MCP call tool, os call) don't get its result; a
	// call naming a capability its operation doesn't serve is refused too.
	// The client declares Capability itself, and runs in the agent's
	// sandbox with the agent's token, so this is not an authority
	// boundary: whatever runs there can make the client's call.
	Capability string
}

// CancelRequest is cancel_work's input: the child Work to cancel and,
// optionally, why (at most MaxCancelReason bytes). The host allows it only
// for Work that is not yet terminal and whose parent is the caller's own
// Work, and refuses any other as unauthorized (a Work that has already
// ended with OperationError code CodeWorkEnded). The child ends cancelled,
// so its parent hears of it with domain.TypeChildEnded like any other end.
type CancelRequest struct {
	Work   domain.ID
	Reason string
}

// MaxCancelReason bounds CancelRequest.Reason.
const MaxCancelReason = 1024

// CodeWorkEnded is the OperationError code of a cancel_work refused because
// the child Work has already ended.
const CodeWorkEnded = "work_ended"

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
	// Summary says in one line what the operation does (at most
	// MaxSummary bytes, no line breaks), for the operations reference and
	// the agents granted it.
	Summary string
	// Input is the JSON Schema of the input's data, an object
	// (InputSchema derives it from the struct a handler decodes). The
	// toolbelt serves it with Summary; it does not check calls against it.
	Input json.RawMessage
	// Handler receives validated authority, not a bearer token or secrets.
	// It must be idempotent on the call's request ID (CallRequestID): the
	// toolbelt records a result for replay only after Handler returns, so a
	// crash between Handler's effect and that record runs Handler again when
	// the caller retries with the same request ID.
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
	// Serves, on a Handler, names a capability the operation answers, such
	// as CapabilityGitCredential. A client that needs the capability asks
	// the toolbelt which of the operations its grant includes serve it
	// (Serving) and calls each in turn until one answers, so the host routes
	// a capability without knowing which extension's domain it belongs to.
	// Such an operation is served only to a call made for the capability
	// (ToolCall.Capability), not to an agent's own toolbelt call, and the toolbelt
	// neither describes it to agents nor keeps its result for replay: what
	// it answers is for the client, and is often a secret.
	Serves string
}

// OperationNames are ops' names, in order.
func OperationNames(ops []Operation) []string {
	out := make([]string, 0, len(ops))
	for _, op := range ops {
		out = append(out, op.Name)
	}
	return out
}

// CapabilityGitCredential is git's credential helper as a capability: input
// {protocol, host, path?}, git's credential request; output {username,
// password, expires_at?} for a remote the operation's extension owns, or {}
// for any other, so the caller can ask the next operation serving it.
const CapabilityGitCredential = "git.credential"

// CodeCapabilityOnly is the OperationError code of a call refused because
// it was not made for the capability its operation serves, or named one
// its operation doesn't serve.
const CodeCapabilityOnly = "capability_only"

// CapabilityCall says, in an agent's own terms, what a call made for a
// capability's client is, so that whatever holds, refuses or approves one
// can tell the agent what to do: the agent didn't make the call, its client
// (git, for CapabilityGitCredential) did.
type CapabilityCall struct {
	// Subject names the call ("git's sign-in to github.com/acme/app"), at
	// most MaxCapabilitySubject bytes, on one line.
	Subject string
	// Again says how the agent makes the call again ("Run the same git
	// command again"), without a full stop.
	Again string
	// Direct says why an agent's own call of an operation serving the
	// capability is refused and what to do instead.
	Direct string
}

// MaxCapabilitySubject bounds CapabilityCall.Subject.
const MaxCapabilitySubject = 200

// DescribeCapabilityCall is CapabilityCall for a call made for capability
// with input. A capability it doesn't know gets generic wording.
func DescribeCapabilityCall(capability string, input domain.Payload) CapabilityCall {
	switch capability {
	case CapabilityGitCredential:
		var in struct{ Host, Path string }
		_ = json.Unmarshal(input.Data, &in)
		where := in.Host
		if p := strings.Trim(in.Path, "/"); p != "" {
			where += "/" + p
		}
		subject := "git's sign-in"
		if where = oneLine(where); where != "" {
			subject += " to " + where
		}
		return CapabilityCall{Subject: clip(subject, MaxCapabilitySubject), Again: "Run the same git command again",
			Direct: "git signs in by itself: run the git command, and its credential helper asks for the token. Don't call this operation; its token is never handed to you"}
	}
	return CapabilityCall{Subject: clip("a call your toolbelt made for you ("+oneLine(capability)+")", MaxCapabilitySubject), Again: "Do again what made it",
		Direct: "this operation answers the " + oneLine(capability) + " capability for your toolbelt's client, which calls it for you. Don't call it yourself"}
}

// oneLine drops control characters, so a call's input can't break the line
// it is shown on.
func oneLine(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
}

// clip cuts s to at most n bytes, on a rune boundary.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n - len("…")
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

// MaxOnce bounds EmitPlan.Once.
const MaxOnce = 1024

// MaxGuidance bounds one operation's Guidance.
const MaxGuidance = 4096

// MaxSummary bounds one operation's Summary.
const MaxSummary = 200

// ValidateOperationDoc checks an operation's Summary and Input: a summary
// of one line within MaxSummary, and an input that is empty or a JSON
// Schema object.
func ValidateOperationDoc(summary string, input json.RawMessage) error {
	if len(summary) > MaxSummary || strings.ContainsAny(summary, "\r\n") {
		return fmt.Errorf("%w: summary must be one line of at most %d bytes", ErrInvalid, MaxSummary)
	}
	if len(input) == 0 {
		return nil
	}
	var schema map[string]any
	if err := json.Unmarshal(input, &schema); err != nil || schema == nil {
		return fmt.Errorf("%w: input must be a JSON Schema object", ErrInvalid)
	}
	return nil
}

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
	// Once, when set, is the emission's identity across executions: at most
	// one Event is accepted per registration, scope and Once. The toolbelt
	// records Once with the accepted Event in the accepting transaction. A
	// later plan with the same Once and the same Event (type, correlation
	// key and payload) accepts nothing, spends no causal budget and commits
	// as a duplicate (Added false) of the first; one with a different Event
	// is refused with OperationError code "once_conflict". So Once names a
	// revision, not a thing that changes: an extension recording edits puts
	// the revision in Once. At most MaxOnce bytes.
	Once string
	// Commit runs inside the accepting transaction once the Event is
	// accepted. It must be pure and deterministic. Its Records are written
	// only when the Event was newly added (a replayed delivery writes none);
	// its Output is returned to the caller either way. A nil Commit accepts
	// the Event, writes nothing and returns no Output.
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

// ErrFilterFailed marks a DependencyFilter error the extension itself
// returned about the Event: it was reached and answered, but with a
// failure (a panic in its handler, say), not with a match or a non-match.
// Core bounds how long it asks again about such an Event. Any other error
// means the filter couldn't be reached (a timeout, a transport failure, an
// overloaded or restarting process); core asks again for as long as that
// lasts, since passing a one-time occurrence by would strand the wait.
var ErrFilterFailed = errors.New("dependency filter failed on the event")

// PureFilter is a Registration.DependencyFilter that always answers: an
// in-process filter that decides from the filter and the Event alone.
func PureFilter(fn func(filter domain.Payload, event domain.Event) bool) func(domain.Payload, domain.Event) (bool, error) {
	return func(filter domain.Payload, event domain.Event) (bool, error) { return fn(filter, event), nil }
}

type Registration struct {
	Version uint32
	// Name is the extension's name and the namespace of everything it
	// registers: every operation is Name + "." + a suffix (OwnsOperation).
	// It is [a-z][a-z0-9_-]* (RegistrationName), with no dot, so no two
	// extensions' namespaces nest; an installed package's is its manifest
	// name, which follows the same rule.
	Name       string
	EventTypes []string
	// SealedEventTypes are those of EventTypes only this registration's
	// own Emit operations may emit: the builtin emit refuses them, as it
	// would an undeclared type, though the caller's authority lists them.
	// A route can then trust that such an Event passed the operation's
	// checks, so an executor can't forge one with the builtin emit.
	SealedEventTypes []string
	Operations       []Operation
	// DependencyFilter evaluates the opaque Filter of this registration's
	// dependencies against a type-matching source Event. Its answer must be
	// pure and deterministic: core calls it inside coordination
	// transactions. An error means it could not answer now (a remote
	// extension that timed out or is restarting), not that the Event
	// doesn't match: core asks again about the same Event later rather than
	// passing it by, since a dependency such as a merge happens only once.
	// A filter that answers in process returns no error (PureFilter).
	DependencyFilter func(filter domain.Payload, event domain.Event) (bool, error)
	// Gate, when set, is consulted before every call of another
	// registration's operation, after the toolbelt has checked the caller's
	// authority and before the operation runs. It is how an extension puts
	// an operator's policy (an approval, say) in the host's path rather
	// than in an executor's instructions. See Gate.
	Gate Gate
	// StartGate, when set, is consulted before the first attempt of any
	// Work starts, so an extension can put an operator's policy (a
	// person's approval before Work of a kind runs) in the scheduler's
	// path. See StartGate.
	StartGate StartGate
}

// StartGate decides whether Work's first attempt may start. It returns nil
// and no error to let it start, or a StartHold to hold the Work instead:
// it stays ready, its input keeps queueing, and no attempt starts until the
// holding registration settles the hold (inproc.Host.SettleHold), which
// either lets exactly one start through or cancels the Work. An error means
// the gate could not decide: the start is refused for now and asked again
// on the next pass (the host fails closed). The scheduler asks every
// registered start gate, in registration-name order, outside any
// transaction, and stops at the first that holds. Later attempts of the
// same Work are not asked.
type StartGate func(context.Context, StartCall) (*StartHold, error)

// StartCall is the start a StartGate decides on: the Work (its kind, scope
// and context), the binding its first attempt would run on and the Event
// that would be its first input.
type StartCall struct {
	Work    domain.Work
	Binding string
	Event   domain.Event
}

// StartHold is a StartGate's hold: why the Work is held, in words an
// operator or the person who can release it reads.
type StartHold struct {
	Reason string
}

// Gate decides whether a call may proceed. It returns nil and no error to
// let the call through; a WaitPlan to park the calling execution instead
// (the toolbelt declares it exactly like a translated wait owned by the
// gating registration, so its dependencies use that registration's
// DependencyFilter, and the operation does not run); or an error to refuse
// the call, an OperationError reaching the caller verbatim. The toolbelt
// asks every registered gate in registration-name order and stops at the
// first that parks or refuses. A gate that cannot decide must refuse: the
// host fails closed. A registration's own operations never pass its own
// gate, and builtin operations pass no gate.
type Gate func(context.Context, Authority, GateCall) (*WaitPlan, error)

// GateCall is the call a Gate decides on.
type GateCall struct {
	// Operation is the operation called and Owner the registration that
	// serves it.
	Operation, Owner string
	Input            domain.Payload
	// Capability is the capability the call was made for (ToolCall.
	// Capability), "" for an agent's own call; DescribeCapabilityCall says
	// what to tell the agent about it.
	Capability string
}

// RegistrationName reports whether name may name a registration: a
// lower-case letter, then up to 62 lower-case letters, digits, hyphens or
// underscores (the manifest name rule).
func RegistrationName(name string) bool {
	if name == "" || len(name) > 63 || name[0] < 'a' || name[0] > 'z' {
		return false
	}
	for _, c := range name {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

// OwnsOperation reports whether an operation name is in the namespace of
// the registration named name: name + "." + a non-empty suffix. A
// toolbelt refuses any other, so an extension that registers first can't
// take the calls meant for another (an installed package claiming
// github.*).
func OwnsOperation(name, op string) bool {
	rest, ok := strings.CutPrefix(op, name+".")
	return ok && rest != ""
}

// OperationError is a refusal an extension handler authored for its caller:
// a stable code ([a-z][a-z0-9_]*) and a short message the agent can act on
// ("PR #12 is already merged: carry on now"). Both must be secret-free; the
// transport returns them verbatim instead of a generic failure.
type OperationError struct{ Code, Message string }

func (e *OperationError) Error() string { return e.Code + ": " + e.Message }

// GateRefusal wraps a Gate's refusal, so the toolbelt can record that a
// gate (and which) stopped the call. Unwrap gives the gate's own error,
// which the caller receives.
type GateRefusal struct {
	Gate string
	Err  error
}

func (e *GateRefusal) Error() string { return e.Err.Error() }
func (e *GateRefusal) Unwrap() error { return e.Err }

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

// Registration is atomic. An operation outside the registration's own
// namespace (OwnsOperation) is ErrInvalid; a duplicate registration name,
// a reserved (builtin) operation or one another registration holds is
// ErrConflict. Each refusal names the operation and why. Undeclared
// operation calls fail. Builtins are reserved even when no extension is
// loaded.
type Registrar interface{ Register(Registration) error }
