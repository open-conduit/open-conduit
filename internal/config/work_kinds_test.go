package config

import (
	"encoding/json"
	"slices"
	"testing"
)

// workKindsConfig is one scope whose authorization lets its bindings ask for
// ceiling and whose shell binding asks for asks with operations ops.
func workKindsConfig(ceiling, asks, ops []string) []byte {
	enc := func(v []string) string { b, _ := json.Marshal(v); return string(b) }
	return []byte(`{"version":1,"settings":{},"connections":[],"scopes":[{"id":"web","name":"web",
		"authorization":{"connections":[],"operations":["create_work","wait"],"read_scopes":[],"work_kinds":` + enc(ceiling) + `},
		"bindings":[{"name":"front","executor":"shell","requires":{"toolbelt":"cli"},"operations":` + enc(ops) + `,"work_kinds":` + enc(asks) + `,"secret_names":[],"config":{}}]}]}`)
}

// A binding may ask for the kinds of child Work it names, within the scope's
// authorization, and the host grants only those.
func TestWorkKindsAuthorizeChildWork(t *testing.T) {
	s := mustValidate(t, workKindsConfig([]string{"agent.task.engineer", "agent.task.sales"}, []string{"agent.task.engineer"}, []string{"create_work"}), registry())
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

// A binding can't name a kind beyond the scope's ceiling, nor kinds without
// create_work, and a kind must have the shape of one.
func TestWorkKindsValidation(t *testing.T) {
	for _, c := range []struct {
		name               string
		ceiling, asks, ops []string
		code               string
	}{
		{"beyond the ceiling", []string{"agent.task.engineer"}, []string{"agent.task.sales"}, []string{"create_work"}, "work_kind_denied"},
		{"no create_work", []string{"agent.task.engineer"}, []string{"agent.task.engineer"}, []string{"wait"}, "work_kinds_unused"},
		{"not a kind", []string{"Engineer"}, []string{}, []string{"create_work"}, "invalid_value"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := Validate(workKindsConfig(c.ceiling, c.asks, c.ops), registry())
			if err == nil {
				t.Fatal("accepted")
			}
			if got := codes(diagnostics(t, err)); !slices.Contains(got, c.code) {
				t.Fatalf("codes = %v, want %s", got, c.code)
			}
		})
	}
}
