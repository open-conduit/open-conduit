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

// String is the caller as kind:id, or "" for no caller.
func (c Caller) String() string {
	if c.Kind == "" {
		return ""
	}
	return c.Kind + ":" + c.ID
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
	// Detail, when set, is what a person needs to decide (the question
	// asked, or the call an approval is for), for a surface that shows the
	// action away from the Work's own panels (a Slack prompt). Plain text,
	// at most 3000 bytes.
	Detail string
	// Awaited says the Work is waiting for a person to take this action: it
	// answers a question the Work asked or an approval it needs, and the
	// Work does not go on until someone does. An action a person may take
	// at any time (sending a message to an open thread, say) is not
	// awaited, so offering it does not put the Work in front of an operator
	// as blocked on them.
	Awaited bool
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

// Acting for a person (issue #364). A surface with real identities (a Slack
// workspace, a GitHub repository) hears a person ask for an action: a
// button pressed, a comment posted. Its extension asks the host to invoke
// that action for them with an ActRequest. The host fixes the caller's Kind
// to the requesting extension's own name, so an extension can vouch only
// for identities on its own surface ("github:octocat", never
// "operator:local"); the provider then authorises that caller exactly as it
// would anyone else's, and the host accepts the Event under ActionSource.

// CodeNoAction is Host.Act's refusal when no provider names the action, or
// none offers it on the target now: the person asked for nothing that is
// here to do, which a surface may treat as an ordinary message.
const CodeNoAction = "no_action"

// ActionOffer is one action a provider offers on a target now, as an
// extension acting for people lists them (Host.Actions) to show them on its
// own surface: a button per choice, say.
type ActionOffer struct {
	Provider string
	Action
}

// ActRequest is an action an extension invokes for a person its own surface
// identified. Work is the target Work (empty for a scope action). Provider
// is the action provider's name; empty means the one provider offering
// Action on the target. Caller is the person's ID on the surface. Key makes
// the request idempotent: the same Key returns the first answer.
type ActRequest struct {
	Scope    domain.ID
	Work     domain.ID
	Provider string
	Action   string
	Input    string
	Caller   string
	Key      string
}

// ActResult is an accepted ActRequest: the Event it produced, whether this
// request added it (false for a replay of the same Key), and the
// provider's message for the person.
type ActResult struct {
	Event   domain.AcceptedEvent
	Added   bool
	Message string
}
