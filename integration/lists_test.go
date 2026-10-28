package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/client"
	"github.com/open-conduit/open-conduit/executors/claudecode"
	"github.com/open-conduit/open-conduit/extension"
	gitws "github.com/open-conduit/open-conduit/extensions/git"
	"github.com/open-conduit/open-conduit/internal/config"
)

// workProvider offers an action and a panel on each Work, named for it.
type workProvider struct{}

func (workProvider) Actions(_ context.Context, t extension.ActionTarget) ([]extension.Action, error) {
	if t.Work == nil {
		return nil, nil
	}
	return []extension.Action{{ID: "nudge", Label: "Nudge " + string(t.Work.ID)}, {ID: "close", Label: "Close", Prompt: "why?", Choices: []string{"done", "dup"}}}, nil
}

func (workProvider) Invoke(context.Context, extension.ActionCall) (extension.ActionPlan, error) {
	return extension.ActionPlan{}, &extension.OperationError{Code: "unknown_action", Message: "no"}
}

func (workProvider) Panels(_ context.Context, t extension.ActionTarget) ([]extension.Panel, error) {
	if t.Work == nil {
		return nil, nil
	}
	return []extension.Panel{{ID: "status", Title: "Status", Sections: []extension.PanelSection{{Text: string(t.Work.ID), Actions: []string{"nudge", "missing"}}}}}, nil
}

// brokenProvider fails every call, counting them.
type brokenProvider struct{ calls *atomic.Int32 }

func (b brokenProvider) Actions(context.Context, extension.ActionTarget) ([]extension.Action, error) {
	b.calls.Add(1)
	return nil, errors.New("down")
}
func (brokenProvider) Invoke(context.Context, extension.ActionCall) (extension.ActionPlan, error) {
	return extension.ActionPlan{}, errors.New("down")
}
func (b brokenProvider) Panels(context.Context, extension.ActionTarget) ([]extension.Panel, error) {
	b.calls.Add(1)
	return nil, errors.New("down")
}

// buttonsProvider shows a panel with a button, but listing its actions
// fails, so the panel is shown without the button.
type buttonsProvider struct{ brokenProvider }

func (buttonsProvider) Panels(context.Context, extension.ActionTarget) ([]extension.Panel, error) {
	return []extension.Panel{{ID: "ask", Title: "Ask", Sections: []extension.PanelSection{{Text: "Approve?", Actions: []string{"approve"}}}}}, nil
}

// stuckProvider blocks every call until it is given up on, closing entered
// on the first.
type stuckProvider struct {
	entered chan time.Time
	once    *sync.Once
	calls   *atomic.Int32
}

func (s stuckProvider) block(ctx context.Context) error {
	s.calls.Add(1)
	s.once.Do(func() { close(s.entered) })
	<-ctx.Done()
	return ctx.Err()
}
func (s stuckProvider) Actions(ctx context.Context, _ extension.ActionTarget) ([]extension.Action, error) {
	return nil, s.block(ctx)
}
func (stuckProvider) Invoke(context.Context, extension.ActionCall) (extension.ActionPlan, error) {
	return extension.ActionPlan{}, errors.New("stuck")
}
func (s stuckProvider) Panels(ctx context.Context, _ extension.ActionTarget) ([]extension.Panel, error) {
	return nil, s.block(ctx)
}

func newStuck() stuckProvider {
	return stuckProvider{entered: make(chan time.Time), once: &sync.Once{}, calls: &atomic.Int32{}}
}

// emitWork routes Events for keys a, b and c (a and b twice) to three Work
// in ops.
func emitWork(h *managementHarness) {
	for i, key := range []string{"a", "b", "c", "a", "b"} {
		h.mustCLI(nil, "events", "emit", "--scope-id", "ops", "--type", "operator.requested", "--delivery-id", fmt.Sprintf("d%d", i), "--key-source", "fixture", "--key-external-id", key, "--key", fmt.Sprintf("k%d", i))
	}
}

// The list forms return, for each Work and Event on the page, exactly what
// the single-target endpoints return for it.
func TestManagementListForms(t *testing.T) {
	h := newManagementHarness(t)
	broken := &atomic.Int32{}
	h.actions = map[string]extension.ActionProvider{"fixture": workProvider{}, "broken": brokenProvider{broken}, "buttons": buttonsProvider{brokenProvider{&atomic.Int32{}}}}
	ctx := context.Background()
	c := h.operatorClient()
	emitWork(h)

	page, err := c.WorksWithActions(ctx, url.Values{"scope_id": {"ops"}})
	if err != nil {
		t.Fatal(err)
	}
	if n := broken.Load(); n != 2 {
		t.Fatalf("a failing provider was asked %d times on one page, want once for actions and once for panels", n)
	}
	plain, err := c.Works(ctx, url.Values{"scope_id": {"ops"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 3 || len(plain.Items) != 3 {
		t.Fatalf("works: %d, %d", len(page.Items), len(plain.Items))
	}
	for i, w := range page.Items {
		actions, err := c.Actions(ctx, client.ActionTarget{WorkID: w.ID})
		if err != nil {
			t.Fatal(err)
		}
		panels, err := c.Panels(ctx, client.ActionTarget{WorkID: w.ID})
		if err != nil {
			t.Fatal(err)
		}
		if len(actions) != 2 || len(panels) != 2 || panels[0].Provider != "buttons" || panels[0].Sections[0].Actions != nil {
			t.Fatalf("fixture: %+v %+v", actions, panels)
		}
		if !reflect.DeepEqual(w.Actions, actions) || !reflect.DeepEqual(w.Panels, panels) {
			t.Fatalf("%s: list %+v %+v, single %+v %+v", w.ID, w.Actions, w.Panels, actions, panels)
		}
		w.Actions, w.Panels = nil, nil
		if !reflect.DeepEqual(w, plain.Items[i]) {
			t.Fatalf("listed Work differs: %+v %+v", w, plain.Items[i])
		}
	}
	if plain.Items[0].Actions != nil || plain.Items[0].Panels != nil {
		t.Fatal("a plain list carries actions")
	}
	only, err := c.Works(ctx, url.Values{"scope_id": {"ops"}, "with": {"panels"}})
	if err != nil || only.Items[0].Actions != nil || len(only.Items[0].Panels) != 2 {
		t.Fatalf("with=panels: %+v %v", only.Items, err)
	}

	// Following the cursor keeps with, and covers the same Work.
	var followed []client.Work
	q := url.Values{"scope_id": {"ops"}, "limit": {"2"}}
	for {
		p, err := c.WorksWithActions(ctx, q)
		if err != nil {
			t.Fatal(err)
		}
		followed = append(followed, p.Items...)
		if p.NextCursor == "" {
			break
		}
		q.Set("cursor", p.NextCursor)
	}
	if !reflect.DeepEqual(followed, page.Items) {
		t.Fatalf("paged: %+v\nwhole: %+v", followed, page.Items)
	}
	if _, err := c.Works(ctx, url.Values{"scope_id": {"ops"}, "limit": {"2"}, "cursor": {q.Get("cursor")}}); !isStatus(err, 400) {
		t.Fatalf("a with cursor followed without with: %v", err)
	}

	// state takes a set, such as every open Work.
	open, err := c.Works(ctx, url.Values{"state": {"ready,running,waiting"}})
	if err != nil || len(open.Items) != 3 {
		t.Fatalf("open Work: %+v %v", open.Items, err)
	}
	if none, err := c.Works(ctx, url.Values{"state": {"completed,cancelled"}}); err != nil || len(none.Items) != 0 {
		t.Fatalf("terminal Work: %+v %v", none.Items, err)
	}

	// A page that asks for actions is bounded, and with takes known names.
	for _, q := range []url.Values{{"with": {"actions"}, "limit": {"51"}}, {"with": {"bogus"}}, {"with": {"actions,actions"}}} {
		if _, err := c.Works(ctx, q); !isStatus(err, 400) {
			t.Fatalf("%v: %v", q, err)
		}
	}
	if _, err := c.Events(ctx, url.Values{"with": {"actions"}}); !isStatus(err, 400) {
		t.Fatalf("events with=actions: %v", err)
	}

	// A control Event (a hold) is not routed by a route; it lists with the
	// same explanation as the routed ones.
	work := page.Items[0].ID
	h.mustCLI(nil, "work", "hold", work, "--revision", page.Items[0].Revision, "--key", "hold")
	events, err := c.EventsWithRoutes(ctx, url.Values{"work_id": {work}})
	if err != nil {
		t.Fatal(err)
	}
	plainEvents, err := c.Events(ctx, url.Values{"work_id": {work}})
	if err != nil {
		t.Fatal(err)
	}
	if len(events.Items) < 3 || len(events.Items) != len(plainEvents.Items) {
		t.Fatalf("events: %d, %d", len(events.Items), len(plainEvents.Items))
	}
	routed, unrouted := 0, 0
	for i, e := range events.Items {
		route, err := c.EventRoute(ctx, e.ID)
		if err != nil {
			t.Fatal(err)
		}
		if e.Route == nil || !reflect.DeepEqual(*e.Route, route) {
			t.Fatalf("%s: list %+v, single %+v", e.ID, e.Route, route)
		}
		if route.Route == "" {
			unrouted++
		} else {
			routed++
		}
		e.Route = nil
		if !reflect.DeepEqual(e, plainEvents.Items[i]) {
			t.Fatalf("listed Event differs: %+v %+v", e, plainEvents.Items[i])
		}
	}
	if routed == 0 || unrouted == 0 {
		t.Fatalf("want routed and unrouted Events, got %d and %d", routed, unrouted)
	}
}

// A provider that hangs is given up on after its timeout, once a page,
// and the other providers' actions and panels still come back.
func TestManagementListFormsHungProvider(t *testing.T) {
	h := newManagementHarness(t)
	stuck := newStuck()
	h.actions = map[string]extension.ActionProvider{"fixture": workProvider{}, "stuck": stuck}
	h.providerTimeout = func(p string) <-chan time.Time {
		if p == "stuck" {
			return stuck.entered
		}
		return nil
	}
	ctx := context.Background()
	c := h.operatorClient()
	emitWork(h)
	page, err := c.WorksWithActions(ctx, url.Values{"scope_id": {"ops"}})
	if err != nil {
		t.Fatal(err)
	}
	if n := stuck.calls.Load(); n > 2 {
		t.Fatalf("a hung provider was asked %d times on one page", n)
	}
	for _, w := range page.Items {
		if len(w.Actions) != 2 || w.Actions[0].Provider != "fixture" || len(w.Panels) != 1 || w.Panels[0].Provider != "fixture" {
			t.Fatalf("%s: %+v %+v", w.ID, w.Actions, w.Panels)
		}
	}
}

// Once a page's view deadline passes, the Work not yet decorated get
// nothing, and the page still comes back.
func TestManagementListFormsViewDeadline(t *testing.T) {
	h := newManagementHarness(t)
	stuck := newStuck()
	h.actions = map[string]extension.ActionProvider{"fixture": workProvider{}, "stuck": stuck}
	h.viewDeadline = func() <-chan time.Time { return stuck.entered }
	ctx := context.Background()
	c := h.operatorClient()
	emitWork(h)
	page, err := c.WorksWithActions(ctx, url.Values{"scope_id": {"ops"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 3 {
		t.Fatalf("works: %d", len(page.Items))
	}
	for _, w := range page.Items {
		if w.Actions == nil || len(w.Actions) != 0 || w.Panels == nil || len(w.Panels) != 0 {
			t.Fatalf("%s after the deadline: %+v %+v", w.ID, w.Actions, w.Panels)
		}
	}
}

// Text that must stay out of GET /config: fake secret references, and
// credentials in fields no type declares safe to show.
var leaks = []string{
	"env:OC_FAKE_CONNECTION_REF", "cmd:print-fake-scope-ref", "file:fake_scope_ref",
	"hunter2", "Bearer", "fake-api-key", "hooks.example.invalid", "fake-pass", "fake-auth",
}

const detailConfig = `{
  "version": 1,
  "settings": {},
  "connections": [{"id": "code", "type": "code", "config": {"repository": "acme/widgets"}, "secret_refs": {"token": "env:OC_FAKE_CONNECTION_REF"}}],
  "scopes": [{
    "id": "ops", "name": "Operations",
    "authorization": {"connections": ["code"], "operations": ["emit", "read_context", "declare_outcome"], "read_scopes": []},
    "bindings": [
      {"name": "run", "executor": "fixture", "connection": "code", "requires": {"toolbelt": "cli"}, "operations": ["emit", "read_context"], "secret_names": ["api"],
        "config": {"command": "curl -H 'Authorization: Bearer fake-token' https://bob:hunter2@jira.example.invalid", "argv": ["--api-key", "fake-api-key"]}},
      {"name": "agent", "executor": "claude", "requires": {"workspace": true, "toolbelt": "mcp"}, "operations": ["emit"], "secret_names": [],
        "config": {"permission_mode": "acceptEdits", "extra_args": ["--api-key", "fake-api-key"], "env": {"AUTH": "Bearer fake-token"}},
        "resource": {"provider": "git", "config": {"checkout": "./checkout", "mode": "work", "setup": "curl -H 'Authorization: Bearer fake' https://bob:hunter2@example.invalid"}}}
    ],
    "routes": [{"name": "incoming", "handler": "manual", "binding": "run", "active_policy": "queue", "config": {}}],
    "extensions": [{"name": "issues", "type": "fixture_entry", "connection": "code", "secret_names": [],
      "config": {"repository": "acme/widgets", "labels": ["bug", "agent"], "limit": 30, "enabled": true,
        "base_url": "https://bob:hunter2@jira.example.invalid", "webhook_url": "https://hooks.example.invalid/services/T0/B0/fake",
        "auth_header": "fake-auth", "key": "fake-api-key", "pass": "fake-pass"}}],
    "secret_refs": {"api": "cmd:print-fake-scope-ref", "other": "file:fake_scope_ref"}
  }]
}`

// fixtureEntrySettings declares four of the fixture entry's fields safe.
func fixtureEntrySettings(raw json.RawMessage) []extension.Setting {
	var c struct {
		Repository string   `json:"repository"`
		Labels     []string `json:"labels"`
		Limit      int      `json:"limit"`
		Enabled    bool     `json:"enabled"`
	}
	if json.Unmarshal(raw, &c) != nil {
		return nil
	}
	return []extension.Setting{{Name: "repository", Value: c.Repository}, {Name: "labels", Value: c.Labels}, {Name: "limit", Value: c.Limit}, {Name: "enabled", Value: c.Enabled}}
}

// GET /config carries each scope's ceiling, each binding's requirements
// and operations, and config only through each type's allowlist: declared
// fields with typed values and defaults, every other field by name alone.
func TestManagementConfigDetailWithoutSecrets(t *testing.T) {
	h := newManagementHarnessWith(t, detailConfig, func(r *config.Registry) {
		r.Executors["claude"] = &claudecode.Executor{}
		r.Connections = map[string]func(json.RawMessage) error{"code": nil}
		r.Extensions = map[string]config.ExtensionType{"fixture_entry": {Settings: fixtureEntrySettings}}
		r.Resources = map[string]bool{gitws.Type: true}
		r.ResourceConfig = map[string]func(json.RawMessage) error{gitws.Type: gitws.ValidateConfig}
		r.ResourceSettings = map[string]func(json.RawMessage) []extension.Setting{gitws.Type: gitws.Settings}
	})
	ctx := context.Background()
	c := h.operatorClient()
	got, err := c.Config(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var raw json.RawMessage
	if err = c.Do(ctx, "GET", "config", nil, nil, "", &raw); err != nil {
		t.Fatal(err)
	}
	for _, leak := range leaks {
		if strings.Contains(string(raw), leak) {
			t.Fatalf("%q in /config: %s", leak, raw)
		}
	}
	sc := got.Scopes[0]
	if !reflect.DeepEqual(sc.Authorization, client.ConfigAuthorization{Connections: []string{"code"}, Operations: []string{"emit", "read_context", "declare_outcome"}, ReadScopes: []string{}}) {
		t.Fatalf("authorization: %+v", sc.Authorization)
	}
	settings := func(ss []client.ConfigSetting) string {
		var parts []string
		for _, s := range ss {
			if s.Set {
				parts = append(parts, s.Name+" set")
			} else {
				v, _ := json.Marshal(s.Value)
				parts = append(parts, s.Name+"="+string(v))
			}
		}
		return strings.Join(parts, " ")
	}
	run, agent := sc.Bindings[0], sc.Bindings[1]
	if run.Requires.Toolbelt != "cli" || !reflect.DeepEqual(run.Operations, []string{"emit", "read_context"}) {
		t.Fatalf("binding: %+v", run)
	}
	// An executor that declares nothing shows its fields by name alone.
	if s := settings(run.Settings); s != "argv set command set" {
		t.Fatalf("undeclared executor settings: %s", s)
	}
	// Claude Code declares its model, which shows its default when unset.
	if s := settings(agent.Settings); s != `model="opus" permission_mode="acceptEdits" playbook="work-issue" structured=true workspace_mode="work" cold_turns="handoff" handoff_minutes=55 env set extra_args set` {
		t.Fatalf("claude settings: %s", s)
	}
	if agent.Resource != gitws.Type || !agent.Requires.Workspace {
		t.Fatalf("agent: %+v", agent)
	}
	if s := settings(agent.ResourceSettings); s != `checkout="./checkout" mode="work" remote="origin" base_branch="" branch_template="agent/work-{work}" sign=false setup set` {
		t.Fatalf("git settings: %s", s)
	}
	if s := settings(sc.Extensions[0].Settings); s != `repository="acme/widgets" labels=["bug","agent"] limit=30 enabled=true auth_header set base_url set key set pass set webhook_url set` {
		t.Fatalf("extension settings: %s", s)
	}
	// Values keep their JSON types on the wire.
	for _, want := range []string{`{"name":"limit","value":30}`, `{"name":"enabled","value":true}`, `{"name":"labels","value":["bug","agent"]}`, `{"name":"handoff_minutes","value":55}`} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("missing %s in %s", want, raw)
		}
	}
}
