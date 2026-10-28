package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/conformance"
	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	hu "github.com/open-conduit/open-conduit/extensions/human"
	"github.com/open-conduit/open-conduit/internal/runtime"
	"github.com/open-conduit/open-conduit/storage"
)

// humanHarness runs human.ask on the production toolbelt and scheduler for
// a real attempt, with a scope whose `human` entry is "decisions".
type humanHarness struct {
	*toolbeltHarness
	control    *runtime.Control
	actions    hu.Actions
	configured bool
}

// humanRoute is the fixture route, except that, like every daemon route, it
// gives Events on an ask key no Work: they reach the asker only as aliases.
func humanRoute(in extension.RouteInput) (extension.RouteDecision, error) {
	if k := in.Event.Event.CorrelationKey; k != nil && k.Source == hu.Type {
		return extension.RouteDecision{}, nil
	}
	return routingDecision(in)
}

func newHumanHarness(t *testing.T, mode, entry string) *humanHarness {
	t.Helper()
	var x struct {
		Name   string
		Config json.RawMessage
	}
	if err := json.Unmarshal([]byte(entry), &x); err != nil {
		t.Fatal(err)
	}
	cfg, err := hu.ParseConfig(x.Config)
	if err != nil {
		t.Fatal(err)
	}
	o := routingOptions(routingHandlerFunc(humanRoute), "queue")
	o.Capabilities = domain.Capabilities{Toolbelt: extension.ToolbeltCLI}
	f, executor := schedulerFixture(t, mode, o)
	base := f.Driver.(*schedulerDriver)
	th := &toolbeltHarness{f: f, executor: executor, echoes: new(int), deny: new(bool), mu: &sync.Mutex{}}
	h := &humanHarness{toolbeltHarness: th, configured: true}
	e := hu.Entry{Name: x.Name, Config: cfg}
	h.actions = hu.Actions{Store: f.Store, Entry: e, Now: f.Clock.Now}
	tools, err := hu.NewTools(hu.Options{Store: f.Store, Now: f.Clock.Now, Entry: func(scope domain.ID) (hu.Entry, bool) {
		return e, scope == "routing-scope" && h.configured
	}})
	if err != nil {
		t.Fatal(err)
	}
	ops := []string{extension.Emit, extension.CreateWork, extension.Wait, extension.DeclareOutcome, extension.ReadContext, hu.OpAsk}
	belt, err := runtime.NewToolbelt(f.Store, f.Clock.Now, base.router, runtime.ToolbeltOptions{
		Grant: func(context.Context, runtime.AttemptState) (runtime.ToolbeltGrant, error) {
			return runtime.ToolbeltGrant{Operations: ops}, nil
		},
		Authorize: func(context.Context, runtime.AuthorityCheck) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = belt.Register(tools.Registration()); err != nil {
		t.Fatal(err)
	}
	options := base.options
	options.Toolbelt, options.ToolbeltEndpoint = belt, "unix:///fixture/management.sock"
	options.AliasFilter = belt.DependencyFilter
	th.scheduler = newTestScheduler(t, f, options)
	th.belt = belt
	executor.onRun = func(ctx context.Context, r extension.RunRequest) (extension.RunHandle, error) {
		th.mu.Lock()
		th.token = r.Context.ToolbeltToken
		th.mu.Unlock()
		if err := r.PersistRecovery(domain.Payload{Schema: "fixture.recovery.v1", Data: []byte(`{}`)}); err != nil {
			return nil, err
		}
		handle := &schedulerHandle{result: extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeCompleted}}}
		executor.mu.Lock()
		if executor.handles == nil {
			executor.handles = map[domain.ID]extension.RunHandle{}
		}
		executor.handles[r.Execution.ID] = handle
		executor.mu.Unlock()
		return handle, nil
	}
	key := toolbeltKey
	requireAccept(t, f, "trigger", &key)
	requireRoute(t, f.Driver, "trigger")
	if th.attempt, err = th.scheduler.Start(context.Background(), "work/item"); err != nil {
		t.Fatal(err)
	}
	if h.control, err = runtime.NewControl(f.Store, f.Clock.Now, base.router, th.scheduler, runtime.CausalLimits{}); err != nil {
		t.Fatal(err)
	}
	return h
}

const decisions = `{"name":"decisions","type":"human","config":{}}`

func (h *humanHarness) target(t *testing.T) extension.ActionTarget {
	t.Helper()
	var tg extension.ActionTarget
	routingRead(t, h.f, func(tx storage.Tx) error {
		w, err := tx.Work("work/item")
		if err != nil {
			return err
		}
		tg.Work = &w
		tg.Scope, err = tx.Scope(w.ScopeID)
		return err
	})
	return tg
}

// ask calls human.ask and ends the attempt, so the wait registers.
func (h *humanHarness) ask(t *testing.T, input string) extension.ToolResult {
	t.Helper()
	out, err := h.belt.CallRequest(context.Background(), h.token, "ask-1", extension.ToolCall{Operation: hu.OpAsk, Input: domain.Payload{Schema: "human.ask.v1", Data: json.RawMessage(input)}})
	if err != nil || out.Wait == nil {
		t.Fatalf("human.ask: %+v %v", out, err)
	}
	if err = h.scheduler.Complete(context.Background(), h.attempt.Lease, extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeHandoff, Summary: "asked"}}); err != nil {
		t.Fatalf("complete: %v", err)
	}
	return out
}

// answer invokes the action as caller and accepts its Event as the
// management API does.
func (h *humanHarness) answer(t *testing.T, caller extension.Caller, input, key string) (domain.AcceptedEvent, error) {
	t.Helper()
	tg := h.target(t)
	plan, err := h.actions.Invoke(context.Background(), extension.ActionCall{Target: tg, Action: hu.ActionAnswer, Input: input, Caller: caller})
	if err != nil {
		return domain.AcceptedEvent{}, err
	}
	// The plan names the ask key, so the host addresses the Event by it.
	a, _, err := h.control.SubmitAction(context.Background(), runtime.ActionEvent{ScopeID: tg.Scope.ID, Provider: "decisions", Action: hu.ActionAnswer, Key: key, Event: plan.Event})
	return a, err
}

func refusedWith(t *testing.T, err error, code string) {
	t.Helper()
	var oe *extension.OperationError
	if !errors.As(err, &oe) || oe.Code != code {
		t.Fatalf("want refusal %s, got %v", code, err)
	}
}

func (h *humanHarness) reconcile(t *testing.T) {
	t.Helper()
	if err := h.scheduler.ReconcileSubscriptions(context.Background(), 100); err != nil {
		t.Fatal(err)
	}
}

func (h *humanHarness) eventsOfType(t *testing.T, typ string) []domain.AcceptedEvent {
	t.Helper()
	var out []domain.AcceptedEvent
	routingRead(t, h.f, func(tx storage.Tx) error {
		all, err := tx.Events(0, 0)
		for _, a := range all {
			if a.Event.Type == typ {
				out = append(out, a)
			}
		}
		return err
	})
	return out
}

// An ask parks the Work; only the authorised answer action, from one of
// the ask's responders, with a valid option, wakes it, and exactly once.
// A posted Event of the same family and key is not an answer.
func TestHumanAskAnsweredThroughTheAction(t *testing.T) {
	for _, mode := range []string{"persistent", "reopen"} {
		t.Run(mode, func(t *testing.T) {
			h := newHumanHarness(t, mode, decisions)
			ctx := context.Background()
			out := h.ask(t, `{"question":"Ship the migration tonight?","options":["yes","no"],"context":"private"}`)
			var told map[string]any
			if json.Unmarshal(out.Output.Data, &told) != nil || told["asked"] != true {
				t.Fatalf("output: %s", out.Output.Data)
			}
			if _, leaked := told["ask"]; leaked {
				t.Fatalf("the ask key reached the agent: %s", out.Output.Data)
			}
			w, work := waitState(t, h.toolbeltHarness, out.Wait.ID)
			if w.State != domain.WaitActive || work.State != domain.WorkWaiting {
				t.Fatalf("not parked: %v %v", w.State, work.State)
			}
			// The ask key is not the Work's.
			for _, k := range work.CorrelationKeys {
				if k.Source == hu.Type {
					t.Fatalf("the Work owns the ask key: %+v", k)
				}
			}

			acts, err := h.actions.Actions(ctx, h.target(t))
			if err != nil || len(acts) != 1 || acts[0].ID != hu.ActionAnswer || acts[0].Prompt == "" {
				t.Fatalf("answer not offered: %+v %v", acts, err)
			}
			if lines, ok := hu.Annotate(ctx, h.f.Store, h.actions.Entry, work, h.f.Clock.Now()); !ok || lines[0].Value != "Ship the migration tonight?" {
				t.Fatalf("annotation: %+v %v", lines, ok)
			}
			panels, err := h.actions.Panels(ctx, h.target(t))
			if err != nil || len(panels) != 1 || panels[0].Sections[0].Text != "Ship the migration tonight?" {
				t.Fatalf("question panel: %+v %v", panels, err)
			}
			if last := panels[0].Sections[len(panels[0].Sections)-1]; len(last.Actions) != 1 || last.Actions[0] != hu.ActionAnswer {
				t.Fatalf("question panel lacks the answer button: %+v", last)
			}

			_, err = h.answer(t, extension.Caller{Kind: "slack_user", ID: "U1"}, "yes", "stranger")
			refusedWith(t, err, "not_allowed")
			_, err = h.answer(t, extension.LocalOperator, "maybe", "bad")
			refusedWith(t, err, "invalid_input")

			// A posted Event with the answer's family, key and data.
			var rec struct{ ID string }
			routingRead(t, h.f, func(tx storage.Tx) error {
				id, err := tx.Get(hu.Namespace, "work/work/item")
				rec.ID = string(id)
				return err
			})
			key := domain.CorrelationKey{Source: hu.Type, ExternalID: "ask/" + rec.ID}
			forged, _ := json.Marshal(hu.AnswerPayload{Ask: rec.ID, Answer: "yes", Responder: "operator:local"})
			if _, _, err = h.control.SubmitEvent(ctx, runtime.ManualEvent{ScopeID: "routing-scope", Type: hu.TypeAnswer, DeliveryID: "forged", CorrelationKey: &key, Data: domain.Payload{Schema: hu.SchemaAnswer, Data: forged}}); err != nil {
				t.Fatal(err)
			}
			h.reconcile(t)
			if w, _ := waitState(t, h.toolbeltHarness, out.Wait.ID); w.State != domain.WaitActive {
				t.Fatalf("a posted Event answered the ask: %v", w.State)
			}
			if len(h.eventsOfType(t, hu.TypeAnswered)) != 0 {
				t.Fatal("a posted Event was aliased")
			}

			first, err := h.answer(t, extension.LocalOperator, "2", "a1")
			if err != nil {
				t.Fatal(err)
			}
			var p hu.AnswerPayload
			if first.Event.Source != extension.ActionSource || json.Unmarshal(first.Event.Data.Data, &p) != nil || p.Answer != "no" || p.Option != 2 || p.Responder != "operator:local" {
				t.Fatalf("answer event: %+v %s", first.Event, first.Event.Data.Data)
			}
			// Accepted but not yet woken: a second answer is refused.
			_, err = h.answer(t, extension.LocalOperator, "yes", "a2")
			refusedWith(t, err, "not_outstanding")

			h.reconcile(t)
			h.reconcile(t)
			w, work = waitState(t, h.toolbeltHarness, out.Wait.ID)
			if w.State != domain.WaitWoken || work.State != domain.WorkReady {
				t.Fatalf("answer did not wake: %v %v", w.State, work.State)
			}
			aliases := h.eventsOfType(t, hu.TypeAnswered)
			if len(aliases) != 1 || aliases[0].Event.CausationID != first.Event.ID || aliases[0].Event.ID != w.WakeEventID {
				t.Fatalf("aliases: %+v wake %s", aliases, w.WakeEventID)
			}
			var alias struct{ Label string }
			if json.Unmarshal(aliases[0].Event.Data.Data, &alias) != nil || alias.Label != "a person's answer" {
				t.Fatalf("alias payload: %s", aliases[0].Event.Data.Data)
			}

			// Closed: no action, refused, recorded as answered.
			if acts, _ := h.actions.Actions(ctx, h.target(t)); len(acts) != 0 {
				t.Fatalf("answer offered on a closed ask: %+v", acts)
			}
			_, err = h.answer(t, extension.LocalOperator, "yes", "a3")
			refusedWith(t, err, "not_outstanding")
			res, err := hu.Sync(ctx, h.f.Store, h.f.Clock.Now())
			if err != nil || res.Answered != 1 {
				t.Fatalf("sync: %+v %v", res, err)
			}
			a, err := hu.Settle(ctx, h.f.Store, rec.ID, h.f.Clock.Now())
			if err != nil || a.State != hu.StateAnswered || a.Answer == nil || a.Answer.Event != first.Event.ID || a.Answer.Answer != "no" {
				t.Fatalf("record: %+v %v", a, err)
			}
			if len(h.eventsOfType(t, hu.TypeAnswer)) != 2 {
				t.Fatal("expected the forged Event and one answer, nothing more")
			}
		})
	}
}

// Two answers that race both land; the lowest sequence wakes the asker and
// is the answer, the other is audited as late and rendered as not counting.
func TestHumanAskFirstAnswerWins(t *testing.T) {
	h := newHumanHarness(t, "persistent", decisions)
	ctx := context.Background()
	out := h.ask(t, `{"question":"Which region?"}`)
	tg := h.target(t)
	var plans []extension.ActionPlan
	for _, in := range []string{"eu-west", "us-east"} {
		plan, err := h.actions.Invoke(ctx, extension.ActionCall{Target: tg, Action: hu.ActionAnswer, Input: in, Caller: extension.LocalOperator})
		if err != nil {
			t.Fatal(err)
		}
		plans = append(plans, plan)
	}
	var accepted []domain.AcceptedEvent
	for i, plan := range plans {
		a, _, err := h.control.SubmitAction(ctx, runtime.ActionEvent{ScopeID: tg.Scope.ID, Provider: "decisions", Action: hu.ActionAnswer, Key: []string{"r1", "r2"}[i], Event: plan.Event})
		if err != nil {
			t.Fatal(err)
		}
		accepted = append(accepted, a)
	}
	h.reconcile(t)
	w, _ := waitState(t, h.toolbeltHarness, out.Wait.ID)
	aliases := h.eventsOfType(t, hu.TypeAnswered)
	if w.State != domain.WaitWoken || len(aliases) != 1 || aliases[0].Event.CausationID != accepted[0].Event.ID {
		t.Fatalf("wake: %v %+v", w.State, aliases)
	}
	if _, err := hu.Sync(ctx, h.f.Store, h.f.Clock.Now()); err != nil {
		t.Fatal(err)
	}
	// The asker is told the answer that woke it, and only that one.
	text, ok := hu.RenderEvent(aliases[0].Event, hu.Lookup(ctx, h.f.Store, h.f.Clock.Now))
	if !ok || !strings.Contains(text, "Which region?") || !strings.Contains(text, "eu-west") || strings.Contains(text, "us-east") {
		t.Fatalf("render: %s", text)
	}
	routingRead(t, h.f, func(tx storage.Tx) error {
		for _, a := range accepted {
			if _, err := tx.WorkByCorrelation(*a.Event.CorrelationKey); !errors.Is(err, storage.ErrNotFound) {
				t.Fatalf("an answer gave the ask key Work: %v", err)
			}
		}
		return nil
	})
	var rec struct{ ID string }
	routingRead(t, h.f, func(tx storage.Tx) error {
		id, err := tx.Get(hu.Namespace, "work/work/item")
		rec.ID = string(id)
		return err
	})
	a, err := hu.Settle(ctx, h.f.Store, rec.ID, h.f.Clock.Now())
	if err != nil || a.Answer == nil || a.Answer.Event != accepted[0].Event.ID || len(a.Late) != 1 || a.Late[0].Event != accepted[1].Event.ID {
		t.Fatalf("audit: %+v %v", a, err)
	}
}

// An ask whose entry does not let the local operator answer refuses it;
// an ask past its deadline wakes the asker without an answer and expires.
func TestHumanAskWrongResponderAndDeadline(t *testing.T) {
	h := newHumanHarness(t, "persistent", `{"name":"decisions","type":"human","config":{"responders":["slack_user:U1"]}}`)
	ctx := context.Background()
	deadline := h.f.Clock.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	out := h.ask(t, `{"question":"Approve?","deadline":"`+deadline+`"}`)
	_, err := h.answer(t, extension.LocalOperator, "yes", "op")
	refusedWith(t, err, "not_allowed")
	if len(h.eventsOfType(t, hu.TypeAnswer)) != 0 {
		t.Fatal("a refused answer left an Event")
	}
	h.f.Clock.Advance(2 * time.Hour)
	// The due timeout wakes the asker without an answer.
	if err := h.scheduler.Wake(ctx, out.Wait.ID); err != nil {
		t.Fatal(err)
	}
	if w, _ := waitState(t, h.toolbeltHarness, out.Wait.ID); w.State != domain.WaitWoken {
		t.Fatalf("deadline did not wake: %v", w.State)
	}
	if acts, _ := h.actions.Actions(ctx, h.target(t)); len(acts) != 0 {
		t.Fatalf("answer offered after the deadline: %+v", acts)
	}
	res, err := hu.Sync(ctx, h.f.Store, h.f.Clock.Now())
	if err != nil || res.Expired != 1 {
		t.Fatalf("sync: %+v %v", res, err)
	}
}

// A scope without a `human` entry refuses the ask and keeps authority.
func TestHumanAskNeedsAnEntry(t *testing.T) {
	h := newHumanHarness(t, "persistent", decisions)
	h.configured = false
	_, err := h.belt.Call(context.Background(), h.token, extension.ToolCall{Operation: hu.OpAsk, Input: domain.Payload{Schema: "human.ask.v1", Data: json.RawMessage(`{"question":"?"}`)}})
	refusedWith(t, err, "not_configured")
	if _, err = h.belt.Call(context.Background(), h.token, extension.ToolCall{Operation: extension.ReadContext}); err != nil {
		t.Fatalf("refusal released authority: %v", err)
	}
}

// The answer action meets the shared action contract.
func TestHumanAnswerActionContract(t *testing.T) {
	bad := "maybe"
	conformance.RunActions(t, func() conformance.ActionFixture {
		h := newHumanHarness(t, "persistent", decisions)
		h.ask(t, `{"question":"Go?","options":["yes","no"]}`)
		other := h.target(t)
		other.Work = nil
		return conformance.ActionFixture{Provider: h.actions, Available: h.target(t), Unavailable: other, Action: hu.ActionAnswer, Input: "yes", BadInput: &bad,
			Caller: extension.LocalOperator, Stranger: extension.Caller{Kind: "slack_user", ID: "U9"}, EventType: hu.TypeAnswer}
	})
}

// The question panel meets the shared panel contract: shown with its answer
// button on Work parked on an ask, and not on the scope.
func TestHumanQuestionPanelContract(t *testing.T) {
	conformance.RunPanels(t, func() conformance.PanelFixture {
		h := newHumanHarness(t, "persistent", decisions)
		h.ask(t, `{"question":"Go?","options":["yes","no"],"context":"the migration"}`)
		other := h.target(t)
		other.Work = nil
		return conformance.PanelFixture{Provider: h.actions, Shown: h.target(t), Hidden: other}
	})
}
