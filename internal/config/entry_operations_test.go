package config

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
)

// entryRegistry registers the "mcp" entry type of the "bridge" extension,
// whose config's tools are the operations it declares, all in toolset
// "all"; a tool named "refused" makes the owner refuse the config.
func entryRegistry() Registry {
	reg := registry()
	reg.Extensions["mcp"] = ExtensionType{Owner: "bridge",
		Validate: func(raw json.RawMessage, _ []string) error {
			if strings.Contains(string(raw), `"refused"`) {
				return fmt.Errorf("refused")
			}
			return nil
		},
		Operations: func(raw json.RawMessage) []extension.EntryOperationDecl {
			var c struct {
				Tools   []string `json:"tools"`
				Summary string   `json:"summary"`
			}
			_ = json.Unmarshal(raw, &c)
			var out []extension.EntryOperationDecl
			for _, t := range c.Tools {
				out = append(out, extension.EntryOperationDecl{Name: t, Summary: c.Summary, Toolset: "all"})
			}
			return out
		}}
	return reg
}

// entryScope is a scope's JSON: its parent, its mcp entries (name to
// config) and one shell binding granted ops.
type entryScope struct {
	id, parent string
	entries    map[string]string
	ops        []string
}

func entryConfig(scopes ...entryScope) []byte {
	var parts []string
	for _, s := range scopes {
		var entries []string
		for _, name := range slices.Sorted(maps.Keys(s.entries)) {
			entries = append(entries, `{"name":"`+name+`","type":"mcp","secret_names":[],"config":`+s.entries[name]+`}`)
		}
		ops, _ := json.Marshal(s.ops)
		parent := ""
		if s.parent != "" {
			parent = `"parent":"` + s.parent + `",`
		}
		auth := `"authorization":{"connections":[],"operations":["read_context","bridge.crm.lookup","bridge.crm.archive","@bridge.crm.all"],"read_scopes":[]},`
		if s.parent != "" {
			auth = ""
		}
		parts = append(parts, `{"id":"`+s.id+`","name":"`+s.id+`",`+parent+auth+`
			"extensions":[`+strings.Join(entries, ",")+`],
			"bindings":[{"name":"agent-`+s.id+`","executor":"shell","requires":{"toolbelt":"cli"},"operations":`+string(ops)+`,"secret_names":[],"config":{}}]}`)
	}
	return []byte(`{"version":1,"settings":{},"connections":[],"scopes":[` + strings.Join(parts, ",") + `]}`)
}

const crm = `{"tools":["lookup","archive"],"summary":"A CRM tool."}`

// An entry's config declares operations, named <owner>.<entry>.<name>:
// a binding is granted them, by name or through the entry's toolset, where
// the entry is in effect (its scope and the descendants that inherit it),
// and the snapshot lists each with those scopes for the toolbelt.
func TestEntryOperationsResolve(t *testing.T) {
	s := mustValidate(t, entryConfig(
		entryScope{id: "org", entries: map[string]string{"crm": crm}, ops: []string{"bridge.crm.lookup"}},
		entryScope{id: "team", parent: "org", ops: []string{"@bridge.crm.all"}},
	), entryRegistry())
	if got := s.Scopes["org"].Bindings[0].Operations; !slices.Equal(got, []string{"bridge.crm.lookup"}) {
		t.Fatalf("org binding operations = %v", got)
	}
	if got := s.Scopes["team"].Bindings[1].Operations; !slices.Equal(got, []string{"bridge.crm.lookup", "bridge.crm.archive"}) {
		t.Fatalf("the inherited entry's toolset granted %v", got)
	}
	want := []extension.EntryOperation{
		{Entry: "crm", EntryOperationDecl: extension.EntryOperationDecl{Name: "archive", Summary: "A CRM tool.", Toolset: "all"}, Scopes: []domain.ID{"org", "team"}},
		{Entry: "crm", EntryOperationDecl: extension.EntryOperationDecl{Name: "lookup", Summary: "A CRM tool.", Toolset: "all"}, Scopes: []domain.ID{"org", "team"}},
	}
	if got := s.EntryOperations["bridge"]; fmt.Sprint(got) != fmt.Sprint(want) || len(s.EntryOperations) != 1 {
		t.Fatalf("snapshot entry operations = %+v", s.EntryOperations)
	}
}

// Only what an entry declares can be granted, and only where the entry is
// in effect; a removed entry takes its operations with it, a toolset of
// operations no entry in the scope declares grants nothing, and two
// entries may not declare one name two ways.
func TestEntryOperationsFailClosed(t *testing.T) {
	for _, c := range []struct {
		name   string
		scopes []entryScope
		code   string
	}{
		{"an operation the entry doesn't declare", []entryScope{{id: "org", entries: map[string]string{"crm": crm}, ops: []string{"bridge.crm.delete"}}}, "unknown_reference"},
		{"the entry removed", []entryScope{{id: "org", ops: []string{"bridge.crm.lookup"}}}, "unknown_reference"},
		{"an entry the owner refuses", []entryScope{{id: "org", entries: map[string]string{"crm": `{"tools":["refused"]}`}, ops: []string{"bridge.crm.refused"}}}, "unknown_reference"},
		{"a scope without the entry", []entryScope{
			{id: "org", ops: []string{"read_context"}},
			{id: "other", entries: map[string]string{"crm": crm}, ops: []string{"read_context"}},
			{id: "team", parent: "org", ops: []string{"bridge.crm.lookup"}},
		}, "operation_not_in_effect"},
		{"two entries declaring a name differently", []entryScope{
			{id: "org", entries: map[string]string{"crm": crm}, ops: []string{"read_context"}},
			{id: "team", parent: "org", entries: map[string]string{"crm": `{"tools":["lookup"],"summary":"Another tool."}`}, ops: []string{"read_context"}},
		}, "operation_conflict"},
		{"a full name longer than the toolbelt takes", []entryScope{{id: "org", entries: map[string]string{strings.Repeat("e", 63): `{"tools":["` + strings.Repeat("t", 63) + `"]}`}, ops: []string{"read_context"}}}, "invalid_operation"},
		{"an operation the toolbelt can't register", []entryScope{{id: "org", entries: map[string]string{"crm": `{"tools":["Look up"]}`}, ops: []string{"read_context"}}}, "invalid_operation"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := Validate(entryConfig(c.scopes...), entryRegistry())
			if err == nil {
				t.Fatal("accepted")
			}
			if got := codes(diagnostics(t, err)); !slices.Contains(got, c.code) {
				t.Fatalf("codes = %v, want %s", got, c.code)
			}
		})
	}
	// A toolset an entry in another scope declares is narrowed away
	// rather than refused, as a toolset is narrowed to the ceiling.
	s := mustValidate(t, entryConfig(
		entryScope{id: "org", ops: []string{"@bridge.crm.all"}},
		entryScope{id: "other", entries: map[string]string{"crm": crm}, ops: []string{"read_context"}},
	), entryRegistry())
	if got := s.Scopes["org"].Bindings[0].Operations; len(got) != 0 {
		t.Fatalf("a toolset of operations not in effect granted %v", got)
	}
	if got := s.EntryOperations["bridge"]; len(got) != 2 || !slices.Equal(got[0].Scopes, []domain.ID{"other"}) {
		t.Fatalf("entry operations registered outside their entry's scope: %+v", got)
	}
}
