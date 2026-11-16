package integration_test

import (
	"context"
	"strings"
	"testing"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	sl "github.com/open-conduit/open-conduit/extensions/slack"
	"github.com/open-conduit/open-conduit/storage"
)

// handOff puts child Work under parent, as create_work does, with one
// attempt on binding, and walks it to state.
func (e *slEnv) handOff(id, parent domain.ID, binding, title string, states ...domain.WorkState) {
	e.t.Helper()
	e.read(func(tx storage.Tx) error {
		w := domain.Work{ID: id, Kind: "fixture.task", ScopeID: slScope, Context: domain.Payload{Schema: "fixture.v1", Data: []byte(`{}`)}, State: domain.WorkReady, ParentID: parent,
			Display: &domain.WorkDisplay{Title: title}, CreatedAt: e.clock.Now()}
		if err := tx.PutWork(w, 0); err != nil {
			return err
		}
		// The attempt's trigger is any accepted Event: the hand-off's.
		evs, err := tx.Events(0, 1)
		if err != nil || len(evs) == 0 {
			return storage.ErrNotFound
		}
		return tx.PutExecution(domain.Execution{ID: id + "/attempt/1", WorkID: id, ExecutorBinding: binding, Attempt: 1, State: domain.ExecutionPending, TriggerID: evs[0].Event.ID}, 0)
	})
	e.moveWork(id, states...)
}

// prompts are the app's prompt messages in the thread: those with buttons
// that aren't a progress message.
func (e *slEnv) prompts(root string) []sl.Message {
	var out []sl.Message
	for _, m := range e.fake.Thread(slChannel, root) {
		if len(m.Blocks) == 2 && m.Blocks[1]["type"] == "actions" && m.Blocks[0]["type"] != "plan" {
			out = append(out, m)
		}
	}
	return out
}

// With child_requests on, a request held by child Work at any depth shows
// in the thread of the conversation that handed it off: as a task below
// the child's in the run's progress message, naming who asks and who may
// answer, complete once answered and an error once refused or expired.
// Only a request a Slack person may answer is also prompted with its
// buttons, and a press answers the child's request, not the conversation's.
func TestSlackShowsChildRequestsInTheConversationsThread(t *testing.T) {
	e := newSlackEnv(t, `,"progress":{"enabled":true},"child_requests":true,"identities":[{"binding":"specialist","username":"Steve"}]`, sl.IngressOptions{})
	root := e.fake.Post(slChannel, slAlice, "Open a PR for the checkout fix")
	e.sync()
	e.reconcile()
	e.route()
	w, _ := e.workFor(root)
	e.converge()
	e.moveWork(w.ID, domain.WorkRunning)

	// The conversation hands off to a planner, which hands off to Steve,
	// whose github.pr.create is held for Alice; the planner's own sign-in
	// is held for the operator only.
	e.handOff("child/planner", w.ID, "planner", "Plan the fix", domain.WorkRunning, domain.WorkWaiting)
	e.handOff("child/steve", "child/planner", "specialist", "Open the PR", domain.WorkRunning, domain.WorkWaiting)
	states := map[domain.ID]string{"child/steve": extension.RequestOpen, "child/planner": extension.RequestOpen}
	e.requests = func(_ context.Context, scope, work domain.ID) ([]extension.RequestOffer, error) {
		switch {
		case scope != slScope:
			return nil, nil
		case work == "child/steve":
			return []extension.RequestOffer{{Provider: "decisions", Request: extension.Request{ID: "ask/1", Label: "Approve or refuse github.pr.create", Action: "answer",
				Responders: []string{"slack:" + slAlice}, State: states[work]}}}, nil
		case work == "child/planner":
			return []extension.RequestOffer{{Provider: "decisions", Request: extension.Request{ID: "ask/2", Label: "Approve or refuse git.sign_in", Action: "answer",
				Responders: []string{"operator"}, State: states[work]}}}, nil
		}
		return nil, nil
	}
	e.actions = func(_ context.Context, _, work domain.ID) ([]extension.ActionOffer, error) {
		if states[work] != extension.RequestOpen || work == w.ID {
			return nil, nil
		}
		return []extension.ActionOffer{{Provider: "decisions", Action: extension.Action{ID: "answer", Label: "Approve or refuse", Detail: "Allow github.pr.create?\n\nInput: {\"title\":\"Fix checkout\"}", Choices: []string{"approve", "refuse"}, Awaited: true}}}, nil
	}
	e.converge()
	title, tasks, _ := planOf(t, e.progress(root)[0])
	want := []shownTask{
		{"Plan the fix", "in_progress"},
		{"planner asks: Approve or refuse git.sign_in (for operator)", "in_progress"},
		{"Steve asks: Approve or refuse github.pr.create (for slack:" + slAlice + ")", "in_progress"},
	}
	if title != "Working" || len(tasks) != len(want) {
		t.Fatalf("plan: %q %+v", title, tasks)
	}
	for i := range want {
		if tasks[i] != want[i] {
			t.Fatalf("task %d: %+v, want %+v", i, tasks[i], want[i])
		}
	}

	// Only Steve's request has a Slack responder, so only it is prompted.
	ps := e.prompts(root)
	if len(ps) != 1 || ps[0].Text != "Steve asks: Approve or refuse" {
		t.Fatalf("prompts: %+v", ps)
	}
	section := ps[0].Blocks[0]["text"].(map[string]any)["text"].(string)
	if !strings.Contains(section, "Fix checkout") || !strings.Contains(section, "Who can answer: slack:"+slAlice) {
		t.Fatalf("prompt text: %q", section)
	}
	if e.converge(); len(e.prompts(root)) != 1 {
		t.Fatal("prompted twice")
	}
	var got []extension.ActRequest
	e.act = func(_ context.Context, r extension.ActRequest) (extension.ActResult, error) {
		got = append(got, r)
		return extension.ActResult{Added: true, Message: "answered: " + r.Input}, nil
	}
	buttons, _ := ps[0].Blocks[1]["elements"].([]any)
	refuse := buttons[1].(map[string]any)
	if err := e.projector.Interact(context.Background(), e.fake.Press(slChannel, root, ps[0].TS, slAlice, refuse["action_id"].(string), refuse["value"].(string))); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Work != "child/steve" || got[0].Provider != "decisions" || got[0].Input != "refuse" || got[0].Caller != slAlice {
		t.Fatalf("act: %+v", got)
	}

	// Steve's request is refused and the planner's times out.
	states["child/steve"], states["child/planner"] = extension.RequestRefused, extension.RequestExpired
	e.converge()
	_, tasks, _ = planOf(t, e.progress(root)[0])
	if len(tasks) != 3 || tasks[1].Status != "error" || tasks[2].Status != "error" {
		t.Fatalf("plan once refused and expired: %+v", tasks)
	}
	states["child/steve"] = extension.RequestAnswered
	e.converge()
	if _, tasks, _ = planOf(t, e.progress(root)[0]); tasks[2].Status != "complete" {
		t.Fatalf("plan once answered: %+v", tasks)
	}
}

// Off by default: without child_requests a child's request is neither a
// task nor a prompt in the conversation's thread.
func TestSlackChildRequestsAreOffByDefault(t *testing.T) {
	e := newSlackEnv(t, `,"progress":{"enabled":true}`, sl.IngressOptions{})
	root := e.fake.Post(slChannel, slAlice, "Open a PR for the checkout fix")
	e.sync()
	e.reconcile()
	e.route()
	w, _ := e.workFor(root)
	e.converge()
	e.moveWork(w.ID, domain.WorkRunning)
	e.handOff("child/steve", w.ID, "specialist", "Open the PR", domain.WorkRunning, domain.WorkWaiting)
	e.requests = func(context.Context, domain.ID, domain.ID) ([]extension.RequestOffer, error) {
		return []extension.RequestOffer{{Provider: "decisions", Request: extension.Request{ID: "ask/1", Label: "Approve or refuse github.pr.create", Action: "answer", Responders: []string{"slack"}, State: extension.RequestOpen}}}, nil
	}
	e.actions = func(_ context.Context, _, work domain.ID) ([]extension.ActionOffer, error) {
		return []extension.ActionOffer{{Provider: "decisions", Action: extension.Action{ID: "answer", Label: "Approve or refuse", Choices: []string{"approve", "refuse"}, Awaited: true}}}, nil
	}
	e.converge()
	if _, tasks, _ := planOf(t, e.progress(root)[0]); len(tasks) != 1 {
		t.Fatalf("plan: %+v", tasks)
	}
	if ps := e.prompts(root); len(ps) != 0 {
		t.Fatalf("prompted without child_requests: %+v", ps)
	}
}

// A request is prompted only when its action answers it alone: when a
// provider names one action for two open requests, a press could answer
// either, so neither is prompted, and a request without an action (not
// answerable yet) isn't either. Both still show in the plan.
func TestSlackPromptsAChildRequestOnlyWhenItsActionIsItsOwn(t *testing.T) {
	e := newSlackEnv(t, `,"progress":{"enabled":true},"child_requests":true`, sl.IngressOptions{})
	root := e.fake.Post(slChannel, slAlice, "Open a PR for the checkout fix")
	e.sync()
	e.reconcile()
	e.route()
	w, _ := e.workFor(root)
	e.converge()
	e.moveWork(w.ID, domain.WorkRunning)
	e.handOff("child/a", w.ID, "specialist", "Open the PR", domain.WorkRunning, domain.WorkWaiting)
	e.handOff("child/b", w.ID, "specialist", "Sign in", domain.WorkRunning, domain.WorkWaiting)
	e.requests = func(_ context.Context, _, work domain.ID) ([]extension.RequestOffer, error) {
		open := func(id, action string) extension.RequestOffer {
			return extension.RequestOffer{Provider: "decisions", Request: extension.Request{ID: id, Label: "Approve or refuse " + id, Action: action, Responders: []string{"slack"}, State: extension.RequestOpen}}
		}
		if work == "child/a" {
			return []extension.RequestOffer{open("ask/1", "answer"), open("ask/2", "answer")}, nil
		}
		return []extension.RequestOffer{open("ask/3", "")}, nil
	}
	e.actions = func(context.Context, domain.ID, domain.ID) ([]extension.ActionOffer, error) {
		return []extension.ActionOffer{{Provider: "decisions", Action: extension.Action{ID: "answer", Label: "Approve or refuse", Choices: []string{"approve", "refuse"}, Awaited: true}}}, nil
	}
	e.converge()
	if ps := e.prompts(root); len(ps) != 0 {
		t.Fatalf("prompted an ambiguous or unanswerable request: %+v", ps)
	}
	if _, tasks, _ := planOf(t, e.progress(root)[0]); len(tasks) != 5 {
		t.Fatalf("plan: %+v", tasks)
	}
}
