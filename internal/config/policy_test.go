package config

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
)

const policyConfig = `{"version":1,"settings":{"concurrency":8},"connections":[],
 "scopes":[{"id":"root","name":"Root","secret_refs":{"token":"cmd:echo hunter2"}}]}`

func mustPolicy(t *testing.T, text string) *Policy {
	t.Helper()
	p, err := ParsePolicy([]byte(text))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func denied(t *testing.T, err error) map[string]string {
	t.Helper()
	var invalid *ValidationError
	if !errors.As(err, &invalid) {
		t.Fatalf("want diagnostics, got %v", err)
	}
	out := map[string]string{}
	for _, d := range invalid.Diagnostics {
		if d.Code == PolicyDenied {
			out[d.Path] = d.Message
		}
	}
	return out
}

func TestPolicyRefusesASchemeWithAPointerAndNeverEchoesTheReference(t *testing.T) {
	reg := Registry{Policy: mustPolicy(t, `{"version":1,"secret_schemes":["file"]}`)}
	_, err := Validate([]byte(policyConfig), reg)
	msg, ok := denied(t, err)["scopes/0/secret_refs/token"]
	if !ok {
		t.Fatalf("no policy_denied at the reference: %v", err)
	}
	if strings.Contains(err.Error(), "hunter2") || strings.Contains(msg, "echo") {
		t.Fatalf("diagnostic echoes the reference: %v", err)
	}
	// Negative control: without the policy the same document validates.
	if _, err := Validate([]byte(policyConfig), Registry{}); err != nil {
		t.Fatal(err)
	}
}

func TestPolicyRefusesAStoredGenerationButClampsItsCeilings(t *testing.T) {
	scheme := Registry{Policy: mustPolicy(t, `{"version":1,"secret_schemes":["file"]}`)}
	if _, err := ValidateStored([]byte(policyConfig), scheme); len(denied(t, err)) != 1 {
		t.Fatalf("a stored generation with a refused scheme: %v", err)
	}
	ceiling := Registry{Policy: mustPolicy(t, `{"version":1,"limits":{"concurrency":2}}`)}
	if _, err := Validate([]byte(policyConfig), ceiling); denied(t, err)["settings/concurrency"] == "" {
		t.Fatalf("an apply above the ceiling: %v", err)
	}
	s, err := ValidateStored([]byte(policyConfig), ceiling)
	if err != nil {
		t.Fatal(err)
	}
	if s.Settings.Concurrency != 2 || s.Scopes["root"].Concurrency != 2 || len(s.PolicyClamps) != 1 {
		t.Fatalf("clamp: settings %d scope %d, %v", s.Settings.Concurrency, s.Scopes["root"].Concurrency, s.PolicyClamps)
	}
	if plain, _ := Validate([]byte(policyConfig), Registry{}); plain.Hash != s.Hash {
		t.Fatal("clamping changed the stored document's hash")
	}
}

func TestPolicyCeilingLeavesAnUnwrittenDefaultValid(t *testing.T) {
	reg := Registry{Policy: mustPolicy(t, `{"version":1,"limits":{"concurrency":2,"job_concurrency":1}}`)}
	s, err := Validate([]byte(`{"version":1,"settings":{},"connections":[],"scopes":[{"id":"root","name":"Root"}]}`), reg)
	if err != nil {
		t.Fatal(err)
	}
	if s.Settings.Concurrency != 2 || s.Settings.JobConcurrency != 1 {
		t.Fatalf("defaults not held to the ceiling: %+v", s.Settings)
	}
}

func TestPolicyBudgetCantBeRemovedOrRaised(t *testing.T) {
	reg := Registry{Policy: mustPolicy(t, `{"version":1,"budgets":[{"unit":"usd","per":"all","max":10}]}`)}
	s, err := Validate([]byte(`{"version":1,"settings":{},"connections":[],"scopes":[{"id":"root","name":"Root"}]}`), reg)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Settings.Budgets) != 1 || !s.Settings.Budgets[0].Pinned || s.Settings.Budgets[0].Max != 10 {
		t.Fatalf("a config without the budget: %+v", s.Settings.Budgets)
	}
	raised := `{"version":1,"settings":{"budgets":[{"unit":"usd","per":"all","max":50}]},"connections":[],"scopes":[{"id":"root","name":"Root"}]}`
	if _, err := Validate([]byte(raised), reg); denied(t, err)["settings/budgets/0/max"] == "" {
		t.Fatalf("a raised budget: %v", err)
	}
	stored, err := ValidateStored([]byte(raised), reg)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored.Settings.Budgets) != 1 || stored.Settings.Budgets[0].Max != 10 || !stored.Settings.Budgets[0].Pinned {
		t.Fatalf("a stored raised budget: %+v", stored.Settings.Budgets)
	}
}

func TestPolicyExecutorsEntryTypesAndPassEnv(t *testing.T) {
	doc := `{"version":1,"settings":{},"connections":[],"scopes":[{"id":"root","name":"Root",
	 "bindings":[{"name":"b","executor":"shell","pass_env":["HOME"]}],
	 "extensions":[{"name":"e","type":"thing"}]}]}`
	reg := Registry{Executors: map[string]extension.Executor{}, Extensions: map[string]ExtensionType{},
		Policy: mustPolicy(t, `{"version":1,"executors":[],"entry_types":[],"pass_env":[]}`)}
	got := denied(t, func() error { _, err := Validate([]byte(doc), reg); return err }())
	for _, p := range []string{"scopes/0/bindings/0/executor", "scopes/0/bindings/0/pass_env/0", "scopes/0/extensions/0/type"} {
		if got[p] == "" {
			t.Errorf("no policy_denied at %s: %v", p, got)
		}
	}
}

func TestPolicyRefusesAnIncludedFolderWithoutReadingIt(t *testing.T) {
	doc := `{"version":1,"settings":{},"connections":[],"includes":[{"path":"/nonexistent/team","parent":"root"}],"scopes":[{"id":"root","name":"Root"}]}`
	reg := Registry{Policy: mustPolicy(t, `{"version":1,"included_folders":false}`)}
	if _, err := ValidateFile([]byte(doc), reg, Includes{Base: t.TempDir()}); denied(t, err)["includes/0/path"] == "" {
		t.Fatalf("an included folder: %v", err)
	}
}

func TestPolicyHoldsIsolationAtTheRootAndAChildCantDropIt(t *testing.T) {
	doc := `{"version":1,"settings":{},"connections":[],"scopes":[{"id":"root","name":"Root","authorization":{"connections":[],"operations":[],"read_scopes":[],"file_scopes":[]}},
	 {"id":"kid","name":"Kid","parent":"root","authorization":{"connections":[],"operations":[],"read_scopes":[],"file_scopes":[]},
	  "bindings":[{"name":"b","executor":"shell"}]}]}`
	reg := Registry{Policy: mustPolicy(t, `{"version":1,"isolation":"required"}`)}
	if _, err := Validate([]byte(doc), reg); denied(t, err)["scopes/1/bindings/0"] == "" {
		t.Fatalf("an unconfined binding: %v", err)
	}
}

func TestResolverRefusesADisallowedSchemeOnEveryPath(t *testing.T) {
	r := &Resolver{Policy: mustPolicy(t, `{"version":1,"secret_schemes":["file"]}`), LookupEnv: func(string) (string, bool) { return "v", true }}
	ctx := context.Background()
	if _, err := r.Resolve(ctx, "env:TOKEN"); SecretCode(err) != PolicyDenied {
		t.Fatalf("Resolve: %v", err)
	}
	if _, err := r.For("pkg").Fresh().ResolveNamed(ctx, "t", "cmd:echo x"); SecretCode(err) != PolicyDenied {
		t.Fatalf("ResolveNamed: %v", err)
	}
	scope := &EffectiveScope{ID: domain.ID("s"), SecretRefs: map[string]string{"t": "env:TOKEN"}}
	if _, err := r.ResolveNames(ctx, scope, []string{"t"}); SecretCode(err) != PolicyDenied {
		t.Fatalf("ResolveNames: %v", err)
	}
	// Negative control: an allowed policy resolves it.
	open := &Resolver{Policy: mustPolicy(t, `{"version":1,"secret_schemes":["env"]}`), LookupEnv: r.LookupEnv}
	if v, err := open.Resolve(ctx, "env:TOKEN"); err != nil || v != "v" {
		t.Fatalf("allowed scheme: %q %v", v, err)
	}
}

func TestLoadPolicy(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "policy.json")
	if err := os.WriteFile(path, []byte(`{"version":1}`), 0o444); err != nil {
		t.Fatal(err)
	}
	p, err := LoadPolicy(path)
	if err != nil || len(p.Digest) != 64 || p.Path != path {
		t.Fatalf("a read-only policy: %+v %v", p, err)
	}
	loose := filepath.Join(dir, "loose.json")
	if err := os.WriteFile(loose, []byte(`{"version":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(loose, 0o666); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadPolicy(loose); !errors.Is(err, ErrUnsafePolicy) {
		t.Fatalf("a world-writable policy: %v", err)
	}
	if p, err := LoadPolicy(""); p != nil || err != nil {
		t.Fatalf("no path: %v %v", p, err)
	}
	if _, err := LoadPolicy(filepath.Join(dir, "gone.json")); err == nil {
		t.Fatal("a missing policy file must fail, not mean no policy")
	}
	for _, bad := range []string{`{"version":1,"secret_scheme":["file"]}`, `{"version":2}`, `{}`, `{"version":1,"limits":{"concurrency":0}}`} {
		if _, err := ParsePolicy([]byte(bad)); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
	var nilPolicy *Policy
	if !nilPolicy.AllowsScheme("cmd") || !nilPolicy.AllowsPassEnv("X") || nilPolicy.RequiresIsolation() {
		t.Fatal("no policy must allow everything")
	}
}
