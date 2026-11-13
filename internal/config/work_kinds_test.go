package config

import (
	"encoding/json"
	"slices"
	"testing"
)

// workKindsConfig is one scope whose authorization lets its bindings ask for
// ceiling, whose shell binding asks for asks with operations ops, and whose
// work route binds routed.
func workKindsConfig(ceiling, asks, ops, routed []string) []byte {
	enc := func(v []string) string { b, _ := json.Marshal(v); return string(b) }
	return []byte(`{"version":1,"settings":{},"connections":[],"scopes":[{"id":"web","name":"web",
		"authorization":{"connections":[],"operations":["create_work","wait"],"read_scopes":[],"work_kinds":` + enc(ceiling) + `},
		"bindings":[{"name":"front","executor":"shell","requires":{"toolbelt":"cli"},"operations":` + enc(ops) + `,"work_kinds":` + enc(asks) + `,"secret_names":[],"config":{}}],
		"routes":[{"name":"children","handler":"work","binding":"front","active_policy":"queue","config":{"kinds":` + enc(routed) + `}}]}]}`)
}

// workRegistry registers a "work" handler whose route config's kinds are
// the kinds it binds.
func workRegistry() Registry {
	reg := registry()
	reg.Handlers["work"] = true
	reg.HandlerWorkKinds = map[string]func(json.RawMessage) []string{"work": func(raw json.RawMessage) []string {
		var c struct{ Kinds []string }
		_ = json.Unmarshal(raw, &c)
		return c.Kinds
	}}
	return reg
}

// A binding may ask for the kinds of child Work it names, within the scope's
// authorization, and the host grants only those.
func TestWorkKindsAuthorizeChildWork(t *testing.T) {
	s := mustValidate(t, workKindsConfig([]string{"agent.task.engineer", "agent.task.sales"}, []string{"agent.task.engineer"}, []string{"create_work"}, []string{"agent.task.engineer"}), workRegistry())
	if err := s.AuthorizeWorkKind("web", "front", "agent.task.engineer"); err != nil {
		t.Fatalf("a granted kind was refused: %v", err)
	}
	for _, kind := range []string{"agent.task.sales", "agent.task.other"} {
		if err := s.AuthorizeWorkKind("web", "front", kind); err == nil {
			t.Fatalf("kind %s the binding doesn't name was granted", kind)
		}
	}
	if err := s.AuthorizeWorkKind("web", "absent", "agent.task.engineer"); err == nil {
		t.Fatal("a binding that isn't there was granted a kind")
	}
	if b := s.Scopes["web"].Bindings[0]; !slices.Equal(b.WorkKinds, []string{"agent.task.engineer"}) {
		t.Fatalf("binding work_kinds = %v", b.WorkKinds)
	}
}

// A binding can't name a kind beyond the scope's ceiling, a kind no enabled
// route binds (its child Work would never run), nor kinds without
// create_work, and a kind must have the shape of one.
func TestWorkKindsValidation(t *testing.T) {
	for _, c := range []struct {
		name                       string
		ceiling, asks, ops, routed []string
		code                       string
	}{
		{"beyond the ceiling", []string{"agent.task.engineer"}, []string{"agent.task.sales"}, []string{"create_work"}, []string{"agent.task.sales"}, "work_kind_denied"},
		{"no route binds it", []string{"agent.task.engineer", "agent.task.sales"}, []string{"agent.task.sales"}, []string{"create_work"}, []string{"agent.task.engineer"}, "work_kind_unrouted"},
		{"no create_work", []string{"agent.task.engineer"}, []string{"agent.task.engineer"}, []string{"wait"}, []string{"agent.task.engineer"}, "work_kinds_unused"},
		{"not a kind", []string{"Engineer"}, []string{}, []string{"create_work"}, []string{"agent.task.engineer"}, "invalid_value"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := Validate(workKindsConfig(c.ceiling, c.asks, c.ops, c.routed), workRegistry())
			if err == nil {
				t.Fatal("accepted")
			}
			if got := codes(diagnostics(t, err)); !slices.Contains(got, c.code) {
				t.Fatalf("codes = %v, want %s", got, c.code)
			}
		})
	}
}
