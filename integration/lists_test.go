package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/open-conduit/open-conduit/client"
	"github.com/open-conduit/open-conduit/extension"
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

// brokenProvider fails every call, so it contributes nothing.
type brokenProvider struct{}

func (brokenProvider) Actions(context.Context, extension.ActionTarget) ([]extension.Action, error) {
	return nil, errors.New("down")
}
func (brokenProvider) Invoke(context.Context, extension.ActionCall) (extension.ActionPlan, error) {
	return extension.ActionPlan{}, errors.New("down")
}
func (brokenProvider) Panels(context.Context, extension.ActionTarget) ([]extension.Panel, error) {
	return nil, errors.New("down")
}

// The list forms return, for each Work and Event on the page, exactly what
// the single-target endpoints return for it.
func TestManagementListForms(t *testing.T) {
	h := newManagementHarness(t)
	h.actions = map[string]extension.ActionProvider{"fixture": workProvider{}, "broken": brokenProvider{}}
	ctx := context.Background()
	c := h.operatorClient()
	for i, key := range []string{"a", "b", "c", "a", "b"} {
		h.mustCLI(nil, "events", "emit", "--scope-id", "ops", "--type", "operator.requested", "--delivery-id", fmt.Sprintf("d%d", i), "--key-source", "fixture", "--key-external-id", key, "--key", fmt.Sprintf("k%d", i))
	}

	page, err := c.WorksWithActions(ctx, url.Values{"scope_id": {"ops"}})
	if err != nil {
		t.Fatal(err)
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
		if len(actions) != 2 || len(panels) != 1 {
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
	if err != nil || only.Items[0].Actions != nil || len(only.Items[0].Panels) != 1 {
		t.Fatalf("with=panels: %+v %v", only.Items, err)
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

	work := page.Items[0].ID
	events, err := c.EventsWithRoutes(ctx, url.Values{"work_id": {work}})
	if err != nil {
		t.Fatal(err)
	}
	plainEvents, err := c.Events(ctx, url.Values{"work_id": {work}})
	if err != nil {
		t.Fatal(err)
	}
	if len(events.Items) < 2 || len(events.Items) != len(plainEvents.Items) {
		t.Fatalf("events: %d, %d", len(events.Items), len(plainEvents.Items))
	}
	for i, e := range events.Items {
		route, err := c.EventRoute(ctx, e.ID)
		if err != nil {
			t.Fatal(err)
		}
		if e.Route == nil || !reflect.DeepEqual(*e.Route, route) || route.Route == "" {
			t.Fatalf("%s: list %+v, single %+v", e.ID, e.Route, route)
		}
		e.Route = nil
		if !reflect.DeepEqual(e, plainEvents.Items[i]) {
			t.Fatalf("listed Event differs: %+v %+v", e, plainEvents.Items[i])
		}
	}
}

// Obviously fake reference text, each of which must stay out of GET /config.
var fakeRefs = []string{"env:OC_FAKE_CONNECTION_REF", "cmd:print-fake-scope-ref", "file:fake_scope_ref", "env:OC_FAKE_INLINE_REF", "keyring:fake-inline-ref", "plain-fake-token-value"}

const detailConfig = `{
  "version": 1,
  "settings": {},
  "connections": [{"id": "code", "type": "code", "config": {"repository": "acme/widgets"}, "secret_refs": {"token": "env:OC_FAKE_CONNECTION_REF"}}],
  "scopes": [{
    "id": "ops", "name": "Operations",
    "authorization": {"connections": ["code"], "operations": ["emit", "read_context", "declare_outcome"], "read_scopes": []},
    "bindings": [{"name": "run", "executor": "fixture", "connection": "code", "requires": {"toolbelt": "cli"}, "operations": ["emit", "read_context"], "secret_names": ["api"],
      "config": {"model": "opus", "max_turns": 30, "flags": ["--fast", "cmd:print-fake-scope-ref"], "api_key": "plain-fake-token-value"},
      "resource": {"provider": "folder", "config": {"checkout": "./checkout", "fallback": "file:fake_scope_ref"}}}],
    "routes": [{"name": "incoming", "handler": "manual", "binding": "run", "active_policy": "queue", "config": {}}],
    "extensions": [{"name": "issues", "type": "fixture_entry", "connection": "code", "secret_names": [],
      "config": {"repository": "acme/widgets", "labels": ["bug", "agent"], "token": "plain-fake-token-value", "fallback": "env:OC_FAKE_INLINE_REF", "keychain": "keyring:fake-inline-ref", "limits": {"a": 1, "b": 2}}}],
    "secret_refs": {"api": "cmd:print-fake-scope-ref", "other": "file:fake_scope_ref"}
  }]
}`

// GET /config carries each scope's ceiling and a summary of each binding's
// and extension's config, and no secret reference text anywhere.
func TestManagementConfigDetailWithoutSecrets(t *testing.T) {
	h := newManagementHarnessWith(t, detailConfig, func(r *config.Registry) {
		r.Connections = map[string]func(json.RawMessage) error{"code": nil}
		r.Extensions = map[string]config.ExtensionType{"fixture_entry": {}}
		r.Resources = map[string]bool{"folder": true}
	})
	ctx := context.Background()
	c := h.operatorClient()
	got, err := c.Config(ctx)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(got)
	var direct json.RawMessage
	if err = c.Do(ctx, "GET", "config", nil, nil, "", &direct); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{string(raw), string(direct)} {
		for _, ref := range fakeRefs {
			if strings.Contains(body, ref) {
				t.Fatalf("reference text %q in /config: %s", ref, body)
			}
		}
	}
	sc := got.Scopes[0]
	if !reflect.DeepEqual(sc.Authorization, client.ConfigAuthorization{Connections: []string{"code"}, Operations: []string{"emit", "read_context", "declare_outcome"}, ReadScopes: []string{}}) {
		t.Fatalf("authorization: %+v", sc.Authorization)
	}
	b := sc.Bindings[0]
	entries := func(es []client.ConfigEntry) map[string]string {
		m := map[string]string{}
		for _, e := range es {
			m[e.Name] = e.Detail
		}
		return m
	}
	if b.Requires.Toolbelt != "cli" || !reflect.DeepEqual(b.Operations, []string{"emit", "read_context"}) || b.Resource != "folder" {
		t.Fatalf("binding: %+v", b)
	}
	if !reflect.DeepEqual(entries(b.Settings), map[string]string{"model": "opus", "max_turns": "30", "flags": "[redacted]", "api_key": "[redacted]"}) {
		t.Fatalf("binding settings: %+v", b.Settings)
	}
	if !reflect.DeepEqual(entries(b.ResourceSettings), map[string]string{"checkout": "./checkout", "fallback": "[redacted]"}) {
		t.Fatalf("resource settings: %+v", b.ResourceSettings)
	}
	x := sc.Extensions[0]
	if !reflect.DeepEqual(entries(x.Settings), map[string]string{"repository": "acme/widgets", "labels": "bug, agent", "token": "[redacted]", "fallback": "[redacted]", "keychain": "[redacted]", "limits": "2 fields"}) {
		t.Fatalf("extension settings: %+v", x.Settings)
	}
}
