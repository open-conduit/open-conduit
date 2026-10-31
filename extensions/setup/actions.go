package setup

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/extension/inproc"
	"github.com/open-conduit/open-conduit/storage"
)

// Action IDs.
const (
	ActionStart   = "start"
	ActionMessage = "message"
	ActionAnswer  = "answer"
)

// Actions is a `setup` entry's extension.ActionProvider. On the scope it
// offers "start" while the scope has no open session. On an open session's
// Work it offers "message", and "answer" while the session waits on its
// question. Once the agent proposes changes it also offers approve and
// reject for each, Approve all and validate (decide.go). Ending a session
// is the operator's cancellation of its Work.
type Actions struct {
	Store storage.Store
	Now   func() time.Time
	// Config validates the change set's candidate.
	Config inproc.ConfigHost
}

var (
	_ extension.ActionProvider = Actions{}
	_ extension.PanelProvider  = Actions{}
)

func (a Actions) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

func (a Actions) transact(ctx context.Context, fn func(storage.Tx) error) error {
	return a.Store.Transact(ctx, storage.Guard{Now: a.now()}, fn)
}

// binding is the scope's setup binding: the one the first route this
// handler serves binds.
func binding(scope domain.Scope) (domain.Binding, bool) {
	for _, r := range scope.Routes {
		if r.Handler != Type {
			continue
		}
		for _, b := range scope.Bindings {
			if b.Name == r.Binding {
				return b, true
			}
		}
		return domain.Binding{}, false
	}
	return domain.Binding{}, false
}

// Models are the models the scope's setup binding lets a session choose:
// its executor config's "models".
func Models(scope domain.Scope) []string {
	b, ok := binding(scope)
	if !ok {
		return nil
	}
	var c struct {
		Models []string `json:"models"`
	}
	_ = json.Unmarshal(b.Config.Data, &c)
	return c.Models
}

// Grants are what the scope's setup binding offers a session beyond talking,
// which the operator turns on when starting it.
func Grants(scope domain.Scope) []domain.BindingGrant {
	b, ok := binding(scope)
	if !ok {
		return nil
	}
	return b.Grants
}

// parseStart reads the start action's input: at most one of models and a
// "+name" for each grant to turn on, in any order and separated by spaces
// ("opus +shell"). With models offered and none named, it is the first.
func parseStart(input string, models []string, grants []domain.BindingGrant) (Start, error) {
	var s Start
	for _, tok := range strings.Fields(input) {
		if name, ok := strings.CutPrefix(tok, "+"); ok {
			i := slices.IndexFunc(grants, func(g domain.BindingGrant) bool { return strings.EqualFold(g.Name, name) })
			if i < 0 {
				return s, &extension.OperationError{Code: "invalid_input", Message: "this setup binding offers no grant " + name + startUsage(models, grants)}
			}
			if !slices.Contains(s.Grants, grants[i].Name) {
				s.Grants = append(s.Grants, grants[i].Name)
			}
			continue
		}
		m, ok := choose(models, tok)
		if !ok || s.Model != "" {
			return s, &extension.OperationError{Code: "invalid_input", Message: "give one model" + startUsage(models, grants)}
		}
		s.Model = m
	}
	if s.Model == "" && len(models) > 0 {
		s.Model = models[0]
	}
	return s, nil
}

// startUsage says what the start action's input may hold.
func startUsage(models []string, grants []domain.BindingGrant) string {
	var parts []string
	if len(models) > 0 {
		parts = append(parts, "one of "+strings.Join(models, ", "))
	}
	if len(grants) > 0 {
		names := make([]string, 0, len(grants))
		for _, g := range grants {
			names = append(names, "+"+g.Name)
		}
		parts = append(parts, "any of "+strings.Join(names, ", "))
	}
	if len(parts) == 0 {
		return ""
	}
	return ": " + strings.Join(parts, ", then ")
}

// openSession reports whether the scope has a session that has not ended.
func openSession(tx storage.Tx, scope domain.ID) (bool, error) {
	works, err := tx.ListWorks()
	if err != nil {
		return false, err
	}
	for _, w := range works {
		if w.ScopeID == scope && w.Kind == WorkKind && !w.State.Terminal() {
			return true, nil
		}
	}
	return false, nil
}

// question is the session's question when the operator can answer it now:
// the session is parked on its answer and none has been accepted yet.
func question(tx storage.Tx, w domain.Work) (Question, bool, error) {
	if w.Kind != WorkKind || w.State != domain.WorkWaiting {
		return Question{}, false, nil
	}
	q, err := latestQuestion(tx, w.ID)
	if errors.Is(err, storage.ErrNotFound) {
		return q, false, nil
	}
	if err != nil {
		return q, false, err
	}
	waits, err := tx.ListWaits(w.ID)
	if err != nil {
		return q, false, err
	}
	parked := slices.ContainsFunc(waits, func(x domain.Wait) bool {
		return x.State == domain.WaitActive && slices.Equal(x.Match.Types, []string{TypeAnswer})
	})
	if !parked {
		return q, false, nil
	}
	key, _ := sessionKey(w)
	after, err := tx.Events(q.Sequence, 0)
	if err != nil {
		return q, false, err
	}
	for _, e := range after {
		var p Answer
		if e.Event.Type == TypeAnswer && e.Event.CorrelationKey != nil && *e.Event.CorrelationKey == key &&
			json.Unmarshal(e.Event.Data.Data, &p) == nil && p.Question == q.ID {
			return q, false, nil
		}
	}
	return q, true, nil
}

func (a Actions) Actions(ctx context.Context, t extension.ActionTarget) ([]extension.Action, error) {
	var out []extension.Action
	err := a.transact(ctx, func(tx storage.Tx) error {
		if t.Work == nil {
			open, err := openSession(tx, t.Scope.ID)
			if err != nil || open {
				return err
			}
			start := extension.Action{ID: ActionStart, Label: "Start a setup session"}
			models, grants := Models(t.Scope), Grants(t.Scope)
			if len(models) > 0 {
				start.Prompt, start.Choices = "the model the agent uses", models
			}
			if len(grants) > 0 {
				// The prompt says how to turn grants on; a choice alone
				// still starts a session with none.
				start.Prompt = "the model the agent uses" + startUsage(models, grants)
				if len(models) == 0 {
					start.Prompt = "what to turn on" + startUsage(models, grants)
				}
				if len(start.Prompt) > 200 {
					start.Prompt = "the model, then +name for each grant to turn on"
				}
			}
			out = append(out, start)
			return nil
		}
		if t.Work.Kind != WorkKind || t.Work.State.Terminal() {
			return nil
		}
		out = append(out, extension.Action{ID: ActionMessage, Label: "Send the agent a message", Prompt: "your message"})
		cs, st, err := sessionChanges(tx, *t.Work)
		if err != nil {
			return err
		}
		out = append(out, changeActions(cs, st)...)
		q, ok, err := question(tx, *t.Work)
		if err != nil || !ok {
			return err
		}
		prompt := "your answer"
		if len(q.Choices) > 0 {
			prompt = "a choice (" + strings.Join(q.Choices, ", ") + ") or your own answer"
		}
		if len(prompt) > 200 {
			prompt = "a choice or your own answer"
		}
		out = append(out, extension.Action{ID: ActionAnswer, Label: "Answer the agent's question", Prompt: prompt, Choices: q.Choices})
		return nil
	})
	return out, err
}

func refuseAction(code, message string) (extension.ActionPlan, error) {
	return extension.ActionPlan{}, &extension.OperationError{Code: code, Message: message}
}

func (a Actions) Invoke(ctx context.Context, c extension.ActionCall) (extension.ActionPlan, error) {
	if c.Caller.Kind != extension.CallerOperator {
		return refuseAction("not_allowed", "only the operator drives a setup session")
	}
	switch {
	case c.Action == ActionStart && c.Target.Work == nil:
		return a.start(ctx, c)
	case c.Action == ActionMessage && c.Target.Work != nil:
		return a.message(ctx, c)
	case c.Action == ActionAnswer && c.Target.Work != nil:
		return a.answer(ctx, c)
	case c.Target.Work != nil && c.Target.Work.Kind == WorkKind && !c.Target.Work.State.Terminal():
		if plan, err, ok := a.decide(ctx, c); ok {
			return plan, err
		}
	}
	return refuseAction("unknown_action", "setup offers no action "+c.Action+" here")
}

func (a Actions) start(ctx context.Context, c extension.ActionCall) (extension.ActionPlan, error) {
	var open bool
	if err := a.transact(ctx, func(tx storage.Tx) error {
		var e error
		open, e = openSession(tx, c.Target.Scope.ID)
		return e
	}); err != nil {
		return extension.ActionPlan{}, err
	}
	if open {
		return refuseAction("session_open", "this scope already has a setup session; end it before starting another")
	}
	s, err := parseStart(c.Input, Models(c.Target.Scope), Grants(c.Target.Scope))
	if err != nil {
		return extension.ActionPlan{}, err
	}
	var b [8]byte
	_, _ = rand.Read(b[:])
	key := SessionKey(c.Target.Scope.ID, hex.EncodeToString(b[:]))
	message := "started a setup session"
	if s.Model != "" {
		message += " on " + s.Model
	}
	if len(s.Grants) > 0 {
		message += " with " + strings.Join(s.Grants, ", ")
	}
	return extension.ActionPlan{Event: domain.Event{Type: TypeStart, CorrelationKey: &key, Data: payload(SchemaStart, s)}, Message: message}, nil
}

func (a Actions) message(_ context.Context, c extension.ActionCall) (extension.ActionPlan, error) {
	w := *c.Target.Work
	key, ok := sessionKey(w)
	if w.Kind != WorkKind || w.State.Terminal() || !ok {
		return refuseAction("not_a_session", "this Work is not an open setup session")
	}
	msg := strings.TrimSpace(c.Input)
	if msg == "" || len(msg) > maxText {
		return refuseAction("invalid_input", "give a message of at most 8192 bytes")
	}
	return extension.ActionPlan{Event: domain.Event{Type: TypeMessage, CorrelationKey: &key, Data: payload(SchemaMessage, Message{Role: RoleOperator, Text: msg})}, Message: "sent"}, nil
}

func (a Actions) answer(ctx context.Context, c extension.ActionCall) (extension.ActionPlan, error) {
	w := *c.Target.Work
	var q Question
	var ok bool
	if err := a.transact(ctx, func(tx storage.Tx) error {
		var e error
		q, ok, e = question(tx, w)
		return e
	}); err != nil {
		return extension.ActionPlan{}, err
	}
	if !ok {
		return refuseAction("not_outstanding", "the session has no open question to answer")
	}
	// Choices are suggestions: one picked by its text or number is given
	// as written, and anything else is the operator's own answer.
	answer := strings.TrimSpace(c.Input)
	if picked, ok := choose(q.Choices, answer); ok {
		answer = picked
	}
	if answer == "" || len(answer) > maxText {
		return refuseAction("invalid_input", "give an answer of at most 8192 bytes")
	}
	key, _ := sessionKey(w)
	return extension.ActionPlan{Event: domain.Event{Type: TypeAnswer, CorrelationKey: &key, Data: payload(SchemaAnswer, Answer{Question: q.ID, Answer: answer})}, Message: "answered: " + answer}, nil
}

// choose matches input against choices: a choice's text (case-insensitive)
// or its number from 1.
func choose(choices []string, input string) (string, bool) {
	input = strings.TrimSpace(input)
	if n, err := strconv.Atoi(input); err == nil && n >= 1 && n <= len(choices) {
		return choices[n-1], true
	}
	i := slices.IndexFunc(choices, func(c string) bool { return strings.EqualFold(c, input) })
	if i < 0 {
		return "", false
	}
	return choices[i], true
}
