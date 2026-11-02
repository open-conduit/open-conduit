package config

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
)

// TestModeWarnings: a resource config's default mode that the binding's
// executor overrides with a mode of its own is warned about once, at the
// scope that declares the binding; agreeing or unset modes are quiet.
func TestModeWarnings(t *testing.T) {
	reg := Registry{Executors: map[string]extension.Executor{"agent": modeExecutor{}, "plain": fakeExecutor{}}}
	reg.ResourceModeDefault = map[string]func(json.RawMessage) string{"ws": func(c json.RawMessage) string {
		var m struct {
			Mode string `json:"mode"`
		}
		_ = json.Unmarshal(c, &m)
		return m.Mode
	}}
	bind := func(name, executor, config, provider, resource string) Binding {
		return Binding{Name: name, Executor: executor, Config: json.RawMessage(config), Resource: &Resource{Provider: provider, Config: json.RawMessage(resource)}}
	}
	overridden := bind("reviewer", "agent", `{"workspace_mode":"work"}`, "ws", `{"mode":"fresh"}`)
	snap := &Snapshot{Order: []domain.ID{"root", "child"}, Scopes: map[domain.ID]*EffectiveScope{
		"root": {ID: "root", Bindings: []Binding{
			overridden,
			bind("agree", "agent", `{"workspace_mode":"work"}`, "ws", `{"mode":"work"}`),
			bind("no-default", "agent", `{"workspace_mode":"work"}`, "ws", `{}`),
			bind("no-request", "agent", `{}`, "ws", `{"mode":"fresh"}`),
			bind("no-moder", "plain", `{}`, "ws", `{"mode":"fresh"}`),
			bind("no-hook", "agent", `{"workspace_mode":"work"}`, "other", `{"mode":"fresh"}`),
			{Name: "no-resource", Executor: "agent", Config: json.RawMessage(`{"workspace_mode":"work"}`)},
		}},
		// child inherits reviewer unchanged and redefines its own.
		"child": {ID: "child", ParentID: "root", Bindings: []Binding{
			overridden,
			bind("own", "agent", `{"workspace_mode":"clone"}`, "ws", `{"mode":"shared"}`),
		}},
	}}
	got := ModeWarnings(snap, reg)
	want := []string{
		`scope root binding reviewer: the ws resource's mode "fresh" never applies, because executor agent requests mode "work" with this config; remove the resource's mode, or set the mode in the executor's config`,
		`scope child binding own: the ws resource's mode "shared" never applies, because executor agent requests mode "clone" with this config; remove the resource's mode, or set the mode in the executor's config`,
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("warnings:\n%s", strings.Join(got, "\n"))
	}
	// Negative control: the same binding with agreeing modes is quiet.
	snap.Scopes["root"].Bindings[0] = bind("reviewer", "agent", `{"workspace_mode":"fresh"}`, "ws", `{"mode":"fresh"}`)
	snap.Order = []domain.ID{"root"}
	if got := ModeWarnings(snap, reg); len(got) != 0 {
		t.Fatalf("agreeing modes warned: %q", got)
	}
	if ModeWarnings(nil, reg) != nil {
		t.Fatal("nil snapshot warned")
	}
}

// warnExecutor warns about a binding config that sets "old".
type warnExecutor struct{ fakeExecutor }

func (warnExecutor) ConfigWarnings(raw json.RawMessage) []string {
	var c struct {
		Old bool `json:"old"`
	}
	if json.Unmarshal(raw, &c) != nil || !c.Old {
		return nil
	}
	return []string{"config.old is deprecated"}
}

// TestBindingWarnings: each executor's warnings about its bindings are
// prefixed with the scope and binding, a binding a child inherits unchanged
// warns once, and an executor without ConfigWarner warns about nothing
// (the negative control).
func TestBindingWarnings(t *testing.T) {
	reg := Registry{Executors: map[string]extension.Executor{"warn": warnExecutor{}, "plain": fakeExecutor{}}}
	old := Binding{Name: "agent", Executor: "warn", Config: json.RawMessage(`{"old": true}`)}
	snap := &Snapshot{Order: []domain.ID{"root", "child"}, Scopes: map[domain.ID]*EffectiveScope{
		"root": {ID: "root", Bindings: []Binding{
			old,
			{Name: "fine", Executor: "warn", Config: json.RawMessage(`{}`)},
			{Name: "plain", Executor: "plain", Config: json.RawMessage(`{"old": true}`)},
		}},
		"child": {ID: "child", ParentID: "root", Bindings: []Binding{
			old,
			{Name: "fine", Executor: "warn", Config: json.RawMessage(`{"old": true}`)},
		}},
	}}
	got := BindingWarnings(snap, reg)
	want := []string{"scope root binding agent: config.old is deprecated", "scope child binding fine: config.old is deprecated"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("%q", got)
	}
	if BindingWarnings(nil, reg) != nil {
		t.Fatal("no snapshot warned")
	}
}
