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

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
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
			"coding_agent":  interruptExecutor{fakeExecutor{domain.Capabilities{Toolbelt: "mcp", Interrupt: true, Workspace: true, Sandbox: true}}},
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
	if !slices.Equal(snap.Order, []domain.ID{"parent", "child"}) || snap.Settings.TimeoutSeconds != 3600 || snap.Settings.JobConcurrency != 4 || !slices.Equal(snap.Settings.RetryDelaysSeconds, []int{300, 600}) {
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

// TestHandlerBinding: a route handler's binding check sees the binding the
// route binds and refuses it with its own reason.
func TestHandlerBinding(t *testing.T) {
	reg := registry()
	var seen []extension.RouteBinding
	reg.HandlerBinding = map[string]func(extension.RouteBinding) error{"manual": func(b extension.RouteBinding) error {
		seen = append(seen, b)
		return errors.New("bind the agent executor")
	}}
	_, err := Validate(example(t, "shell-job"), reg)
	got := diagnostics(t, err)
	if len(got) != 1 || got[0].Code != "binding_refused" || !strings.HasSuffix(got[0].Path, "/binding") || !strings.Contains(got[0].Message, "route handler manual refuses this binding: bind the agent executor") {
		t.Fatalf("diagnostics %v", got)
	}
	if len(seen) != 1 || seen[0].Executor != "shell" || len(seen[0].Config) == 0 {
		t.Fatalf("seen %+v", seen)
	}
	reg.HandlerBinding["manual"] = func(extension.RouteBinding) error { return nil }
	if _, err = Validate(example(t, "shell-job"), reg); err != nil {
		t.Fatal(err)
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
	if !strings.Contains(string(a.Normalized), `"enabled":true`) || !strings.Contains(string(a.Normalized), `"kept_executions":20`) || !strings.Contains(string(a.Normalized), `"job_concurrency":4`) {
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
	scope(m, 0)["authorization"].(map[string]any)["file_scopes"] = []any{"narrow"}
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
	// Filing is its own grant: read_scopes doesn't imply it, nor it reading.
	if snap.AuthorizeFile("parent", "narrow") != nil || snap.AuthorizeFile("child", "child") != nil {
		t.Fatal("file denied")
	}
	if !errors.Is(snap.AuthorizeFile("parent", "child"), ErrDenied) || !errors.Is(snap.AuthorizeRead("parent", "narrow"), ErrDenied) || !errors.Is(snap.AuthorizeFile("narrow", "parent"), ErrDenied) {
		t.Fatal("file widened")
	}
}

// file_scopes is optional, must name defined scopes, and a child's may only
// narrow its parent's.
func TestFileScopesValidation(t *testing.T) {
	m := generic(t, example(t, "inheritance"))
	scope(m, 0)["authorization"].(map[string]any)["file_scopes"] = []any{"absent"}
	_, err := Validate(encode(t, m), registry())
	if got := diagnostics(t, err); len(got) != 1 || got[0].Path+": "+got[0].Code != "scopes/0/authorization/file_scopes: unknown_reference" {
		t.Fatalf("diagnostics %v", got)
	}
	m = generic(t, example(t, "inheritance"))
	m["scopes"] = append(scopes(m), map[string]any{"id": "wide", "name": "wide", "parent": "parent",
		"authorization": map[string]any{"connections": []any{}, "operations": []any{}, "read_scopes": []any{}, "file_scopes": []any{"parent"}}})
	_, err = Validate(encode(t, m), registry())
	got := diagnostics(t, err)
	if !slices.ContainsFunc(got, func(d Diagnostic) bool {
		return d.Path == "scopes/2/authorization/file_scopes" && d.Code == "authority_widening"
	}) {
		t.Fatalf("diagnostics %v", got)
	}
}

// TestResourceConfigValidation: a registered provider's config check rejects a
// binding's resource config at load time without echoing it.
func TestResourceConfigValidation(t *testing.T) {
	reg := registry()
	reg.ResourceConfig = map[string]func(json.RawMessage) error{"git": func(c json.RawMessage) error {
		if strings.Contains(string(c), "\"mode\":\"clone\"") {
			return errors.New("bad: clone")
		}
		return nil
	}}
	if _, err := Validate(example(t, "git-scope"), reg); err != nil {
		t.Fatal(err)
	}
	m := generic(t, example(t, "git-scope"))
	entry(m, 0, "bindings", 0)["resource"].(map[string]any)["config"].(map[string]any)["mode"] = "clone"
	_, err := Validate(encode(t, m), reg)
	got := diagnostics(t, err)
	if len(got) != 1 || got[0].Path != "scopes/0/bindings/0/resource/config" || got[0].Code != "invalid_config" || strings.Contains(got[0].Message, "clone") {
		t.Fatalf("diagnostics %+v", got)
	}
}

// modeExecutor requests the resource mode its binding config names.
type modeExecutor struct{ fakeExecutor }

func (modeExecutor) ResourceMode(c json.RawMessage) string {
	var m struct {
		Mode string `json:"workspace_mode"`
	}
	_ = json.Unmarshal(c, &m)
	return m.Mode
}

// TestSandboxedResource: a binding that declares a sandbox (a stand-in until
// B09 adds the field) is refused when its provider says the workspace the
// executor would request writes outside the sandbox, with the provider's
// reason, and accepted otherwise.
func TestSandboxedResource(t *testing.T) {
	reg := registry()
	reg.Executors["agent"] = modeExecutor{}
	var asked []string
	reg.ResourceSandbox = map[string]func(json.RawMessage, string) error{"git": func(c json.RawMessage, mode string) error {
		asked = append(asked, mode)
		if mode == "clone" || mode == "" && strings.Contains(string(c), `"mode":"clone"`) {
			return nil
		}
		return errors.New("use mode clone")
	}}
	for _, tc := range []struct {
		config, resource string
		refused          bool
	}{
		{`{"workspace_mode":"work"}`, `{"mode":"clone"}`, true},
		{`{"workspace_mode":"clone"}`, `{"mode":"work"}`, false},
		{`{}`, `{"mode":"clone"}`, false},
		{`{}`, `{}`, true},
	} {
		v := &validator{reg: reg}
		v.sandboxed("scopes/0/bindings/0", Binding{Executor: "agent", Config: json.RawMessage(tc.config), Resource: &Resource{Provider: "git", Config: json.RawMessage(tc.resource)}})
		if tc.refused != (len(v.diags) == 1) || tc.refused && (v.diags[0].Path != "scopes/0/bindings/0/resource/config" || v.diags[0].Code != "sandbox_workspace" || v.diags[0].Message != "use mode clone") {
			t.Errorf("%s with %s: %+v", tc.config, tc.resource, v.diags)
		}
	}
	if strings.Join(asked, ",") != "work,clone,," {
		t.Errorf("modes asked: %q", asked)
	}
	// No resource, or a provider with no sandbox check, is not refused.
	v := &validator{reg: reg}
	v.sandboxed("p", Binding{Executor: "agent"})
	v.sandboxed("p", Binding{Executor: "shell", Resource: &Resource{Provider: "other"}})
	if len(v.diags) != 0 {
		t.Errorf("diagnostics %+v", v.diags)
	}
}

// TestSandboxBinding: a binding's sandbox (B09) is parsed, kept in the
// effective config and checked: a registered provider that accepts its
// config, an executor that can run in one and a workspace a sandbox can use.
func TestSandboxBinding(t *testing.T) {
	reg := registry()
	reg.Sandboxes = map[string]func(json.RawMessage) error{"srt": func(c json.RawMessage) error {
		if strings.Contains(string(c), "bad") {
			return errors.New("invalid srt sandbox config: fixed reason")
		}
		return nil
	}}
	reg.ResourceSandbox = map[string]func(json.RawMessage, string) error{"git": func(c json.RawMessage, _ string) error {
		if !strings.Contains(string(c), `"mode":"clone"`) {
			return errors.New("use mode clone")
		}
		return nil
	}}
	reg.Executors["coding_agent"] = sandboxCheckingExecutor{reg.Executors["coding_agent"].(interruptExecutor)}
	snap, err := Validate(example(t, "sandboxed"), reg)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := snap.Scopes["engineering"].Binding("run")
	if b.Sandbox == nil || b.Sandbox.Provider != "srt" || !strings.Contains(string(b.Sandbox.Config), "{workspace}") {
		t.Fatalf("sandbox %+v", b.Sandbox)
	}
	for _, tc := range []struct {
		name, path, code string
		mutate           func(b map[string]any)
	}{
		{"unknown provider", "scopes/0/bindings/0/sandbox/provider", "unknown_registration", func(b map[string]any) { b["sandbox"].(map[string]any)["provider"] = "docker" }},
		{"invalid policy", "scopes/0/bindings/0/sandbox/config", "invalid_config", func(b map[string]any) { b["sandbox"].(map[string]any)["config"] = map[string]any{"bad": true} }},
		{"executor without sandbox", "scopes/0/bindings/0/sandbox", "unsupported_capability", func(b map[string]any) {
			b["executor"], b["requires"] = "support_agent", map[string]any{}
		}},
		{"worktree workspace", "scopes/0/bindings/0/resource/config", "sandbox_workspace", func(b map[string]any) {
			b["resource"].(map[string]any)["config"].(map[string]any)["mode"] = "work"
		}},
		{"missing provider and config", "scopes/0/bindings/0/sandbox", "schema", func(b map[string]any) { b["sandbox"] = map[string]any{} }},
		{"executor refuses the sandboxed config", "scopes/0/bindings/0/config", "sandbox_config", func(b map[string]any) { b["config"] = map[string]any{"shared": true} }},
		{"executor refuses a grant's sandboxed config", "scopes/0/bindings/0/grants/0/config", "sandbox_config", func(b map[string]any) {
			b["grants"] = []any{map[string]any{"name": "shared", "config": map[string]any{"shared": true}}}
		}},
	} {
		m := generic(t, example(t, "sandboxed"))
		tc.mutate(entry(m, 0, "bindings", 0))
		_, err := Validate(encode(t, m), reg)
		found := false
		for _, d := range diagnostics(t, err) {
			if strings.HasPrefix(d.Path, tc.path) && (tc.code == "schema" || d.Code == tc.code) {
				found = true
			}
		}
		if !found {
			t.Errorf("%s: diagnostics %+v, want %s at %s", tc.name, diagnostics(t, err), tc.code, tc.path)
		}
	}
}

// TestContainerBinding: a binding's container is parsed and kept in the
// effective config, and refused when combined with a sandbox, when its
// image isn't pinned by digest, when its provider isn't registered or when
// its executor can't run in a container.
func TestContainerBinding(t *testing.T) {
	const image = "ghcr.io/acme/agent@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	reg := registry()
	reg.Sandboxes = map[string]func(json.RawMessage) error{"srt": nil}
	reg.Containers = map[string]func(json.RawMessage) error{"docker": nil}
	reg.Executors["coding_agent"] = interruptExecutor{fakeExecutor{domain.Capabilities{Toolbelt: "mcp", Interrupt: true, Workspace: true, Sandbox: true, Container: true}}}
	contained := func(b map[string]any) {
		delete(b, "sandbox")
		b["container"] = map[string]any{"provider": "docker", "config": map[string]any{"image": image, "write": []any{"{workspace}"}}}
	}
	m := generic(t, example(t, "sandboxed"))
	contained(entry(m, 0, "bindings", 0))
	snap, err := Validate(encode(t, m), reg)
	if err != nil {
		t.Fatalf("%+v", diagnostics(t, err))
	}
	b, _ := snap.Scopes["engineering"].Binding("run")
	if b.Container == nil || b.Container.Provider != "docker" || !strings.Contains(string(b.Container.Config), image) || b.Sandbox != nil {
		t.Fatalf("container %+v", b.Container)
	}
	// An executor's own sandbox config check (claude-code's config
	// directory) is for a process sandbox: a container gives it the Work's
	// home instead.
	checking := reg.Executors["coding_agent"]
	reg.Executors["coding_agent"] = sandboxCheckingExecutor{interruptExecutor{fakeExecutor{domain.Capabilities{Toolbelt: "mcp", Interrupt: true, Workspace: true, Sandbox: true, Container: true}}}}
	m = generic(t, example(t, "sandboxed"))
	contained(entry(m, 0, "bindings", 0))
	entry(m, 0, "bindings", 0)["config"] = map[string]any{"shared": true}
	if _, err := Validate(encode(t, m), reg); err != nil {
		t.Errorf("a container binding got the sandbox config check: %+v", diagnostics(t, err))
	}
	reg.Executors["coding_agent"] = checking
	for _, tc := range []struct {
		name, path, code string
		mutate           func(b map[string]any)
	}{
		{"with a sandbox", "scopes/0/bindings/0/container", "sandbox_conflict", func(b map[string]any) {
			b["sandbox"] = map[string]any{"provider": "srt", "config": map[string]any{}}
		}},
		{"image by tag", "scopes/0/bindings/0/container/config", "invalid_config", func(b map[string]any) {
			b["container"].(map[string]any)["config"].(map[string]any)["image"] = "ghcr.io/acme/agent:latest"
		}},
		{"unknown provider", "scopes/0/bindings/0/container/provider", "unknown_registration", func(b map[string]any) { b["container"].(map[string]any)["provider"] = "podman" }},
		{"executor without container", "scopes/0/bindings/0/container", "unsupported_capability", func(b map[string]any) {
			b["executor"], b["requires"] = "support_agent", map[string]any{}
		}},
		{"worktree workspace", "scopes/0/bindings/0/resource/config", "sandbox_workspace", func(b map[string]any) {
			b["resource"].(map[string]any)["config"].(map[string]any)["mode"] = "work"
		}},
	} {
		reg.ResourceSandbox = map[string]func(json.RawMessage, string) error{"git": func(c json.RawMessage, _ string) error {
			if !strings.Contains(string(c), `"mode":"clone"`) {
				return errors.New("use mode clone")
			}
			return nil
		}}
		m := generic(t, example(t, "sandboxed"))
		contained(entry(m, 0, "bindings", 0))
		tc.mutate(entry(m, 0, "bindings", 0))
		_, err := Validate(encode(t, m), reg)
		found := false
		for _, d := range diagnostics(t, err) {
			if strings.HasPrefix(d.Path, tc.path) && d.Code == tc.code {
				found = true
			}
		}
		if !found {
			t.Errorf("%s: diagnostics %+v, want %s at %s", tc.name, diagnostics(t, err), tc.code, tc.path)
		}
	}
}

// TestBindingTimeout: a binding's timeout_seconds (05-02) reaches the domain
// binding the scheduler pins, survives normalization, and is bounded like
// settings.timeout_seconds.
func TestBindingTimeout(t *testing.T) {
	m := generic(t, example(t, "git-scope"))
	entry(m, 0, "bindings", 0)["timeout_seconds"] = 21600
	snap, err := Validate(encode(t, m), registry())
	if err != nil {
		t.Fatal(err)
	}
	if b := snap.DomainScopes()[0].Bindings[0]; b.TimeoutSeconds != 21600 {
		t.Fatalf("domain binding %+v", b)
	}
	if !strings.Contains(string(snap.Redacted()), `"timeout_seconds":21600`) {
		t.Fatal("normalized config dropped the binding timeout")
	}
	for _, bad := range []any{0, -1, "60", 1.5} {
		m := generic(t, example(t, "git-scope"))
		entry(m, 0, "bindings", 0)["timeout_seconds"] = bad
		if _, err := Validate(encode(t, m), registry()); err == nil {
			t.Fatalf("timeout_seconds %v accepted", bad)
		}
	}
}

// sandboxCheckingExecutor refuses, in a sandbox, a binding config that sets
// "shared".
type sandboxCheckingExecutor struct{ interruptExecutor }

func (sandboxCheckingExecutor) SandboxConfig(c json.RawMessage) error {
	if strings.Contains(string(c), `"shared"`) {
		return errors.New("fixed reason")
	}
	return nil
}

// refusingExecutor rejects a binding config that sets "bad".
type refusingExecutor struct{ interruptExecutor }

func (refusingExecutor) ValidateConfig(c json.RawMessage, _ []string) error {
	if strings.Contains(string(c), `"bad"`) {
		return errors.New("bad")
	}
	return nil
}

// TestBindingGrants: a binding's grants are kept in the normalized config
// and projected onto the domain binding; their operations count towards the
// ceiling but must sit within the scope authorization, and their config is
// checked by the executor as applied over the binding's own.
func TestBindingGrants(t *testing.T) {
	reg := registry()
	reg.Executors["coding_agent"] = refusingExecutor{interruptExecutor{fakeExecutor{domain.Capabilities{Toolbelt: "mcp", Interrupt: true, Workspace: true}}}}
	with := func(grants any) (*Snapshot, error) {
		m := generic(t, example(t, "git-scope"))
		b := entry(m, 0, "bindings", 0)
		b["operations"] = []string{"read_context", "declare_outcome"}
		b["grants"] = grants
		return Validate(encode(t, m), reg)
	}
	snap, err := with([]any{map[string]any{"name": "emitting", "label": "Emit events", "operations": []string{"emit"}, "config": map[string]any{"playbook": "other"}}})
	if err != nil {
		t.Fatal(err)
	}
	b := snap.DomainScopes()[0].Bindings[0]
	if len(b.Grants) != 1 || b.Grants[0].Name != "emitting" || b.Grants[0].Label != "Emit events" || !slices.Equal(b.Grants[0].Operations, []string{"emit"}) {
		t.Fatalf("domain grants %+v", b.Grants)
	}
	if !strings.Contains(string(snap.Redacted()), `"grants":[{"config":{"playbook":"other"},"label":"Emit events","name":"emitting","operations":["emit"]}]`) {
		t.Fatalf("normalized config dropped the grants: %s", snap.Redacted())
	}
	if snap.AuthorizeOperation("engineering", "run", "emit") != nil || snap.AuthorizeOperation("engineering", "run", "wait") == nil {
		t.Fatal("a grant's operation is outside the ceiling, or an unoffered one is inside it")
	}
	for name, c := range map[string]struct {
		grants any
		code   string
	}{
		"outside the authorization": {[]any{map[string]any{"name": "waiting", "operations": []string{"wait"}}}, "operation_denied"},
		"refused config":            {[]any{map[string]any{"name": "worse", "config": map[string]any{"bad": true}}}, "invalid_config"},
		"repeated name":             {[]any{map[string]any{"name": "a"}, map[string]any{"name": "a"}}, "duplicate_name"},
		"unknown field":             {[]any{map[string]any{"name": "a", "secret_names": []string{}}}, "unknown_field"},
		"no name":                   {[]any{map[string]any{"operations": []string{"emit"}}}, "missing_field"},
	} {
		_, err := with(c.grants)
		if got := diagnostics(t, err); !slices.ContainsFunc(got, func(d Diagnostic) bool { return d.Code == c.code && strings.Contains(d.Path, "/grants") }) {
			t.Errorf("%s: %v", name, got)
		}
	}
}

func TestWebhookSettings(t *testing.T) {
	with := func(hooks any) (*Snapshot, error) {
		m := generic(t, example(t, "git-scope"))
		settings, _ := m["settings"].(map[string]any)
		if settings == nil {
			settings = map[string]any{}
			m["settings"] = settings
		}
		settings["webhooks"] = hooks
		return Validate(encode(t, m), registry())
	}
	plain, err := Validate(example(t, "git-scope"), registry())
	if err != nil {
		t.Fatal(err)
	}
	if plain.Settings.Webhooks != (Webhooks{}) || strings.Contains(string(plain.Normalized), "webhooks") {
		t.Fatalf("no stored listener: %+v %s", plain.Settings.Webhooks, plain.Normalized)
	}
	snap, err := with(map[string]any{"addr": "127.0.0.1:8787", "tunnel": "cloudflared", "tunnel_name": "oc", "tunnel_hostname": "hooks.example.com"})
	if err != nil {
		t.Fatal(err)
	}
	want := Webhooks{Addr: "127.0.0.1:8787", Tunnel: "cloudflared", TunnelName: "oc", TunnelHostname: "hooks.example.com"}
	if snap.Settings.Webhooks != want || snap.Hash == plain.Hash {
		t.Fatalf("stored listener %+v", snap.Settings.Webhooks)
	}
	// The normalized form (what a committed generation restores) keeps it.
	again, err := Validate(snap.Normalized, registry())
	if err != nil || again.Settings.Webhooks != want || again.Hash != snap.Hash {
		t.Fatalf("round trip %+v %v", again, err)
	}
	for _, ok := range []map[string]any{
		{"addr": "127.0.0.1:0"},
		{"addr": ":8787", "tunnel": "ngrok", "tunnel_domain": "hooks.ngrok.app"},
		{"addr": "127.0.0.1:8787", "public_url": "https://hooks.example.com"},
		{"addr": "127.0.0.1:8787", "tunnel": "static", "public_url": "http://box.tail.ts.net"},
	} {
		if _, err := with(ok); err != nil {
			t.Fatalf("%v rejected: %v", ok, err)
		}
	}
	for _, c := range []struct {
		hooks any
		path  string
	}{
		{map[string]any{"tunnel": "ngrok"}, "settings/webhooks/addr"},
		{map[string]any{"addr": "8787"}, "settings/webhooks/addr"},
		{map[string]any{"addr": "127.0.0.1:8787", "tunnel": "frp"}, "settings/webhooks/tunnel"},
		{map[string]any{"addr": "127.0.0.1:8787", "tunnel": "cloudflared", "tunnel_domain": "x"}, "settings/webhooks/tunnel_domain"},
		{map[string]any{"addr": "127.0.0.1:8787", "tunnel": "cloudflared", "tunnel_name": "oc"}, "settings/webhooks/tunnel_name"},
		{map[string]any{"addr": "127.0.0.1:8787", "tunnel": "ngrok", "tunnel_hostname": "h"}, "settings/webhooks/tunnel_name"},
		{map[string]any{"addr": "127.0.0.1:8787", "tunnel": "ngrok", "public_url": "https://x"}, "settings/webhooks/public_url"},
		{map[string]any{"addr": "127.0.0.1:8787", "tunnel": "static"}, "settings/webhooks/public_url"},
		{map[string]any{"addr": "127.0.0.1:8787", "public_url": "https://u:p@x"}, "settings/webhooks/public_url"},
		{map[string]any{"addr": "127.0.0.1:8787", "port": 8787}, "settings/webhooks/port"},
		{"127.0.0.1:8787", "settings/webhooks"},
	} {
		_, err := with(c.hooks)
		if err == nil {
			t.Fatalf("%v accepted", c.hooks)
		}
		if d := diagnostics(t, err); len(d) != 1 || d[0].Path != c.path {
			t.Fatalf("%v: %v", c.hooks, d)
		}
	}
}
