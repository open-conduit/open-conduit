package human

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/storage"
)

// ActionAnswer answers the asking Work's open question.
const ActionAnswer = "answer"

// Actions is a `human` entry's extension.ActionProvider. On a Work parked
// on one of its asks it offers "answer", and only while the ask is open.
// Invoke checks the caller against the ask's responders and the input
// against its options, and produces TypeAnswer on the ask key. It is also
// the entry's extension.PanelProvider: the question panel on asking Work.
type Actions struct {
	Store storage.Store
	Entry Entry
	Now   func() time.Time
}

var (
	_ extension.ActionProvider  = Actions{}
	_ extension.PanelProvider   = Actions{}
	_ extension.RequestProvider = Actions{}
)

// Panels shows the target Work's open question.
func (a Actions) Panels(ctx context.Context, t extension.ActionTarget) ([]extension.Panel, error) {
	if t.Work == nil || a.Store == nil {
		return nil, nil
	}
	p, ok := Panel(ctx, a.Store, a.Entry, *t.Work, a.now())
	if !ok {
		return nil, nil
	}
	return []extension.Panel{p}, nil
}

func (a Actions) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

// open is the target Work's latest ask when a person can answer it now.
func (a Actions) open(ctx context.Context, t extension.ActionTarget) (Ask, bool, error) {
	if t.Work == nil || a.Store == nil {
		return Ask{}, false, nil
	}
	var rec Ask
	var ok bool
	now := a.now()
	err := a.Store.Transact(ctx, storage.Guard{Now: now}, func(tx storage.Tx) error {
		ok = false
		var err error
		rec, err = latestAsk(tx, t.Work.ID)
		if errors.Is(err, storage.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if rec.Provider != a.Entry.Name || rec.Scope != t.Scope.ID {
			return nil
		}
		ok, err = outstanding(tx, rec, now)
		return err
	})
	return rec, ok, err
}

func (a Actions) Actions(ctx context.Context, t extension.ActionTarget) ([]extension.Action, error) {
	rec, ok, err := a.open(ctx, t)
	if err != nil || !ok {
		return nil, err
	}
	prompt := "your answer"
	if len(rec.Options) > 0 {
		prompt = "an option's number or text (" + numbered(rec.Options) + ")"
	}
	if len(prompt) > 200 {
		prompt = "an option's number or text"
	}
	now := a.now()
	return []extension.Action{{ID: ActionAnswer, Label: label(rec, now), Prompt: prompt, Choices: rec.Options, Detail: detail(rec, now), Awaited: true}}, nil
}

// label is what an ask's action and request read as.
func label(rec Ask, now time.Time) string {
	l := "Answer the agent's question"
	if rec.Gate != "" {
		l = "Approve or refuse " + rec.Gate
	}
	if rec.State == StateOpen && rec.escalated(now) {
		l += " (escalated)"
	}
	return l
}

// Requests are the target Work's asks of this entry: each with who may
// answer it, and once settled whether it was answered, refused (an
// approval answered with anything but approve), expired or withdrawn. An
// ask is settled here as Sync would settle it, so what is reported doesn't
// wait for the next pass: open means not settled yet, and Action is set
// only while the ask is outstanding, exactly when Actions offers it.
func (a Actions) Requests(ctx context.Context, t extension.ActionTarget) ([]extension.Request, error) {
	if t.Work == nil || a.Store == nil {
		return nil, nil
	}
	now := a.now()
	var out []extension.Request
	err := a.Store.Transact(ctx, storage.Guard{Now: now}, func(tx storage.Tx) error {
		out = nil
		asks, err := workAsks(tx, t.Work.ID)
		if err != nil {
			return err
		}
		for _, rec := range asks {
			if rec.Provider != a.Entry.Name || rec.Scope != t.Scope.ID {
				continue
			}
			r, err := request(tx, rec, now)
			if err != nil {
				return err
			}
			out = append(out, r)
		}
		return nil
	})
	return out, err
}

// request is rec as a request, settled as of now without writing it.
func request(tx storage.Tx, rec Ask, now time.Time) (extension.Request, error) {
	r := extension.Request{ID: rec.ID, Label: label(rec, now), Detail: detail(rec, now), Responders: slices.Clone(rec.responders(now)), State: extension.RequestOpen,
		Wording: wording(rec, now)}
	if rec.State != StateOpen {
		r.State, r.By = rec.settled()
		r.SettledAt = settledAt(rec)
		return r, nil
	}
	// Open in its record: what settle would make of it now, read in
	// settle's order (the wait, then the answers).
	gone, err := withdrawn(tx, rec)
	if err != nil {
		return r, err
	}
	given, _, err := answers(tx, rec, rec.Checked)
	if err != nil {
		return r, err
	}
	if len(given) > 0 {
		first := given[0]
		answered := rec
		answered.State, answered.Answer = StateAnswered, &first
		r.State, r.By = answered.settled()
		r.SettledAt = first.At
		return r, nil
	}
	if rec.expired(now) {
		r.State, r.SettledAt = extension.RequestExpired, rec.Deadline
		return r, nil
	}
	if gone {
		r.State = extension.RequestWithdrawn
		return r, nil
	}
	ok, err := outstanding(tx, rec, now)
	if ok {
		r.Action = ActionAnswer
	}
	return r, err
}

// detail is what a person needs to decide an ask away from its panel: the
// question, then its context (for an approval, the call's input), and once
// it has escalated, that nobody answered in time and who it now goes to.
func detail(rec Ask, now time.Time) string {
	d := rec.Question
	if rec.escalated(now) {
		d += "\n\nNobody answered by " + rec.EscalateAt.UTC().Format(time.RFC3339) + ", so this now also goes to " + strings.Join(rec.EscalateTo, ", ") + ". Answer by " + rec.Deadline.UTC().Format(time.RFC3339) + " or the call is refused."
	}
	if rec.Context != "" {
		d += "\n\n" + rec.Context
	}
	if len(d) > 3000 {
		d = strings.ToValidUTF8(d[:2997], "") + "..."
	}
	return d
}

func refuseAction(code, message string) (extension.ActionPlan, error) {
	return extension.ActionPlan{}, &extension.OperationError{Code: code, Message: message}
}

func (a Actions) Invoke(ctx context.Context, c extension.ActionCall) (extension.ActionPlan, error) {
	if c.Action != ActionAnswer || c.Target.Work == nil {
		return refuseAction("unknown_action", "human offers no action "+c.Action+" here")
	}
	rec, ok, err := a.open(ctx, c.Target)
	if err != nil {
		return extension.ActionPlan{}, err
	}
	if !ok {
		return refuseAction("not_outstanding", "this Work has no open question to answer")
	}
	if !answerable(rec, c.Caller, a.now()) {
		return refuseAction("not_allowed", "you are not one of the people this question is for")
	}
	// An answer accepted but not yet woken counts: refuse a second one.
	var prior []Given
	if err = a.Store.Transact(ctx, storage.Guard{Now: a.now()}, func(tx storage.Tx) error {
		var e error
		prior, _, e = answers(tx, rec, 0)
		return e
	}); err != nil {
		return extension.ActionPlan{}, err
	}
	if len(prior) > 0 {
		return refuseAction("not_outstanding", "this question has already been answered")
	}
	answer, option, ok := rec.choose(c.Input)
	if !ok {
		if len(rec.Options) > 0 {
			return refuseAction("invalid_input", "choose one of: "+numbered(rec.Options))
		}
		return refuseAction("invalid_input", "give an answer")
	}
	key := rec.Key()
	data, _ := json.Marshal(AnswerPayload{Ask: rec.ID, Question: rec.Question, Answer: answer, Option: option, Responder: c.Caller.Kind + ":" + c.Caller.ID, Operation: rec.Gate, Capability: rec.Capability, Subject: rec.Subject})
	return extension.ActionPlan{
		Event:   domain.Event{Type: TypeAnswer, CorrelationKey: &key, Data: domain.Payload{Schema: SchemaAnswer, Data: data}},
		Message: "answered: " + answer,
	}, nil
}

// answerable reports whether the caller is one of the ask's responders now.
func answerable(rec Ask, caller extension.Caller, now time.Time) bool {
	return Config{Responders: rec.responders(now)}.Allows(caller)
}
