package extension

import (
	"context"
	"time"

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

// ManualSource is the source of every Event an operator submits through the
// management API, including the creation of Work an operator starts.
const ManualSource = "core.manual"

// CallerOperator is the kind of an operator of the management API.
const CallerOperator = "operator"

// Caller is who invokes an action. The management API calls as the
// operator whose credential made the request, {Kind: CallerOperator, ID:
// <operator name>}, or as the person a client says it acts for; other
// surfaces carry their own identities. Providers refuse callers they do
// not recognise.
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

// LocalOperator is the owner's own operator, whose credential is the one in
// the daemon's state directory. Other operators call as their own names.
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
	// Secret, when set with a Prompt, makes this an action that asks a
	// person for a secret, stored under this name ([a-z][a-z0-9_-]*). A
	// surface that can take a value privately stores it in the daemon's
	// secret store as that person (which needs the admin role) and invokes
	// the action with the name as its Input. The value never reaches the
	// provider, an Event or a panel: the host refuses any other Input, and
	// doesn't offer the action to a surface that can't take a value this
	// way. Choices don't apply.
	Secret string
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

// Requests (issue #681). An awaited action answers a request: a question
// Work asked, or an approval a call needs. A surface away from the Work
// (the thread of a conversation that handed the Work its task, say) shows
// the request's state, who may answer it, and how it ended, without
// knowing what it means. The provider owns all of that: it says who its
// responders are and whether an answer granted or refused the request.

// Request states: open until it is settled; answered (a question answered,
// an approval given), refused (an approval refused), expired (nobody
// answered in time) or withdrawn (the asker stopped waiting) once it is.
const (
	RequestOpen      = "open"
	RequestAnswered  = "answered"
	RequestRefused   = "refused"
	RequestExpired   = "expired"
	RequestWithdrawn = "withdrawn"
)

// Request is one request a person is asked to decide on a target. ID is
// stable for the request's life, new for each new request, and unique
// among the provider's requests on the target. Label says what it is.
// Action is the action that answers it, set only while that action, offered
// on the target now, answers this request and no other: so at most one of
// a provider's requests on a target names a given action, and only an open
// one. An open request without Action is not answerable yet (or no longer
// through this provider's actions). Responders are who may answer it now,
// each kind (anyone of that kind) or kind:id, in the provider's own terms
// (the caller forms Act gives, such as slack:U0123ABCD, and operator).
// State is one of the Request states, and By who settled it, set only on
// a settled request that someone settled. conformance.CheckRequests checks
// these rules.
type Request struct {
	ID         string
	Label      string
	Detail     string
	Action     string
	Responders []string
	State      string
	By         string
	// SettledAt is when it was settled, on a settled request (zero when
	// the provider doesn't know).
	SettledAt time.Time
	// Wording, when set, is the same request in a person's words, in parts
	// a surface lays out itself (a Slack message's sentence, fields and
	// buttons). Label and Detail stay its plain-text form, for surfaces
	// that show text.
	Wording *Wording
}

// Wording is how a request reads to the person asked, in parts, so a
// surface can lay it out in its own terms without knowing what it means.
// Every string is plain text; a surface escapes it for its own markup.
type Wording struct {
	// Wants is what the asker wants to do, for an approval: a verb phrase
	// that follows "wants to" ("sign in to git for acme/backend").
	Wants string
	// Question is the question asked, for a request that isn't an
	// approval.
	Question string
	// Fields are the values the decision is about (an approval's call's
	// top-level input), each with a name a person reads, in order.
	Fields []WordingField
	// Input is the whole input as indented JSON, set only when some value
	// is nested and Fields can't show it.
	Input string
	// Note is anything else the person should know (that the request has
	// escalated, say), one or two sentences.
	Note string
	// Choices say how each of the answering action's choices reads, by
	// its value; a choice not listed reads as itself.
	Choices []ChoiceWording
}

// WordingField is one named value of a Wording.
type WordingField struct {
	Name, Value string
}

// Choice tones: affirm says yes to what is asked (approve), and danger
// turns it down or does something hard to undo.
const (
	ToneAffirm = "affirm"
	ToneDanger = "danger"
)

// ChoiceWording is how one choice reads. Label is what its button says
// ("Approve"); Tone is ToneAffirm, ToneDanger or ""; Confirm, when set,
// asks the person to confirm before the choice is sent, in these words.
type ChoiceWording struct {
	Choice, Label, Tone, Confirm string
}

// ChoiceWording is how choice reads: its entry in Choices, else itself.
func (w *Wording) ChoiceWording(choice string) ChoiceWording {
	if w != nil {
		for _, c := range w.Choices {
			if c.Choice == choice {
				if c.Label == "" {
					c.Label = choice
				}
				return c
			}
		}
	}
	return ChoiceWording{Choice: choice, Label: choice}
}

// RequestProvider is an ActionProvider that also reports its requests on a
// target: the open ones, and those it settled that it still remembers
// (at least the latest), so a surface can say how each ended.
type RequestProvider interface {
	Requests(ctx context.Context, t ActionTarget) ([]Request, error)
}

// RequestOffer is one request and the provider it is from.
type RequestOffer struct {
	Provider string
	Request
}
