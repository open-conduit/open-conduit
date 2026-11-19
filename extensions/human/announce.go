package human

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/protocol"
	"github.com/open-conduit/open-conduit/storage"
)

// Request events (issue #758). A request's responders learn of it only
// where it shows, and many of them are nowhere it shows: Work started by a
// schedule has no thread, an approver may not be in the conversation that
// asked, or may not use any surface Open Conduit has. So each change of a
// request is also an Event, for whatever a deployment uses to tell people
// (a route to its own binding that sends an email or a page, or a reader of
// the event log), and that answers through the answer action as anyone
// else does. The Events say what happened; they carry no authority, and
// answering still goes through the action alone.
//
// Every request gets TypeRequestOpened, then TypeRequestEscalated if it
// escalates, then TypeRequestSettled once it is answered, refused, expired
// or withdrawn, in that order and each once. They are announced from the
// ask records by the settling task, after it settles them, so a change made
// anywhere (an answer, a gate that withdrew a request it asked again, the
// clock) is announced the same way and a restart loses none.
const (
	TypeRequestOpened    = "human.request.opened"
	TypeRequestEscalated = "human.request.escalated"
	TypeRequestSettled   = "human.request.settled"

	SchemaRequest = "human.request.v1"
)

// RequestTypes are the request Event families, in the order a request goes
// through them.
var RequestTypes = []string{TypeRequestOpened, TypeRequestEscalated, TypeRequestSettled}

// RequestLoss is the declared loss limitation of the request families.
const RequestLoss = "announced from the extension's own ask records; nothing upstream re-derives them"

// Request kinds: a question an agent asked with human.ask, an approval a
// gated call needs, or an approval Work needs before it first starts.
const (
	KindQuestion = "question"
	KindApproval = "approval"
	KindStart    = "start"
)

// RequestPayload is a request Event's data: the request as it stood when
// it changed. Request, with Work and Provider, names it: the answer action
// is Action of Provider on Work (POST /v1/work/{work}/actions/{provider}/
// {action}). Responders are who may answer it then, each kind or kind:id
// (slack:U0123ABCD, operator:local). State is open on an opened or
// escalated request, and once it is settled answered, refused, expired or
// withdrawn, with By who answered or refused it.
type RequestPayload struct {
	Request    string    `json:"request"`
	Provider   string    `json:"provider"`
	Work       domain.ID `json:"work"`
	Execution  domain.ID `json:"execution,omitempty"`
	Binding    string    `json:"binding,omitempty"`
	Kind       string    `json:"kind"`
	Operation  string    `json:"operation,omitempty"`
	Label      string    `json:"label"`
	Detail     string    `json:"detail,omitempty"`
	Action     string    `json:"action"`
	Options    []string  `json:"options,omitempty"`
	Responders []string  `json:"responders"`
	AskedAt    time.Time `json:"asked_at"`
	ExpiresAt  time.Time `json:"expires_at,omitzero"`
	// EscalatedAt is when it escalated to the wider Responders, if it has.
	EscalatedAt time.Time `json:"escalated_at,omitzero"`
	State       string    `json:"state"`
	By          string    `json:"by,omitempty"`
	SettledAt   time.Time `json:"settled_at,omitzero"`
}

// RequestKey is the correlation key of a request's Events: one per
// request, which no asking Work owns, so the Events never wake the asker
// and a route can give all of one request's Events to one Work.
func RequestKey(ask string) domain.CorrelationKey {
	return domain.CorrelationKey{Source: Type, ExternalID: "request/" + ask}
}

// ParseRequestKey is the ask a request key names.
func ParseRequestKey(k domain.CorrelationKey) (string, bool) {
	const prefix = "request/"
	if k.Source != Type || len(k.ExternalID) <= len(prefix) || k.ExternalID[:len(prefix)] != prefix {
		return "", false
	}
	return k.ExternalID[len(prefix):], true
}

// Announcement phases: the latest of a request's Events announced.
const (
	announcedOpened    = "opened"
	announcedEscalated = "escalated"
	announcedSettled   = "settled"
)

// AcceptFunc accepts a root Event of the extension's own families inside
// the caller's transaction (inproc.Host.AcceptInTx).
type AcceptFunc func(tx storage.Tx, scope domain.ID, e domain.Event) (domain.AcceptedEvent, bool, error)

// Announce accepts the request Events each ask's changes call for that are
// not announced yet, in each ask's own transaction with the record of what
// it announced, and returns how many it accepted. Asks recorded before
// requests were announced are left alone. It is idempotent: an Event's
// delivery is its request and phase.
func Announce(ctx context.Context, store storage.Store, now time.Time, accept AcceptFunc) (int, error) {
	if accept == nil {
		return 0, nil
	}
	var ids []string
	if err := store.Transact(ctx, storage.Guard{Now: now}, func(tx storage.Tx) error {
		ids = nil
		rows, err := tx.List(Namespace, "ask/")
		if err != nil {
			return err
		}
		for _, row := range rows {
			var a Ask
			if json.Unmarshal(row.Value, &a) == nil && a.Announce && a.Announced != announcedSettled {
				ids = append(ids, a.ID)
			}
		}
		return nil
	}); err != nil {
		return 0, err
	}
	n := 0
	var errs []error
	for _, id := range ids {
		added := 0
		err := store.Transact(ctx, storage.Guard{Now: now}, func(tx storage.Tx) error {
			added = 0
			a, err := getAsk(tx, id)
			if err != nil {
				return err
			}
			due := a.due()
			if len(due) == 0 {
				return nil
			}
			binding, err := askBinding(tx, a)
			if err != nil {
				return err
			}
			for _, phase := range due {
				e, err := requestEvent(a, binding, phase, now)
				if err != nil {
					return err
				}
				if _, fresh, err := accept(tx, a.Scope, e); err != nil {
					return err
				} else if fresh {
					added++
				}
				a.Announced = phase
			}
			return putAsk(tx, a)
		})
		if err != nil {
			errs = append(errs, err)
			continue
		}
		n += added
	}
	return n, errors.Join(errs...)
}

// due are the phases a's record calls for that it has not announced yet,
// in order.
func (a Ask) due() []string {
	if !a.Announce {
		return nil
	}
	want := []string{announcedOpened}
	if !a.EscalatedAt.IsZero() {
		want = append(want, announcedEscalated)
	}
	if a.State != StateOpen {
		want = append(want, announcedSettled)
	}
	// Nothing announced yet (or a phase this record no longer calls for)
	// announces from the start; each delivery is accepted once.
	return want[slices.Index(want, a.Announced)+1:]
}

// askBinding is the binding of the asking run (or, for a start, the one
// the Work would start on).
func askBinding(tx storage.Tx, a Ask) (string, error) {
	if a.Binding != "" || a.Execution == "" {
		return a.Binding, nil
	}
	x, err := tx.Execution(a.Execution)
	if errors.Is(err, storage.ErrNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return x.ExecutorBinding, nil
}

// kind is what the ask asks for.
func (a Ask) kind() string {
	switch {
	case a.Start:
		return KindStart
	case a.Gate != "":
		return KindApproval
	}
	return KindQuestion
}

// settled is how a settled ask ended, as a request state, and who settled
// it: an approval answered with anything but approve is refused.
func (a Ask) settled() (state, by string) {
	switch a.State {
	case StateAnswered:
		if a.Answer == nil {
			return extension.RequestAnswered, ""
		}
		if (a.Gate != "" || a.Start) && a.Answer.Answer != OptionApprove {
			return extension.RequestRefused, a.Answer.Responder
		}
		return extension.RequestAnswered, a.Answer.Responder
	case StateExpired:
		return extension.RequestExpired, ""
	case StateWithdrawn:
		return extension.RequestWithdrawn, ""
	}
	return extension.RequestOpen, ""
}

// requestEvent is a's Event for phase.
func requestEvent(a Ask, binding, phase string, now time.Time) (domain.Event, error) {
	at := a.AskedAt
	// The label and detail as they read at the change: an escalated
	// request reads as escalated.
	seen := a.AskedAt
	p := RequestPayload{Request: a.ID, Provider: a.Provider, Work: a.Work, Execution: a.Execution, Binding: binding, Kind: a.kind(),
		Action: ActionAnswer, Options: slices.Clone(a.Options), AskedAt: a.AskedAt.UTC(), State: extension.RequestOpen}
	if a.Gate != "" && !a.Start {
		p.Operation = a.Gate
	}
	if !a.Deadline.IsZero() {
		p.ExpiresAt = a.Deadline.UTC()
	}
	switch phase {
	case announcedEscalated:
		at, seen = a.EscalatedAt, a.EscalatedAt
	case announcedSettled:
		at = a.ClosedAt
		if at.IsZero() {
			at = now
		}
		seen = at
		p.State, p.By = a.settled()
		p.SettledAt = at.UTC()
	}
	if !a.EscalatedAt.IsZero() && !seen.Before(a.EscalatedAt) {
		p.EscalatedAt = a.EscalatedAt.UTC()
	}
	p.Label, p.Detail = label(a, seen), detail(a, seen)
	p.Responders = slices.Clone(a.responders(seen))
	if p.Responders == nil {
		p.Responders = []string{}
	}
	data, err := json.Marshal(p)
	if err != nil {
		return domain.Event{}, err
	}
	typ := map[string]string{announcedOpened: TypeRequestOpened, announcedEscalated: TypeRequestEscalated, announcedSettled: TypeRequestSettled}[phase]
	key := RequestKey(a.ID)
	delivery := "request/" + a.ID + "/" + phase
	return domain.Event{ID: domain.ID(protocol.EmittedEventID(Type, delivery)), Type: typ, Source: Type, DeliveryID: delivery,
		OccurredAt: at.UTC(), ReceivedAt: now.UTC(), CorrelationKey: &key, Data: domain.Payload{Schema: SchemaRequest, Data: data}}, nil
}
