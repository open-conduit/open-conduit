package config

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
)

// playbookExecutor starts its turns on the binding config's playbook.
type playbookExecutor struct{ fakeExecutor }

func (playbookExecutor) Playbook(raw json.RawMessage) string {
	var c struct {
		Playbook string `json:"playbook"`
	}
	_ = json.Unmarshal(raw, &c)
	return c.Playbook
}

// toolsetRegistry registers the tracker extension's operations and its
// author toolset at version (1: view and comment; 2 adds edit), and a
// playbook that relies on all three.
func toolsetRegistry(version int) Registry {
	reg := registry()
	reg.Executors["agent"] = playbookExecutor{fakeExecutor{domain.Capabilities{Toolbelt: "mcp"}}}
	reg.Operations = []string{"tracker.view", "tracker.comment", "tracker.edit", "tracker.delete"}
	author := extension.Toolset{Name: "tracker.author", Operations: []extension.ToolsetOperation{
		{Name: extension.ReadContext}, {Name: "tracker.view"}, {Name: "tracker.comment"}}}
	if version >= 2 {
		author.Operations = append(author.Operations, extension.ToolsetOperation{Name: "tracker.edit", Since: 2})
	}
	reg.Toolsets = map[string]extension.Toolset{author.Name: author}
	reg.Playbooks = map[string][]string{"work": {"tracker.view", "tracker.comment", "tracker.edit"}}
	return reg
}

// grantConfig is one scope whose authorization and agent binding grant
// scopeOps and bindingOps, the binding starting its turns on playbook work.
func grantConfig(scopeOps, bindingOps []string) []byte {
	enc := func(v []string) string { b, _ := json.Marshal(v); return string(b) }
	return []byte(`{"version":1,"settings":{},"connections":[],"scopes":[{"id":"web","name":"web",
		"authorization":{"connections":[],"operations":` + enc(scopeOps) + `,"read_scopes":[]},
		"bindings":[{"name":"agent","executor":"agent","requires":{"toolbelt":"mcp"},"operations":` + enc(bindingOps) + `,"secret_names":[],"config":{"playbook":"work"}}],
		"routes":[{"name":"issues","handler":"issue","binding":"agent","active_policy":"queue","config":{}}]}]}`)
}

func mustValidate(t *testing.T, data []byte, reg Registry) *Snapshot {
	t.Helper()
	s, err := Validate(data, reg)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func allowed(s *Snapshot, op string) bool { return s.AuthorizeOperation("web", "agent", op) == nil }

// A toolset grants what the running release puts in it: a config written
// against version 1 gets the operation version 2 adds with no change,
// while an explicit list of version 1's operations still doesn't.
func TestToolsetGrantsOperationsAddedLater(t *testing.T) {
	toolset := grantConfig([]string{"@tracker.author"}, []string{"@tracker.author"})
	if s := mustValidate(t, toolset, toolsetRegistry(1)); allowed(s, "tracker.edit") || !allowed(s, "tracker.comment") {
		t.Fatalf("version 1: %+v", s.Scopes["web"].Bindings[0].Operations)
	}
	s := mustValidate(t, toolset, toolsetRegistry(2))
	if !allowed(s, "tracker.edit") {
		t.Fatalf("version 2 does not grant tracker.edit to a toolset grant: %+v", s.Scopes["web"].Authorization.Operations)
	}
	if got := s.Scopes["web"].Bindings[0].Operations; !slices.Equal(got, []string{"read_context", "tracker.view", "tracker.comment", "tracker.edit"}) {
		t.Fatalf("resolved binding operations: %v", got)
	}
	// The stored text is what the operator wrote; only the snapshot resolves.
	if !strings.Contains(string(s.Normalized), `"@tracker.author"`) {
		t.Fatalf("normalized config lost the toolset: %s", s.Normalized)
	}
	// Negative control: the same operations named one by one.
	explicit := []string{"read_context", "tracker.view", "tracker.comment"}
	if s := mustValidate(t, grantConfig(explicit, explicit), toolsetRegistry(2)); allowed(s, "tracker.edit") {
		t.Fatal("an explicit list without tracker.edit is granted it")
	}
}

func TestDeniedOperationLeavesTheToolset(t *testing.T) {
	for name, c := range map[string][2][]string{
		"scope":   {{"@tracker.author", "!tracker.edit"}, {"@tracker.author"}},
		"binding": {{"@tracker.author"}, {"@tracker.author", "!tracker.edit"}},
	} {
		s := mustValidate(t, grantConfig(c[0], c[1]), toolsetRegistry(2))
		if allowed(s, "tracker.edit") || !allowed(s, "tracker.comment") {
			t.Errorf("%s: !tracker.edit: edit %v, comment %v", name, allowed(s, "tracker.edit"), allowed(s, "tracker.comment"))
		}
	}
	// A whole toolset can be denied too.
	s := mustValidate(t, grantConfig([]string{"@tracker.author", "tracker.delete"}, []string{"tracker.delete", "@tracker.author", "!@tracker.author"}), toolsetRegistry(2))
	if got := s.Scopes["web"].Bindings[0].Operations; !slices.Equal(got, []string{"tracker.delete"}) {
		t.Fatalf("!@tracker.author: %v", got)
	}
}

// What a list names that isn't registered fails closed, a denial included,
// so a typo never grants or fails to deny.
func TestUnknownToolsetFailsClosed(t *testing.T) {
	for name, c := range map[string][2][]string{
		"toolset":        {{"@tracker.author", "@tracker.writer"}, {"@tracker.author"}},
		"binding":        {{"@tracker.author"}, {"@tracker.autor"}},
		"denial":         {{"@tracker.author", "!tracker.edti"}, {"@tracker.author"}},
		"operation":      {{"@tracker.author", "tracker.vew"}, {"@tracker.author"}},
		"widening":       {{"tracker.view"}, {"tracker.view", "tracker.delete"}},
		"toolset prefix": {{"@@tracker.author"}, {}},
	} {
		_, err := Validate(grantConfig(c[0], c[1]), toolsetRegistry(2))
		var invalid *ValidationError
		if !errors.As(err, &invalid) {
			t.Errorf("%s: validated: %v", name, err)
		}
	}
}

// A toolset a binding names is narrowed to the scope's grant, never refused,
// so a release adding to it can't stop a config with an explicit scope list
// from loading.
func TestToolsetNarrowsToTheScope(t *testing.T) {
	s := mustValidate(t, grantConfig([]string{"read_context", "tracker.view", "tracker.comment"}, []string{"@tracker.author"}), toolsetRegistry(2))
	if allowed(s, "tracker.edit") || !allowed(s, "tracker.view") {
		t.Fatalf("narrowed: %v", s.Scopes["web"].Bindings[0].Operations)
	}
	// A child scope's toolset is narrowed to its parent's in the same way.
	data := []byte(`{"version":1,"settings":{},"connections":[],"scopes":[
		{"id":"root","name":"root","authorization":{"connections":[],"operations":["tracker.view"],"read_scopes":[]}},
		{"id":"web","name":"web","parent":"root","authorization":{"connections":[],"operations":["@tracker.author"],"read_scopes":[]}}]}`)
	s = mustValidate(t, data, toolsetRegistry(2))
	if got := s.Scopes["web"].Authorization.Operations; !slices.Equal(got, []string{"tracker.view"}) {
		t.Fatalf("child: %v", got)
	}
}

func TestGrantWarningsReportPlaybookDrift(t *testing.T) {
	reg := toolsetRegistry(2)
	short := []string{"read_context", "tracker.view", "tracker.comment"}
	w := GrantWarnings(mustValidate(t, grantConfig(append(short, "tracker.edit"), short), reg), reg)
	if !slices.ContainsFunc(w, func(s string) bool {
		return strings.HasPrefix(s, "scope web binding agent: its playbook work uses tracker.edit, which the binding's operations don't grant")
	}) {
		t.Fatalf("binding drift: %q", w)
	}
	w = GrantWarnings(mustValidate(t, grantConfig(short, short), reg), reg)
	if !slices.ContainsFunc(w, func(s string) bool {
		return strings.Contains(s, "scope web binding agent") && strings.Contains(s, "tracker.edit") && strings.Contains(s, "authorization.operations")
	}) {
		t.Fatalf("scope drift: %q", w)
	}
	// Negative control: none once it is granted.
	if w := GrantWarnings(mustValidate(t, grantConfig([]string{"@tracker.author"}, []string{"@tracker.author"}), reg), reg); len(w) != 0 {
		t.Fatalf("granted: %q", w)
	}
	// The trail's classification follows the same declaration.
	s := mustValidate(t, grantConfig(short, short), reg)
	if !s.Declared(reg, "web", "agent", "tracker.edit") || s.Declared(reg, "web", "agent", "tracker.delete") {
		t.Fatal("Declared does not follow the playbook")
	}
}

func TestGrantWarningsOfferTheToolset(t *testing.T) {
	reg := toolsetRegistry(2)
	v1 := []string{"read_context", "tracker.view", "tracker.comment", "tracker.delete"}
	w := GrantWarnings(mustValidate(t, grantConfig(v1, []string{"tracker.view"}), reg), reg)
	var offer string
	for _, s := range w {
		if strings.HasPrefix(s, "scope web authorization.operations lists the tracker.author toolset's operations as of its version 1") {
			offer = s
		}
	}
	if offer == "" || !strings.Contains(offer, "lacks tracker.edit") ||
		!strings.Contains(offer, `open-conduit config set /scopes/web/authorization/operations '["tracker.delete","@tracker.author"]'`) {
		t.Fatalf("offer: %q", w)
	}
	// The binding's list names no whole version, so it is left alone.
	if slices.ContainsFunc(w, func(s string) bool { return strings.Contains(s, "binding agent operations lists") }) {
		t.Fatalf("binding offered: %q", w)
	}
	// Negative control: a list that grants the toolset, or denies what
	// it gained, is up to date.
	for _, list := range [][]string{{"@tracker.author"}, append(slices.Clone(v1), "!tracker.edit")} {
		s := mustValidate(t, grantConfig(list, []string{}), reg)
		if slices.ContainsFunc(GrantWarnings(s, reg), func(s string) bool { return strings.Contains(s, "toolset's operations as of") }) {
			t.Fatalf("%v offered the toolset", list)
		}
	}
}
