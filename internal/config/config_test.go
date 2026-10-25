package config

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/alsiola/open-conduit/domain"
	"github.com/alsiola/open-conduit/extension"
)

type fakeExecutor struct{ caps domain.Capabilities }

func (f fakeExecutor) Capabilities() domain.Capabilities { return f.caps }
func (fakeExecutor) Run(context.Context, extension.RunRequest) (extension.RunHandle, error) {
	return nil, errors.New("unused")
}
func (fakeExecutor) Recover(context.Context, extension.RunRequest, domain.Payload) (extension.RunHandle, error) {
	return nil, extension.ErrNotAdoptable
}

type validatingExecutor struct{ fakeExecutor }

func (validatingExecutor) ValidateConfig(c json.RawMessage, names []string) error {
	if !strings.Contains(string(c), "command") || len(names) != 0 {
		return errors.New("rejected secret-looking value")
	}
	return nil
}

type injectExecutor struct{ fakeExecutor }

func (injectExecutor) Inject(context.Context, domain.ID, domain.Event) error { return nil }

type interruptExecutor struct{ fakeExecutor }

func (interruptExecutor) Interrupt(context.Context, domain.ID) error { return nil }

// registry mirrors the synthetic registrations of scripts/check-config-contract.py.
func registry() Registry {
	return Registry{
		Executors: map[string]extension.Executor{
			"shell":         fakeExecutor{domain.Capabilities{Toolbelt: "cli"}},
			"support_agent": injectExecutor{fakeExecutor{domain.Capabilities{Toolbelt: "mcp", Inject: true}}},
			"coding_agent":  interruptExecutor{fakeExecutor{domain.Capabilities{Toolbelt: "mcp", Interrupt: true, Workspace: true}}},
		},
		Handlers:    map[string]bool{"manual": true, "thread": true, "issue": true},
		Extensions:  map[string]ExtensionType{"chat_ingress": {Ingress: true}, "code_ingress": {Ingress: true}, "schedule": {}},
		Connections: map[string]func(json.RawMessage) error{"chat": nil, "code": nil},
		Resources:   map[string]bool{"git": true},
	}
}

func example(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "docs", "architecture", "examples", "config", name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	return data
}
func generic(t *testing.T, data []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	return m
}
func encode(t *testing.T, v any) []byte {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
func diagnostics(t *testing.T, err error) []Diagnostic {
	t.Helper()
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("expected ValidationError, got %v", err)
	}
	return ve.Diagnostics
}
func codes(diags []Diagnostic) []string {
	var out []string
	for _, d := range diags {
		out = append(out, d.Code)
	}
	return out
}
func scopes(m map[string]any) []any { return m["scopes"].([]any) }
func scope(m map[string]any, i int) map[string]any {
	return scopes(m)[i].(map[string]any)
}
func entry(m map[string]any, i int, category string, j int) map[string]any {
	return scope(m, i)[category].([]any)[j].(map[string]any)
}
func clone(t *testing.T, m map[string]any) map[string]any { return generic(t, encode(t, m)) }

func TestExamplesValidate(t *testing.T) {
	for _, name := range []string{"workspace-free", "git-scope", "shell-job", "inheritance"} {
		if _, err := Validate(example(t, name), registry()); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

func TestInvalidCapabilityDiagnostic(t *testing.T) {
	_, err := Validate(example(t, "invalid-capability"), registry())
	diags := diagnostics(t, err)
	want := "scopes/0/bindings/0/requires/inject: unsupported_capability: executor shell has no inject; select an inject executor or remove the requirement and set route unsupported_fallback=queue"
	if len(diags) != 1 || diags[0].String() != want {
		t.Fatalf("diagnostics %v", diags)
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatal(err)
	}
}

func TestInheritance(t *testing.T) {
	snap, err := Validate(example(t, "inheritance"), registry())
	if err != nil {
		t.Fatal(err)
	}
	child, ok := snap.Scope("child")
	if !ok {
		t.Fatal("child missing")
	}
	var routes []string
	for _, r := range child.Routes {
		routes = append(routes, r.Name)
	}
	if !slices.Equal(routes, []string{"incoming", "second"}) || child.Routes[0].ActivePolicy != "inject" || child.Routes[0].UnsupportedFallback != "queue" {
		t.Fatalf("routes %+v", child.Routes)
	}
	if string(child.Bindings[0].Config) != `{"command":"printf hello"}` {
		t.Fatalf("binding config %s", child.Bindings[0].Config)
	}
	if _, ok := child.SecretRefs["temporary"]; ok || child.SecretRefs["retained"] != "env:RETAINED" {
		t.Fatalf("secret refs %v", child.SecretRefs)
	}
	if child.ParentID != "parent" || !child.Enabled || child.Concurrency != 4 || child.AdmissionLimit != 10 {
		t.Fatalf("child %+v", child)
	}
	if !slices.Equal(child.Authorization.Operations, []string{"emit", "read_context", "declare_outcome"}) {
		t.Fatalf("inherited authorization %+v", child.Authorization)
	}
	if !slices.Equal(snap.Order, []domain.ID{"parent", "child"}) || snap.Settings.TimeoutSeconds != 3600 || !slices.Equal(snap.Settings.RetryDelaysSeconds, []int{300, 600}) {
		t.Fatalf("snapshot %+v", snap)
	}
}

func TestValidCommandReference(t *testing.T) {
	m := generic(t, example(t, "inheritance"))
	scope(m, 0)["secret_refs"].(map[string]any)["temporary"] = "cmd:printf token"
	if _, err := Validate(encode(t, m), registry()); err != nil {
		t.Fatal(err)
	}
}

func TestMutations(t *testing.T) {
	base := generic(t, example(t, "inheritance"))
	secret := func(v string) func(map[string]any) {
		return func(m map[string]any) { scope(m, 0)["secret_refs"].(map[string]any)["temporary"] = v }
	}
	cases := []struct {
		code   string
		mutate func(map[string]any)
	}{
		{"unknown_field", func(m map[string]any) { m["unrecognized"] = true }},
		{"invalid_value", func(m map[string]any) { m["settings"].(map[string]any)["causal_max_depth"] = 0 }},
		{"invalid_value", func(m map[string]any) { entry(m, 0, "routes", 0)["active_policy"] = "coalesce" }},
		{"invalid_value", secret("literal-secret")},
		{"invalid_value", secret("cmd:printf token\n")},
		{"invalid_value", secret("cmd:printf\rtoken")},
		{"scope_cycle", func(m map[string]any) { scope(m, 0)["parent"] = "child" }},
		{"unknown_scope", func(m map[string]any) { scope(m, 1)["parent"] = "absent" }},
		{"duplicate_name", func(m map[string]any) {
			s := scope(m, 0)
			s["routes"] = append(s["routes"].([]any), clone(t, entry(m, 0, "routes", 0)))
		}},
		{"authority_widening", func(m map[string]any) {
			scope(m, 1)["authorization"] = map[string]any{"connections": []any{}, "operations": []any{"emit", "wait"}, "read_scopes": []any{}}
		}},
		{"limit_widening", func(m map[string]any) { scope(m, 1)["limits"] = map[string]any{"concurrency": 5} }},
		{"missing_binding", func(m map[string]any) {
			s := scope(m, 1)
			s["bindings"] = append(s["bindings"].([]any), map[string]any{"name": "run", "disabled": true})
		}},
		{"unsupported_policy", func(m map[string]any) { delete(entry(m, 1, "routes", 0), "unsupported_fallback") }},
		{"unknown_secret", func(m map[string]any) { entry(m, 1, "bindings", 0)["secret_names"] = []any{"temporary"} }},
		{"connection_denied", func(m map[string]any) { entry(m, 1, "bindings", 0)["connection"] = "ungranted" }},
		{"resource_required", func(m map[string]any) { entry(m, 1, "bindings", 0)["requires"] = map[string]any{"workspace": true} }},
		{"unknown_disabled_entry", func(m map[string]any) {
			s := scope(m, 1)
			s["routes"] = append(s["routes"].([]any), map[string]any{"name": "missing", "disabled": true})
		}},
	}
	if len(cases) != 17 {
		t.Fatal("all 17 oracle mutations must be ported")
	}
	for i, c := range cases {
		m := clone(t, base)
		c.mutate(m)
		_, err := Validate(encode(t, m), registry())
		if got := codes(diagnostics(t, err)); !slices.Contains(got, c.code) {
			t.Fatalf("mutation %d: want %s, got %v", i, c.code, got)
		}
		if strings.Contains(err.Error(), "printf") || strings.Contains(err.Error(), "literal-secret") {
			t.Fatalf("mutation %d echoed a value: %v", i, err)
		}
	}
}

func TestDiagnosticPaths(t *testing.T) {
	m := generic(t, example(t, "inheritance"))
	delete(entry(m, 1, "routes", 0), "unsupported_fallback")
	entry(m, 1, "bindings", 0)["secret_names"] = []any{"temporary"}
	scope(m, 1)["limits"] = map[string]any{"concurrency": 5}
	_, err := Validate(encode(t, m), registry())
	got := diagnostics(t, err)
	want := []string{"scopes/1/bindings/0/secret_names: unknown_secret", "scopes/1/limits/concurrency: limit_widening", "scopes/1/routes/0/active_policy: unsupported_policy"}
	if len(got) != len(want) {
		t.Fatalf("diagnostics %v", got)
	}
	for i, d := range got {
		if d.Path+": "+d.Code != want[i] {
			t.Fatalf("diagnostics %v", got)
		}
	}
	// Inherited entries revalidated in a descendant are named by entry name.
	m = generic(t, example(t, "inheritance"))
	s := scope(m, 1)
	s["bindings"] = []any{map[string]any{"name": "run", "disabled": true}}
	s["routes"] = []any{}
	_, err = Validate(encode(t, m), registry())
	got = diagnostics(t, err)
	if len(got) != 2 || got[0].Path != "scopes/1/routes/incoming/binding" || got[1].Path != "scopes/1/routes/second/binding" {
		t.Fatalf("diagnostics %v", got)
	}
	// Repeated validation yields identical, sorted diagnostics.
	_, again := Validate(encode(t, m), registry())
	if err.Error() != again.Error() {
		t.Fatal("nondeterministic diagnostics")
	}
}

func TestIndependentDiagnostics(t *testing.T) {
	m := generic(t, example(t, "git-scope"))
	m["connections"] = append(m["connections"].([]any), map[string]any{"id": "code", "type": "unknown", "config": map[string]any{}})
	entry(m, 0, "bindings", 0)["executor"] = "absent"
	entry(m, 0, "routes", 0)["handler"] = "absent"
	entry(m, 0, "extensions", 0)["config"].(map[string]any)["scope"] = "other"
	entry(m, 0, "extensions", 0)["type"] = "chat_ingress"
	scope(m, 0)["authorization"].(map[string]any)["read_scopes"] = []any{"absent"}
	entry(m, 0, "bindings", 0)["resource"].(map[string]any)["provider"] = "svn"
	_, err := Validate(encode(t, m), registry())
	got := diagnostics(t, err)
	want := []string{
		"connections/1/id: duplicate_id", "connections/1/type: unknown_registration",
		"scopes/0/authorization/read_scopes: unknown_reference",
		"scopes/0/bindings/0/executor: unknown_registration", "scopes/0/bindings/0/resource/provider: unknown_registration",
		"scopes/0/extensions/0/config/scope: ingress_target_mismatch", "scopes/0/routes/0/handler: unknown_registration",
	}
	if len(got) != len(want) {
		t.Fatalf("diagnostics %v", got)
	}
	for i, d := range got {
		if d.Path+": "+d.Code != want[i] {
			t.Fatalf("diagnostics %v", got)
		}
	}
}

func TestOwnerValidationAndRegistrations(t *testing.T) {
	reg := registry()
	reg.Extensions["schedule"] = ExtensionType{Validate: func(c json.RawMessage, names []string) error {
		if !strings.Contains(string(c), "every_seconds") {
			return errors.New("bad")
		}
		return nil
	}}
	reg.Connections["chat"] = func(json.RawMessage) error { return errors.New("bad") }
	m := generic(t, example(t, "shell-job"))
	delete(entry(m, 0, "extensions", 0)["config"].(map[string]any), "every_seconds")
	scope(m, 0)["authorization"].(map[string]any)["operations"] = []any{"emit", "read_context", "declare_outcome", "custom.op"}
	_, err := Validate(encode(t, m), reg)
	if got := diagnostics(t, err); len(got) != 2 || got[0].Path != "scopes/0/authorization/operations" || got[1].Path != "scopes/0/extensions/0/config" {
		t.Fatalf("diagnostics %v", got)
	}
	reg.Operations = []string{"custom.op"}
	entry(m, 0, "extensions", 0)["config"].(map[string]any)["every_seconds"] = 60
	if _, err := Validate(encode(t, m), reg); err != nil {
		t.Fatal(err)
	}
	if _, err := Validate(example(t, "workspace-free"), reg); err == nil || diagnostics(t, err)[0].Code != "invalid_config" {
		t.Fatalf("connection owner validation: %v", err)
	}
	// An executor owner rejects its binding config without the error text.
	reg = registry()
	reg.Executors["shell"] = validatingExecutor{fakeExecutor{domain.Capabilities{Toolbelt: "cli"}}}
	if _, err := Validate(example(t, "shell-job"), reg); err != nil {
		t.Fatal(err)
	}
	m = generic(t, example(t, "shell-job"))
	entry(m, 0, "bindings", 0)["config"] = map[string]any{"cmd": "x"}
	_, err = Validate(encode(t, m), reg)
	if got := diagnostics(t, err); len(got) != 1 || got[0].Path != "scopes/0/bindings/0/config" || got[0].Code != "invalid_config" || strings.Contains(got[0].Message, "secret") {
		t.Fatalf("executor config diagnostics %v", got)
	}
	// An executor advertising inject without implementing it is rejected.
	reg = registry()
	reg.Executors["support_agent"] = fakeExecutor{domain.Capabilities{Toolbelt: "mcp", Inject: true}}
	_, err = Validate(example(t, "workspace-free"), reg)
	if got := diagnostics(t, err); len(got) != 1 || got[0].Code != "invalid_registration" {
		t.Fatalf("diagnostics %v", got)
	}
}

func TestWholeBindingReplacement(t *testing.T) {
	m := generic(t, example(t, "inheritance"))
	entry(m, 0, "bindings", 0)["config"].(map[string]any)["parent_only"] = true
	snap, err := Validate(encode(t, m), registry())
	if err != nil {
		t.Fatal(err)
	}
	child, _ := snap.Scope("child")
	if strings.Contains(string(child.Bindings[0].Config), "parent_only") {
		t.Fatalf("ancestor config retained: %s", child.Bindings[0].Config)
	}
	parent, _ := snap.Scope("parent")
	if !strings.Contains(string(parent.Bindings[0].Config), "parent_only") {
		t.Fatal("parent config lost")
	}
}

func TestTombstoneSlotRestoration(t *testing.T) {
	base := generic(t, example(t, "inheritance"))
	m := clone(t, base)
	scope(m, 1)["routes"] = []any{map[string]any{"name": "incoming", "disabled": true}}
	m["scopes"] = append(scopes(m), map[string]any{"id": "leaf", "name": "leaf", "parent": "child", "routes": []any{clone(t, entry(base, 0, "routes", 0))}})
	snap, err := Validate(encode(t, m), registry())
	if err != nil {
		t.Fatal(err)
	}
	names := func(id domain.ID) []string {
		var out []string
		s, _ := snap.Scope(id)
		for _, r := range s.Routes {
			out = append(out, r.Name)
		}
		return out
	}
	if !slices.Equal(names("child"), []string{"second"}) || !slices.Equal(names("leaf"), []string{"incoming", "second"}) {
		t.Fatalf("child %v leaf %v", names("child"), names("leaf"))
	}
	// A false secret overlay can be restored by a descendant.
	m = clone(t, base)
	m["scopes"] = append(scopes(m), map[string]any{"id": "leaf", "name": "leaf", "parent": "child", "secret_refs": map[string]any{"temporary": "env:RESTORED"}})
	snap, err = Validate(encode(t, m), registry())
	if err != nil {
		t.Fatal(err)
	}
	if leaf, _ := snap.Scope("leaf"); leaf.SecretRefs["temporary"] != "env:RESTORED" {
		t.Fatalf("leaf refs %v", leaf.SecretRefs)
	}
}

func TestStrictParse(t *testing.T) {
	base := string(example(t, "shell-job"))
	cases := map[string]struct{ doc, path, code string }{
		"duplicate top":     {`{"version":1,"version":2}`, "version", "duplicate_key"},
		"duplicate nested":  {strings.Replace(base, `"command": "printf hello"`, `"command": "a", "command": "b"`, 1), "scopes/0/bindings/0/config/command", "duplicate_key"},
		"null":              {strings.Replace(base, `"settings": {}`, `"settings": {"concurrency": null}`, 1), "settings/concurrency", "null_value"},
		"null opaque":       {strings.Replace(base, `"command": "printf hello"`, `"command": null`, 1), "scopes/0/bindings/0/config/command", "null_value"},
		"unknown field":     {strings.Replace(base, `"name": "maintenance",`, `"name": "maintenance", "extra": 1,`, 1), "scopes/0/extra", "unknown_field"},
		"version":           {strings.Replace(base, `"version": 1`, `"version": 2`, 1), "version", "unsupported_version"},
		"bad type":          {strings.Replace(base, `"settings": {}`, `"settings": {"concurrency": "4"}`, 1), "settings/concurrency", "invalid_type"},
		"fraction":          {strings.Replace(base, `"settings": {}`, `"settings": {"concurrency": 1.5}`, 1), "settings/concurrency", "invalid_type"},
		"missing":           {strings.Replace(base, `"connections": [],`, ``, 1), "connections", "missing_field"},
		"tombstone false":   {strings.Replace(base, `"name": "nightly",`, `"name": "nightly", "disabled": false,`, 1), "scopes/0/extensions/0/disabled", "invalid_value"},
		"pointer escaping":  {strings.Replace(base, `"command": "printf hello"`, `"a/b~c": 1, "a/b~c": 2`, 1), "scopes/0/bindings/0/config/a~1b~0c", "duplicate_key"},
		"duplicate list":    {strings.Replace(base, `"secret_names": [],`, `"secret_names": ["a", "a"],`, 1), "scopes/0/bindings/0/secret_names/1", "duplicate_value"},
		"empty scope name":  {strings.Replace(base, `"name": "maintenance"`, `"name": ""`, 1), "scopes/0/name", "invalid_value"},
		"invalid json":      {`{"version":1,`, "", "invalid_json"},
		"trailing":          {base + "{}", "", "invalid_json"},
		"too large":         {strings.Repeat(" ", MaxSize+1), "", "too_large"},
		"empty scopes list": {strings.Replace(base, `"scopes": [`, `"scopes": [], "x": [`, 1), "scopes", "invalid_value"},
	}
	for name, c := range cases {
		_, err := Parse([]byte(c.doc))
		found := false
		for _, d := range diagnostics(t, err) {
			found = found || d.Path == c.path && d.Code == c.code
		}
		if !found {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

func TestHash(t *testing.T) {
	data := example(t, "inheritance")
	a, err := Validate(data, registry())
	if err != nil {
		t.Fatal(err)
	}
	// Reordered keys and whitespace hash identically; defaults are materialized.
	reordered := encode(t, generic(t, data))
	b, _ := Validate(reordered, registry())
	explicit := strings.Replace(string(data), `"settings": {}`, `"settings": {"concurrency": 4, "retry_delays_seconds": [300, 600]}`, 1)
	c, _ := Validate([]byte(explicit), registry())
	if a.Hash != b.Hash || a.Hash != c.Hash || !strings.HasPrefix(a.Hash, "sha256:") || string(a.Normalized) != string(c.Normalized) {
		t.Fatalf("hashes %s %s %s", a.Hash, b.Hash, c.Hash)
	}
	changed, _ := Validate([]byte(strings.Replace(string(data), "printf hello", "printf other", 1)), registry())
	if changed.Hash == a.Hash {
		t.Fatal("content change kept hash")
	}
	if !strings.Contains(string(a.Normalized), `"enabled":true`) || !strings.Contains(string(a.Normalized), `"kept_executions":20`) {
		t.Fatalf("defaults missing: %s", a.Normalized)
	}
}

func TestRedacted(t *testing.T) {
	refs := []string{"cmd:printf token", "env:RETAINED", "keyring:chat_token", "file:code_token", "env:SUPPORT_KEY"}
	m := generic(t, example(t, "workspace-free"))
	m["connections"] = append(m["connections"].([]any), map[string]any{"id": "code", "type": "code", "config": map[string]any{}, "secret_refs": map[string]any{"token": "file:code_token"}})
	scope(m, 0)["secret_refs"].(map[string]any)["temporary"] = "cmd:printf token"
	m["scopes"] = append(scopes(m), map[string]any{"id": "child", "name": "child", "parent": "support", "extensions": []any{map[string]any{"name": "threads", "disabled": true}},
		"secret_refs": map[string]any{"temporary": false, "retained": "env:RETAINED"}})
	snap, err := Validate(encode(t, m), registry())
	if err != nil {
		t.Fatal(err)
	}
	red := string(snap.Redacted())
	for _, s := range append(refs, "printf", "env:", "cmd:", "keyring:", "file:") {
		if strings.Contains(red, s) {
			t.Fatalf("redacted output contains %q: %s", s, red)
		}
	}
	for _, s := range []string{`"temporary":false`, `"temporary":"[redacted]"`, `"token":"[redacted]"`, `"support_key":"[redacted]"`} {
		if !strings.Contains(red, s) {
			t.Fatalf("redacted output lacks %s: %s", s, red)
		}
	}
	if !strings.Contains(string(snap.Normalized), "cmd:printf token") {
		t.Fatal("normalized lost reference")
	}
	for _, d := range snap.DomainScopes() {
		if strings.Contains(string(d.Config.Data), "printf") || strings.Contains(string(d.Config.Data), "env:") {
			t.Fatalf("scope config leaks references: %s", d.Config.Data)
		}
	}
}

func TestDomainScopes(t *testing.T) {
	snap, err := Validate(example(t, "git-scope"), registry())
	if err != nil {
		t.Fatal(err)
	}
	ds := snap.DomainScopes()
	if len(ds) != 1 {
		t.Fatal(ds)
	}
	d := ds[0]
	if d.ID != "engineering" || d.Name != "engineering" || d.ParentID != "" || len(d.Bindings) != 1 || len(d.Routes) != 1 {
		t.Fatalf("%+v", d)
	}
	b := d.Bindings[0]
	if b.Name != "run" || b.Executor != "coding_agent" || !b.Requires.Workspace || !b.Requires.Interrupt || b.Requires.Toolbelt != "mcp" ||
		b.Config.Schema != BindingSchema || string(b.Config.Data) != `{"playbook":"work-issue"}` {
		t.Fatalf("%+v", b)
	}
	r := d.Routes[0]
	if r.Name != "incoming" || r.Handler != "issue" || r.Binding != "run" || r.ActivePolicy != "interrupt" || r.Config.Schema != RouteSchema {
		t.Fatalf("%+v", r)
	}
	var meta ScopeMetadata
	if d.Config.Schema != ScopeSchema || json.Unmarshal(d.Config.Data, &meta) != nil {
		t.Fatalf("config %+v", d.Config)
	}
	if !meta.Enabled || meta.Limits.Concurrency != 4 || meta.Limits.AdmissionLimit != 10 || !slices.Equal(meta.Authorization.Connections, []string{"code"}) ||
		meta.Bindings["run"].Connection != "code" || meta.Bindings["run"].Resource.Provider != "git" || len(meta.Extensions) != 1 || meta.Extensions[0].Type != "code_ingress" {
		t.Fatalf("meta %+v", meta)
	}
	if len(d.SecretRefs) != 0 {
		t.Fatalf("refs %v", d.SecretRefs)
	}
}

func TestAuthorize(t *testing.T) {
	m := generic(t, example(t, "inheritance"))
	scope(m, 0)["authorization"].(map[string]any)["read_scopes"] = []any{"child"}
	m["scopes"] = append(scopes(m),
		map[string]any{"id": "narrow", "name": "narrow", "parent": "parent", "authorization": map[string]any{"connections": []any{}, "operations": []any{"emit"}, "read_scopes": []any{}},
			"bindings": []any{map[string]any{"name": "run", "executor": "shell", "requires": map[string]any{}, "operations": []any{"emit"}, "secret_names": []any{}, "config": map[string]any{}}},
			"routes":   []any{map[string]any{"name": "incoming", "disabled": true}, map[string]any{"name": "second", "disabled": true}}},
		map[string]any{"id": "off", "name": "off", "parent": "parent", "enabled": false},
		map[string]any{"id": "gone", "name": "gone", "parent": "parent", "bindings": []any{map[string]any{"name": "run", "disabled": true}},
			"routes": []any{map[string]any{"name": "incoming", "disabled": true}, map[string]any{"name": "second", "disabled": true}}})
	snap, err := Validate(encode(t, m), registry())
	if err != nil {
		t.Fatal(err)
	}
	if err := snap.AuthorizeOperation("parent", "run", "declare_outcome"); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		scope      domain.ID
		binding, o string
	}{{"narrow", "run", "declare_outcome"}, {"off", "run", "emit"}, {"gone", "run", "emit"}, {"absent", "run", "emit"}, {"parent", "absent", "emit"}, {"parent", "run", "wait"}} {
		if err := snap.AuthorizeOperation(c.scope, c.binding, c.o); !errors.Is(err, ErrDenied) {
			t.Fatalf("%+v: %v", c, err)
		}
	}
	if snap.AuthorizeOperation("narrow", "run", "emit") != nil {
		t.Fatal("narrowed grant denied")
	}
	if snap.AuthorizeRead("parent", "child") != nil || snap.AuthorizeRead("child", "child") != nil {
		t.Fatal("read denied")
	}
	if !errors.Is(snap.AuthorizeRead("child", "parent"), ErrDenied) || !errors.Is(snap.AuthorizeRead("off", "off"), ErrDenied) || !errors.Is(snap.AuthorizeRead("parent", "absent"), ErrDenied) {
		t.Fatal("read widened")
	}
}
