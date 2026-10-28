package management

import (
	"context"
	"regexp"
	"sort"
	"strings"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/internal/runtime"
	"github.com/open-conduit/open-conduit/storage"
)

// Extension actions (issue #51): GET /scopes/{id}/actions and
// GET /work/{id}/actions list what each provider offers on the target now;
// POST …/actions/{provider}/{action} invokes one as the local operator. The
// provider authorises the call and returns the Event; this server accepts it
// under core.action. A refusal is the provider's code and message, and
// leaves no Event.

// wireAction is one available action.
type wireAction struct {
	Provider string   `json:"provider"`
	ID       string   `json:"id"`
	Label    string   `json:"label"`
	Prompt   string   `json:"prompt,omitempty"`
	Choices  []string `json:"choices,omitempty"`
}

var actionID = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

// actionTarget reads a route's target: a scope, or Work and its scope.
func (s *Server) actionTarget(c *call, work bool) (extension.ActionTarget, error) {
	var t extension.ActionTarget
	err := s.read(c.ctx, func(tx storage.Tx) error {
		scope := domain.ID(c.params[0])
		if work {
			w, e := tx.Work(domain.ID(c.params[0]))
			if e != nil {
				return e
			}
			t.Work, scope = &w, w.ScopeID
		}
		var e error
		t.Scope, e = tx.Scope(scope)
		return e
	})
	return t, err
}

func (s *Server) providers(ctx context.Context, scope domain.ID) map[string]extension.ActionProvider {
	if s.o.Actions == nil {
		return nil
	}
	return s.o.Actions(ctx, scope)
}

// available lists the providers' actions on t, sorted by provider. A
// provider that fails contributes nothing, like annotations.
func (s *Server) available(ctx context.Context, t extension.ActionTarget) []wireAction {
	ps := s.providers(ctx, t.Scope.ID)
	out := []wireAction{}
	for name, p := range ps {
		out = append(out, offered(ctx, name, p, t)...)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Provider != out[j].Provider {
			return out[i].Provider < out[j].Provider
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// offered is one provider's well-formed actions on t.
func offered(ctx context.Context, name string, p extension.ActionProvider, t extension.ActionTarget) []wireAction {
	actions, err := p.Actions(ctx, t)
	if err != nil {
		return nil
	}
	var out []wireAction
	for _, a := range actions {
		if !actionID.MatchString(a.ID) || strings.TrimSpace(a.Label) == "" || len(a.Label) > 200 || len(a.Prompt) > 200 {
			continue
		}
		out = append(out, wireAction{Provider: name, ID: a.ID, Label: a.Label, Prompt: a.Prompt, Choices: choices(a)})
	}
	return out
}

// choices are a's well-formed choices: none unless it has a Prompt and
// every choice is a short, non-empty line.
func choices(a extension.Action) []string {
	if a.Prompt == "" || len(a.Choices) > 20 {
		return nil
	}
	for _, c := range a.Choices {
		if strings.TrimSpace(c) == "" || len(c) > 200 || strings.ContainsAny(c, "\r\n") {
			return nil
		}
	}
	return a.Choices
}

func (s *Server) listScopeActions(c *call) (int, any, error) { return s.listActions(c, false) }
func (s *Server) listWorkActions(c *call) (int, any, error)  { return s.listActions(c, true) }
func (s *Server) invokeScopeAction(c *call) (int, any, error) {
	return s.invokeAction(c, false)
}
func (s *Server) invokeWorkAction(c *call) (int, any, error) { return s.invokeAction(c, true) }

func (s *Server) listActions(c *call, work bool) (int, any, error) {
	if err := noQuery(c); err != nil {
		return 0, nil, err
	}
	t, err := s.actionTarget(c, work)
	if err != nil {
		return 0, nil, err
	}
	return 200, map[string]any{"actions": s.available(c.ctx, t)}, nil
}

func (s *Server) invokeAction(c *call, work bool) (int, any, error) {
	var in struct {
		Input string `json:"input"`
	}
	if err := c.decode(&in); err != nil {
		return 0, nil, err
	}
	if len(in.Input) > 4096 {
		return 0, nil, fail(400, "malformed", "input is too long")
	}
	t, err := s.actionTarget(c, work)
	if err != nil {
		return 0, nil, err
	}
	name, id := c.params[1], c.params[2]
	p, ok := s.providers(c.ctx, t.Scope.ID)[name]
	if !ok {
		return 0, nil, fail(404, "not_found", "no such action provider on this target")
	}
	plan, err := p.Invoke(c.ctx, extension.ActionCall{Target: t, Action: id, Input: in.Input, Caller: extension.LocalOperator})
	if err != nil {
		return 0, nil, err
	}
	a := runtime.ActionEvent{ScopeID: t.Scope.ID, Provider: name, Action: id, Key: c.key, Event: plan.Event}
	// An Event that names a correlation key is addressed by it (an answer
	// on a key no Work owns, say); one without is the target Work's.
	if t.Work != nil && plan.Event.CorrelationKey == nil {
		a.WorkID = t.Work.ID
	}
	accepted, added, err := s.o.Control.SubmitAction(c.ctx, a)
	if err != nil {
		return 0, nil, err
	}
	status := 200
	if added {
		status = 201
	}
	msg := strings.ToValidUTF8(plan.Message, "")
	if len(msg) > 500 {
		msg = strings.ToValidUTF8(msg[:500], "")
	}
	return status, map[string]any{"event": toEvent(accepted, t.Scope.ID), "message": msg}, nil
}
