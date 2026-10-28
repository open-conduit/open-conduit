package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/open-conduit/open-conduit/domain"
)

// folderExecutor records the folder Compose hands it in its config.
type folderExecutor struct{ fakeExecutor }

func (folderExecutor) FolderConfig(c json.RawMessage, folder string, resource bool) (json.RawMessage, error) {
	var m map[string]any
	if err := json.Unmarshal(c, &m); err != nil {
		return nil, err
	}
	if !resource {
		m["dir"] = folder
	}
	return json.Marshal(m)
}

func includeRegistry() Registry {
	reg := registry()
	reg.Executors["folder_agent"] = folderExecutor{fakeExecutor{domain.Capabilities{Toolbelt: "cli"}}}
	return reg
}

// central is a config file with one root scope, "home", and one include.
func central(folder string) []byte {
	return []byte(`{"version":1,"settings":{},"connections":[],
		"includes":[{"path":` + quote(folder) + `,"parent":"home"}],
		"scopes":[{"id":"home","name":"home",
			"authorization":{"connections":[],"operations":["emit","read_context","declare_outcome"],"read_scopes":[]},
			"secret_refs":{"key":"env:KEY"}}]}`)
}

func quote(s string) string { b, _ := json.Marshal(s); return string(b) }

const fragment = `{"version":1,"scopes":[{"id":"web","name":"web",
	"bindings":[{"name":"run","executor":"folder_agent","requires":{"toolbelt":"cli"},"operations":["emit"],"secret_names":["key"],"config":{"command":"x"}}],
	"routes":[{"name":"incoming","handler":"manual","binding":"run","active_policy":"queue","config":{}}]}]}`

func writeFragment(t *testing.T, folder, data string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(folder, ".open-conduit"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(folder, FragmentFile)
	if err := os.WriteFile(path, []byte(data), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

// TestIncludeComposesFolderScopes: a folder's scopes join the generation
// under the include's parent, inheriting its secrets, with their source
// recorded and their bindings adapted to the folder; the stored generation
// validates again on its own.
func TestIncludeComposesFolderScopes(t *testing.T) {
	folder := filepath.Join(t.TempDir(), "web")
	writeFragment(t, folder, fragment, 0o644)
	s, err := ValidateFile(central(folder), includeRegistry(), Includes{})
	if err != nil {
		t.Fatal(err)
	}
	web, ok := s.Scope("web")
	if !ok || web.ParentID != "home" || web.Source != folder || !web.Enabled {
		t.Fatalf("web scope %+v", web)
	}
	if b, _ := web.Binding("run"); !strings.Contains(string(b.Config), quote(folder)) {
		t.Fatalf("binding config not adapted to the folder: %s", b.Config)
	}
	if web.SecretRefs["key"] != "env:KEY" {
		t.Fatalf("the parent's secret was not inherited: %v", web.SecretRefs)
	}
	if !strings.Contains(string(s.Normalized), `"source":`+quote(folder)) || strings.Contains(string(s.Normalized), "includes") {
		t.Fatalf("normalized %s", s.Normalized)
	}
	stored, err := Validate(s.Normalized, includeRegistry())
	if err != nil || stored.Hash != s.Hash {
		t.Fatalf("stored generation did not round-trip: %v", err)
	}
	if home, _ := s.Scope("home"); home.Source != "" {
		t.Fatalf("the file's own scope has a source: %+v", home)
	}
}

// TestIncludePathsResolveAgainstTheConfigDirectory: a relative include is
// beside the config file; one that cannot be resolved is refused.
func TestIncludePathsResolveAgainstTheConfigDirectory(t *testing.T) {
	base := t.TempDir()
	writeFragment(t, filepath.Join(base, "web"), fragment, 0o644)
	if s, err := ValidateFile(central("web"), includeRegistry(), Includes{Base: base}); err != nil {
		t.Fatal(err)
	} else if web, _ := s.Scope("web"); web.Source != filepath.Join(base, "web") {
		t.Fatalf("source %q", web.Source)
	}
	for _, path := range []string{"web", "~/web"} {
		_, err := ValidateFile(central(path), includeRegistry(), Includes{})
		if d := diagnostics(t, err); len(d) != 1 || d[0].Path != "includes/0/path" {
			t.Fatalf("%s: %v", path, d)
		}
	}
}

// TestIncludedScopesStayBeneathTheirParent: a folder cannot carry secret
// references, hang its scopes elsewhere or widen its parent's ceiling, and
// diagnostics point into the include.
func TestIncludedScopesStayBeneathTheirParent(t *testing.T) {
	cases := []struct{ name, fragment, path, code string }{
		{"secret references", `{"version":1,"scopes":[{"id":"web","name":"web","secret_refs":{"key":"cmd:true"}}]}`, "includes/0/scopes/0/secret_refs", "unknown_field"},
		{"source", `{"version":1,"scopes":[{"id":"web","name":"web","source":"/elsewhere"}]}`, "includes/0/scopes/0/source", "unknown_field"},
		{"connections", `{"version":1,"connections":[],"scopes":[]}`, "includes/0/connections", "unknown_field"},
		{"another parent", `{"version":1,"scopes":[{"id":"web","name":"web","parent":"other"}]}`, "includes/0/scopes/0/parent", "invalid_value"},
		{"wider ceiling", `{"version":1,"scopes":[{"id":"web","name":"web","authorization":{"connections":[],"operations":["github.issue.comment"],"read_scopes":[]}}]}`, "includes/0/scopes/0/authorization/operations", ""},
		{"duplicate id", `{"version":1,"scopes":[{"id":"home","name":"again"}]}`, "includes/0/scopes/0/id", "duplicate_id"},
		{"version", `{"version":2,"scopes":[]}`, "includes/0/version", "unsupported_version"},
	}
	for _, tc := range cases {
		folder := filepath.Join(t.TempDir(), "web")
		writeFragment(t, folder, tc.fragment, 0o644)
		_, err := ValidateFile(central(folder), includeRegistry(), Includes{})
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
	folder := filepath.Join(t.TempDir(), "web")
	writeFragment(t, folder, fragment, 0o644)
	data := []byte(strings.Replace(string(central(folder)), `"parent":"home"`, `"parent":"nowhere"`, 1))
	if d := diagnostics(t, func() error { _, err := ValidateFile(data, includeRegistry(), Includes{}); return err }()); !slices.Contains(codes(d), "unknown_scope") {
		t.Fatalf("unknown include parent: %v", d)
	}
}

// TestIncludedFolderFileMustBeSafe: a fragment others can write, or a
// symlink, is refused rather than read.
func TestIncludedFolderFileMustBeSafe(t *testing.T) {
	folder := filepath.Join(t.TempDir(), "web")
	writeFragment(t, folder, fragment, 0o666)
	_, err := ValidateFile(central(folder), includeRegistry(), Includes{})
	if d := diagnostics(t, err); len(d) != 1 || d[0].Code != "include_unreadable" {
		t.Fatalf("writable fragment: %v", d)
	}
	other := filepath.Join(t.TempDir(), "real.json")
	if err := os.WriteFile(other, []byte(fragment), 0o644); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(folder, FragmentFile)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, path); err != nil {
		t.Fatal(err)
	}
	_, err = ValidateFile(central(folder), includeRegistry(), Includes{})
	if d := diagnostics(t, err); len(d) != 1 || d[0].Code != "include_unreadable" {
		t.Fatalf("symlinked fragment: %v", d)
	}
}

// TestDeletedFolderKeepsItsScopesDisabled: once a folder has gone, the
// configuration still validates; the committed generation's scopes from it
// stay, disabled, and come back enabled with the folder.
func TestDeletedFolderKeepsItsScopesDisabled(t *testing.T) {
	folder := filepath.Join(t.TempDir(), "web")
	writeFragment(t, folder, fragment, 0o644)
	data := central(folder)
	first, err := ValidateFile(data, includeRegistry(), Includes{})
	if err != nil {
		t.Fatal(err)
	}
	if w := IncludeWarnings(data, ""); len(w) != 0 {
		t.Fatalf("warnings with the folder present: %v", w)
	}
	if err := os.RemoveAll(folder); err != nil {
		t.Fatal(err)
	}
	gone, err := ValidateFile(data, includeRegistry(), Includes{Previous: first.Normalized})
	if err != nil {
		t.Fatalf("a deleted folder broke the configuration: %v", err)
	}
	web, ok := gone.Scope("web")
	if !ok || web.Enabled || web.Source != folder {
		t.Fatalf("web after deletion %+v", web)
	}
	if home, _ := gone.Scope("home"); !home.Enabled {
		t.Fatal("the parent was disabled")
	}
	if w := IncludeWarnings(data, ""); len(w) != 1 || !strings.Contains(w[0], folder) {
		t.Fatalf("warnings %v", w)
	}
	if again, err := ValidateFile(data, includeRegistry(), Includes{Previous: gone.Normalized}); err != nil {
		t.Fatal(err)
	} else if web, _ := again.Scope("web"); web.Enabled {
		t.Fatal("still gone, but enabled")
	}
	if none, err := ValidateFile(data, includeRegistry(), Includes{}); err != nil {
		t.Fatal(err)
	} else if _, ok := none.Scope("web"); ok {
		t.Fatal("a folder never applied contributed scopes")
	}
	writeFragment(t, folder, fragment, 0o644)
	back, err := ValidateFile(data, includeRegistry(), Includes{Previous: gone.Normalized})
	if err != nil {
		t.Fatal(err)
	}
	if web, _ := back.Scope("web"); !web.Enabled || back.Hash != first.Hash {
		t.Fatalf("folder back: %+v", web)
	}
}

// TestStoredAndFileFormsStayApart: the config file cannot claim a scope's
// source, and a stored generation never carries includes.
func TestStoredAndFileFormsStayApart(t *testing.T) {
	file := []byte(`{"version":1,"settings":{},"connections":[],"scopes":[{"id":"home","name":"home","source":"/x"}]}`)
	if _, err := ValidateFile(file, registry(), Includes{}); !slices.Contains(codes(diagnostics(t, err)), "unknown_field") {
		t.Fatalf("source accepted in the config file: %v", err)
	}
	if _, err := Validate(central("/x"), registry()); !slices.Contains(codes(diagnostics(t, err)), "unknown_field") {
		t.Fatalf("includes accepted without the file: %v", err)
	}
}
