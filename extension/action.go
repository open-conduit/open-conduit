package extension

import (
	"context"

	"github.com/open-conduit/open-conduit/domain"
)

// Work actions (issue #51). An extension declares the actions a person may
// take on a target now, and authorises each invocation itself. An operator
// surface lists and invokes them by name: it never writes an Event, and it
// holds no domain knowledge.
//
// The host calls Invoke with the target, the input and the caller's
// identity. The provider authorises the call (is this caller allowed, is
// what it answers still outstanding, is the input valid) and either refuses
// with an *OperationError, which reaches the caller verbatim and leaves no
// Event, or returns the one Event the action produces. The host accepts
// that Event under ActionSource as the root of its own chain; the provider
// chooses its family, correlation key and data, never its source or
// identity. Because ActionSource is assigned only on this path, posting an
// Event of the same family and key through the management API confers none
// of an action's authority.

// ActionSource is the source of every Event an action produces.
const ActionSource = "core.action"

// CallerOperator is the kind of the local operator: the owner of the
// management socket.
const CallerOperator = "operator"

// Caller is who invokes an action. The local management API always calls
// as {Kind: CallerOperator, ID: "local"}; surfaces with real identities
// carry their own. Providers refuse callers they do not recognise.
type Caller struct {
	Kind, ID string
}

// LocalOperator is the caller of every management API invocation.
var LocalOperator = Caller{Kind: CallerOperator, ID: "local"}

// ActionTarget is what an action applies to: a scope, and the Work in it
// when the action is on Work. Scope-level actions (Work nil) are for things
// that have no Work yet, such as claiming an issue by hand.
type ActionTarget struct {
	Scope domain.Scope
	Work  *domain.Work
}

// Action is one action available on a target now. ID is stable
// ([a-z][a-z0-9_]*) and is what callers invoke; Label is what a person
// reads. Prompt, when set, asks for one line of input (the action is then
// invoked with that text); an action without a Prompt takes none. Choices,
// when set with a Prompt, are the inputs the action accepts: a view offers
// one button per choice rather than a text box.
type Action struct {
	ID      string
	Label   string
	Prompt  string
	Choices []string
}

// ActionCall is one invocation.
type ActionCall struct {
	Target ActionTarget
	Action string
	Input  string
	Caller Caller
}

// ActionPlan is an authorised invocation: the Event to accept (Type,
// CorrelationKey and Data only; the host assigns identity, source, delivery
// and causation) and an optional short, secret-free Message for the caller.
// An Event with a CorrelationKey is addressed by that key; without one, an
// action on Work produces an Event addressed to that Work.
type ActionPlan struct {
	Event   domain.Event
	Message string
}

// ActionProvider lists and authorises an extension's actions. Actions
// returns only what is available on the target now: an action that does not
// apply is not listed. Invoke must refuse (with *OperationError) anything
// it would not list, and must not write state itself.
type ActionProvider interface {
	Actions(ctx context.Context, t ActionTarget) ([]Action, error)
	Invoke(ctx context.Context, c ActionCall) (ActionPlan, error)
}
