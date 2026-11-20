package config

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

// delegated is a config file whose scope "teams" takes the submitted
// include "web".
func delegated(parent string) []byte {
	return []byte(`{"version":1,"settings":{},"connections":[],
		"includes":[{"name":"web","parent":` + quote(parent) + `}],
		"scopes":[{"id":"home","name":"home",
			"authorization":{"connections":[],"operations":["emit","read_context","declare_outcome"],"read_scopes":[]},
			"secret_refs":{"key":"env:KEY"}},
			{"id":"teams","name":"teams","parent":"home"}]}`)
}

func mustFile(t *testing.T, data, previous []byte) *Snapshot {
	t.Helper()
	s, err := ValidateFile(data, includeRegistry(), Includes{Previous: previous})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// TestSubmittedIncludeTakesAFragment: a declared include starts empty; a
// fragment applied to it joins the generation beneath its parent with its
// source recorded and its bindings as given; the stored generation is
// self-contained, and applying the config file again keeps the fragment.
func TestSubmittedIncludeTakesAFragment(t *testing.T) {
	root := mustFile(t, delegated("teams"), nil)
	if in, ok := root.Include("web"); !ok || in.Parent != "teams" {
		t.Fatalf("includes %+v", root.Includes)
	}
	empty, err := FragmentOf(root.Normalized, "web")
	if err != nil || empty.Parent != "teams" || !strings.Contains(string(empty.Text), `"scopes": []`) {
		t.Fatalf("empty fragment %+v %v", empty, err)
	}
	applied, err := ApplyFragment(root.Normalized, "web", []byte(fragment), includeRegistry())
	if err != nil {
		t.Fatal(err)
	}
	web, ok := applied.Scope("web")
	if !ok || web.ParentID != "teams" || web.Source != SubmittedSource("web") || web.SecretRefs["key"] != "env:KEY" {
		t.Fatalf("web %+v", web)
	}
	if b, _ := web.Binding("run"); strings.Contains(string(b.Config), "dir") {
		t.Fatalf("a submitted binding was adapted to a folder: %s", b.Config)
	}
	if stored, err := Validate(applied.Normalized, includeRegistry()); err != nil || stored.Hash != applied.Hash {
		t.Fatalf("stored generation did not round-trip: %v", err)
	}
	now, err := FragmentOf(applied.Normalized, "web")
	if err != nil || now.Hash == empty.Hash {
		t.Fatalf("hash did not move: %+v %v", now, err)
	}
	// The fragment as read applies back to the same generation.
	if again, err := ApplyFragment(applied.Normalized, "web", now.Text, includeRegistry()); err != nil || again.Hash != applied.Hash {
		t.Fatalf("read fragment did not apply back unchanged: %v", err)
	}
	// The config file, applied again, carries the fragment over unchanged.
	if file := mustFile(t, delegated("teams"), applied.Normalized); file.Hash != applied.Hash {
		t.Fatal("applying the config file again changed the generation")
	}
	// Moving the include moves its top scopes.
	if moved, _ := mustFile(t, delegated("home"), applied.Normalized).Scope("web"); moved.ParentID != "home" {
		t.Fatalf("moved web %+v", moved)
	}
	// Removing the include drops them.
	gone := mustFile(t, []byte(strings.Replace(string(delegated("teams")), `{"name":"web","parent":"teams"}`, "", 1)), applied.Normalized)
	if _, ok := gone.Scope("web"); ok || len(gone.Includes) != 0 {
		t.Fatal("removed include kept its scopes")
	}
	if _, err := ApplyFragment(gone.Normalized, "web", []byte(fragment), includeRegistry()); !errors.Is(err, ErrUnknownInclude) {
		t.Fatalf("applied to an include that is gone: %v", err)
	}
}

// TestSubmittedFragmentDiagnosticsNameTheirSide: a fragment holds what a
// folder may, and a problem in it is at fragment/...; one it causes
// elsewhere is at config/....
func TestSubmittedFragmentDiagnosticsNameTheirSide(t *testing.T) {
	root := mustFile(t, delegated("teams"), nil)
	cases := []struct{ name, fragment, path, code string }{
		{"secret references", `{"version":1,"scopes":[{"id":"web","name":"web","secret_refs":{"key":"cmd:true"}}]}`, "fragment/scopes/0/secret_refs", "unknown_field"},
		{"source", `{"version":1,"scopes":[{"id":"web","name":"web","source":"/x"}]}`, "fragment/scopes/0/source", "unknown_field"},
		{"connections", `{"version":1,"connections":[],"scopes":[]}`, "fragment/connections", "unknown_field"},
		{"another parent", `{"version":1,"scopes":[{"id":"web","name":"web","parent":"home"}]}`, "fragment/scopes/0/parent", "invalid_value"},
		{"wider ceiling", `{"version":1,"scopes":[{"id":"web","name":"web","authorization":{"connections":[],"operations":["github.issue.comment"],"read_scopes":[]}}]}`, "fragment/scopes/0/authorization/operations", ""},
		{"duplicate id", `{"version":1,"scopes":[{"id":"home","name":"again"}]}`, "fragment/scopes/0/id", "duplicate_id"},
		{"not json", `{`, "fragment", ""},
	}
	for _, tc := range cases {
		_, err := ApplyFragment(root.Normalized, "web", []byte(tc.fragment), includeRegistry())
		found := false
		for _, d := range diagnostics(t, err) {
			if strings.HasPrefix(d.Path, tc.path) && (tc.code == "" || d.Code == tc.code) {
				found = true
			}
		}
		if !found {
			t.Fatalf("%s: diagnostics %v", tc.name, diagnostics(t, err))
		}
	}
	// A budget in the committed configuration naming a scope the fragment
	// drops is the configuration's to resolve.
	withBudget := strings.Replace(string(delegated("teams")), `"settings":{}`, `"settings":{"budgets":[{"unit":"usd","per":"scope","max":5,"warn_at":0.8,"scope":"web"}]}`, 1)
	if _, err := ValidateFile([]byte(withBudget), includeRegistry(), Includes{}); err == nil {
		t.Fatal("a budget on a scope that doesn't exist validated")
	}
	applied, err := ApplyFragment(root.Normalized, "web", []byte(fragment), includeRegistry())
	if err != nil {
		t.Fatal(err)
	}
	budgeted := mustFile(t, []byte(withBudget), applied.Normalized)
	_, err = ApplyFragment(budgeted.Normalized, "web", []byte(`{"version":1,"scopes":[]}`), includeRegistry())
	if d := diagnostics(t, err); len(d) != 1 || !strings.HasPrefix(d[0].Path, "config/settings/budgets/0") {
		t.Fatalf("diagnostics %v", d)
	}
}

// TestSubmittedIncludesAreDeclaredOnce: an include is a folder or
// submitted, named once, under a scope of the config file; a stored
// scope's submitted include must still be declared.
func TestSubmittedIncludesAreDeclaredOnce(t *testing.T) {
	both := strings.Replace(string(delegated("teams")), `{"name":"web",`, `{"name":"web","path":"/x",`, 1)
	if !slices.Contains(codes(diagnostics(t, func() error { _, err := ValidateFile([]byte(both), includeRegistry(), Includes{}); return err }())), "invalid_value") {
		t.Fatal("an include with both a path and a name validated")
	}
	twice := strings.Replace(string(delegated("teams")), `{"name":"web","parent":"teams"}`, `{"name":"web","parent":"teams"},{"name":"web","parent":"home"}`, 1)
	if !slices.Contains(codes(diagnostics(t, func() error { _, err := ValidateFile([]byte(twice), includeRegistry(), Includes{}); return err }())), "duplicate_value") {
		t.Fatal("an include declared twice validated")
	}
	if !slices.Contains(codes(diagnostics(t, func() error { _, err := ValidateFile(delegated("nowhere"), includeRegistry(), Includes{}); return err }())), "unknown_scope") {
		t.Fatal("an include under an unknown scope validated")
	}
	applied, err := ApplyFragment(mustFile(t, delegated("teams"), nil).Normalized, "web", []byte(fragment), includeRegistry())
	if err != nil {
		t.Fatal(err)
	}
	orphan := strings.Replace(string(applied.Normalized), `"includes":[{"name":"web","parent":"teams"}],`, "", 1)
	if orphan == string(applied.Normalized) {
		t.Fatalf("normalized lacks its includes: %s", applied.Normalized)
	}
	if !slices.Contains(codes(diagnostics(t, func() error { _, err := Validate([]byte(orphan), includeRegistry()); return err }())), "unknown_include") {
		t.Fatal("a scope from an undeclared include validated")
	}
}

// TestExportKeepsSubmittedIncludes: export writes a submitted include as
// its name and parent, not its scopes, and the export composes back to the
// same generation.
func TestExportKeepsSubmittedIncludes(t *testing.T) {
	applied, err := ApplyFragment(mustFile(t, delegated("teams"), nil).Normalized, "web", []byte(fragment), includeRegistry())
	if err != nil {
		t.Fatal(err)
	}
	text, err := ExportFile(applied.Normalized)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(text), `"name": "web"`) || strings.Contains(string(text), "submitted:") {
		t.Fatalf("export %s", text)
	}
	if back := mustFile(t, text, applied.Normalized); back.Hash != applied.Hash {
		t.Fatalf("export did not compose back: %s", text)
	}
}
