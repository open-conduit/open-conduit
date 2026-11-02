package human

import (
	"context"
	"encoding/json"
	"errors"
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
	_ extension.ActionProvider = Actions{}
	_ extension.PanelProvider  = Actions{}
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
	label := "Answer the agent's question"
	if rec.Gate != "" {
		label = "Approve or refuse " + rec.Gate
	}
	return []extension.Action{{ID: ActionAnswer, Label: label, Prompt: prompt, Choices: rec.Options}}, nil
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
	if !answerable(rec, c.Caller) {
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
	data, _ := json.Marshal(AnswerPayload{Ask: rec.ID, Question: rec.Question, Answer: answer, Option: option, Responder: c.Caller.Kind + ":" + c.Caller.ID, Operation: rec.Gate})
	return extension.ActionPlan{
		Event:   domain.Event{Type: TypeAnswer, CorrelationKey: &key, Data: domain.Payload{Schema: SchemaAnswer, Data: data}},
		Message: "answered: " + answer,
	}, nil
}

// answerable reports whether the caller is one of the ask's responders.
func answerable(rec Ask, caller extension.Caller) bool {
	return Config{Responders: rec.Responders}.Allows(caller)
}
