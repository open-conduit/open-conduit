package config

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/conformance"
	"github.com/open-conduit/open-conduit/extension"
)

// The resolver holds the host's side of a secret provider: the shared
// suite's cases, run against it.
func TestResolverIsAConformingSecretHost(t *testing.T) {
	conformance.RunSecretHost(t, func(p extension.SecretProvider) conformance.SecretHost {
		return resolverHost{&Resolver{Providers: func(scheme string) (extension.SecretProvider, string, bool) {
			return p, "fixture", scheme == conformance.SecretScheme
		}}}
	})
}

type resolverHost struct{ r *Resolver }

func (h resolverHost) Resolve(ctx context.Context, ref string) (string, string) {
	v, err := h.r.Resolve(ctx, ref)
	return v, SecretCode(err)
}

type funcProvider func(context.Context, extension.SecretRequest) (string, error)

func (f funcProvider) Serve(ctx context.Context, r extension.SecretRequest) (string, error) {
	return f(ctx, r)
}

func TestProviderResolution(t *testing.T) {
	var got []SecretResolution
	served := funcProvider(func(_ context.Context, r extension.SecretRequest) (string, error) {
		if r.Reference == "missing" {
			return "", &extension.SecretRefusal{Code: "not_found"}
		}
		return "v-" + r.Reference, nil
	})
	r := &Resolver{
		Providers: func(scheme string) (extension.SecretProvider, string, bool) {
			return served, "vault", scheme == "vault"
		},
		Observe: func(_ context.Context, s SecretResolution) { got = append(got, s) },
	}
	ctx := context.Background()
	if v, err := r.Resolve(ctx, "vault:kv/app#token"); err != nil || v != "v-kv/app#token" {
		t.Fatalf("resolved %q, %v", v, err)
	}
	_, err := r.Resolve(ctx, "vault:missing")
	failure(t, err, "not_found")
	_, err = r.Resolve(ctx, "aws:arn:aws:secretsmanager:eu-west-1:1:secret:x")
	failure(t, err, "provider_unavailable")
	// The package that serves a scheme is never served by it.
	_, err = r.For("vault").Resolve(ctx, "vault:kv/app#token")
	failure(t, err, "provider_cycle")
	if v, err := r.For("github").Resolve(ctx, "vault:kv/app#token"); err != nil || v == "" {
		t.Fatalf("another package's resolution failed: %v", err)
	}
	_, err = (&Resolver{}).Resolve(ctx, "vault:kv/app")
	failure(t, err, "provider_unavailable")

	// Each resolution is recorded by reference and outcome, never value.
	if len(got) != 5 {
		t.Fatalf("recorded %d resolutions, want 5: %+v", len(got), got)
	}
	want := []SecretResolution{
		{Reference: "vault:kv/app#token", Scheme: "vault", Provider: "vault"},
		{Reference: "vault:missing", Scheme: "vault", Provider: "vault", Code: "not_found"},
		{Reference: "aws:arn:aws:secretsmanager:eu-west-1:1:secret:x", Scheme: "aws", Code: "provider_unavailable"},
		{Reference: "vault:kv/app#token", Scheme: "vault", Provider: "vault", Consumer: "vault", Code: "provider_cycle"},
		{Reference: "vault:kv/app#token", Scheme: "vault", Provider: "vault", Consumer: "github"},
	}
	for i, w := range want {
		g := got[i]
		g.Duration = 0
		if g != w {
			t.Errorf("record %d = %+v, want %+v", i, g, w)
		}
		if strings.Contains(g.Code, "v-") {
			t.Errorf("record %d carries the value", i)
		}
	}
}

func TestProviderTimeoutAndFailureAreSafe(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	blocked := funcProvider(func(ctx context.Context, _ extension.SecretRequest) (string, error) {
		<-ctx.Done()
		return "late", ctx.Err()
	})
	r := &Resolver{Providers: func(string) (extension.SecretProvider, string, bool) { return blocked, "vault", true }}
	_, err := r.Resolve(ctx, "vault:kv/app")
	failure(t, err, "cancelled")
	leaky := funcProvider(func(context.Context, extension.SecretRequest) (string, error) {
		return "", errors.New("permission denied for token hunter2")
	})
	r.Providers = func(string) (extension.SecretProvider, string, bool) { return leaky, "vault", true }
	_, err = r.Resolve(context.Background(), "vault:kv/app")
	failure(t, err, "provider_failed", "hunter2", "kv/app")
}

func TestProviderReferenceSyntax(t *testing.T) {
	for _, ref := range []string{"vault:secret/data/app#token", "aws:arn:aws:secretsmanager:eu-west-1:123:secret:gh", "gcp-sm:projects/p/secrets/s/versions/latest"} {
		if !ValidSecretRef(ref) || ProviderScheme(ref) == "" {
			t.Errorf("%q is a provider reference", ref)
		}
	}
	for _, ref := range []string{"keyring:x", "file:x", "env:X", "cmd:echo"} {
		if ProviderScheme(ref) != "" {
			t.Errorf("%q is a built-in form, not a provider's", ref)
		}
	}
	for _, ref := range []string{"vault:", "vault:a b", "vault:a\tb", "Vault:x", "1vault:x", "vault:" + strings.Repeat("a", 1025)} {
		if ValidSecretRef(ref) {
			t.Errorf("%q is not a valid reference", ref)
		}
	}
}

func providerRegistry() Registry {
	reg := registry()
	reg.SecretSchemes = map[string]string{"vault": "vault", "aws": "aws"}
	reg.Connections["vault"] = nil
	reg.Connections["aws"] = nil
	reg.ConnectionOwners = map[string]string{"vault": "vault", "aws": "aws", "chat": "chat"}
	return reg
}

func providerConfig(t *testing.T, connections ...map[string]any) []byte {
	t.Helper()
	conns := []any{}
	for _, c := range connections {
		conns = append(conns, c)
	}
	return encode(t, map[string]any{
		"version": 1, "settings": map[string]any{}, "connections": conns,
		"scopes": []any{map[string]any{"id": "root", "name": "Root", "secret_refs": map[string]any{"token": "vault:kv/app#token"}}},
	})
}

func conn(id, typ, ref string) map[string]any {
	return map[string]any{"id": id, "type": typ, "config": map[string]any{}, "secret_refs": map[string]any{"token": ref}}
}

func diagnosticsOf(err error) []Diagnostic {
	var ve *ValidationError
	if errors.As(err, &ve) {
		return ve.Diagnostics
	}
	return nil
}

func TestProviderSchemesAreValidatedAgainstPackages(t *testing.T) {
	data := providerConfig(t, conn("chat", "chat", "aws:arn:x"))
	if _, err := Validate(data, providerRegistry()); err != nil {
		t.Fatalf("declared schemes refused: %v", err)
	}
	_, err := Validate(data, registry())
	ds := diagnostics(t, err)
	if len(ds) != 2 {
		t.Fatalf("undeclared schemes: %v", err)
	}
	for _, d := range ds {
		if d.Code != "unknown_registration" || !strings.HasSuffix(d.Path, "/secret_refs/token") || strings.Contains(d.Message, "kv/app") || strings.Contains(d.Message, "arn:x") {
			t.Errorf("diagnostic %+v", d)
		}
	}
}

func TestProviderCredentialCycleIsRefused(t *testing.T) {
	for name, tc := range map[string]struct {
		conns []map[string]any
		cycle bool
	}{
		"own scheme":      {[]map[string]any{conn("v", "vault", "vault:auth/token")}, true},
		"through another": {[]map[string]any{conn("v", "vault", "aws:arn:vault-role"), conn("a", "aws", "vault:aws/creds")}, true},
		"a chain":         {[]map[string]any{conn("v", "vault", "aws:arn:vault-role"), conn("a", "aws", "env:AWS_SECRET")}, false},
		"from env":        {[]map[string]any{conn("v", "vault", "env:VAULT_TOKEN")}, false},
	} {
		_, err := Validate(providerConfig(t, tc.conns...), providerRegistry())
		cycle := false
		for _, d := range diagnosticsOf(err) {
			if d.Code == "provider_cycle" {
				cycle = true
			}
		}
		if cycle != tc.cycle || !tc.cycle && err != nil {
			t.Errorf("%s: cycle %t, want %t (%v)", name, cycle, tc.cycle, err)
		}
	}
}

func TestProviderCycleThroughAnEntrysSecrets(t *testing.T) {
	reg := providerRegistry()
	reg.Extensions["vault_auth"] = ExtensionType{Owner: "vault"}
	data := encode(t, map[string]any{
		"version": 1, "settings": map[string]any{}, "connections": []any{},
		"scopes": []any{map[string]any{"id": "root", "name": "Root",
			"secret_refs": map[string]any{"token": "vault:kv/auth"},
			"extensions":  []any{map[string]any{"name": "auth", "type": "vault_auth", "config": map[string]any{}, "secret_names": []any{"token"}}}}},
	})
	_, err := Validate(data, reg)
	ds := diagnostics(t, err)
	if len(ds) != 1 || ds[0].Code != "provider_cycle" {
		t.Fatalf("%v", err)
	}
}

func TestResolveNamesRecordsTheSecretsName(t *testing.T) {
	var got []SecretResolution
	r := &Resolver{
		Providers: func(string) (extension.SecretProvider, string, bool) {
			return funcProvider(func(context.Context, extension.SecretRequest) (string, error) { return "v", nil }), "vault", true
		},
		Observe: func(_ context.Context, s SecretResolution) { got = append(got, s) },
	}
	scope := &EffectiveScope{SecretRefs: map[string]string{"token": "vault:kv/app", "plain": "env:HOME"}}
	if _, err := r.For("github").ResolveNames(context.Background(), scope, []string{"token", "plain"}); err != nil {
		t.Fatal(err)
	}
	// Only the provider-backed resolution is recorded, under its name.
	if len(got) != 1 || got[0].Name != "token" || got[0].Consumer != "github" || got[0].Code != "" {
		t.Fatalf("%+v", got)
	}
}

// countingProvider counts its calls and, with gate set, blocks each until
// gate is closed.
type countingProvider struct {
	mu    sync.Mutex
	calls int
	gate  chan struct{}
	err   error
}

func (p *countingProvider) Serve(ctx context.Context, r extension.SecretRequest) (string, error) {
	p.mu.Lock()
	p.calls++
	gate := p.gate
	p.mu.Unlock()
	if gate != nil {
		<-gate
	}
	if p.err != nil {
		return "", p.err
	}
	return "v-" + r.Reference, nil
}

func (p *countingProvider) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

func cachedResolver(p extension.SecretProvider, now *time.Time, records *int) *Resolver {
	return &Resolver{
		Providers: func(string) (extension.SecretProvider, string, bool) { return p, "vault", true },
		Observe:   func(context.Context, SecretResolution) { *records++ },
		Cache:     &SecretCache{Now: func() time.Time { return *now }},
	}
}

// A consumer that resolves on every inbound request (a webhook's signature
// check) asks the provider once per lifetime, not once per request.
func TestProviderAnswersAreCachedForABoundedLifetime(t *testing.T) {
	p := &countingProvider{}
	now, records := time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC), 0
	r := cachedResolver(p, &now, &records)
	ctx := context.Background()
	for range 50 {
		if v, err := r.For("github").Resolve(ctx, "vault:kv/hook"); err != nil || v != "v-kv/hook" {
			t.Fatalf("%q, %v", v, err)
		}
	}
	if p.count() != 1 || records != 1 {
		t.Fatalf("%d calls and %d records for 50 resolutions, want 1", p.count(), records)
	}
	// Another consumer has its own entry.
	_, _ = r.For("slack").Resolve(ctx, "vault:kv/hook")
	if p.count() != 2 {
		t.Fatalf("another consumer shared the entry")
	}
	now = now.Add(DefaultSecretTTL)
	_, _ = r.For("github").Resolve(ctx, "vault:kv/hook")
	if p.count() != 3 {
		t.Fatalf("an expired entry answered")
	}
	// A config apply drops every entry.
	r.Cache.Reset()
	_, _ = r.For("github").Resolve(ctx, "vault:kv/hook")
	if p.count() != 4 {
		t.Fatalf("an entry outlived Reset")
	}
	// A check of whether it resolves now always asks.
	_, _ = r.For("github").Fresh().Resolve(ctx, "vault:kv/hook")
	if p.count() != 5 {
		t.Fatalf("Fresh answered from the cache")
	}
}

func TestProviderRefusalsAreCachedBriefly(t *testing.T) {
	p := &countingProvider{err: &extension.SecretRefusal{Code: "not_found"}}
	now, records := time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC), 0
	r := cachedResolver(p, &now, &records)
	for range 20 {
		_, err := r.Resolve(context.Background(), "vault:kv/missing")
		failure(t, err, "not_found")
	}
	if p.count() != 1 {
		t.Fatalf("%d calls for 20 refused resolutions", p.count())
	}
	now = now.Add(DefaultSecretFailureTTL)
	p.mu.Lock()
	p.err = nil
	p.mu.Unlock()
	if v, err := r.Resolve(context.Background(), "vault:kv/missing"); err != nil || v == "" || p.count() != 2 {
		t.Fatalf("a refusal outlived its lifetime: %q, %v, %d calls", v, err, p.count())
	}
}

func TestConcurrentResolutionsShareOneProviderCall(t *testing.T) {
	p := &countingProvider{gate: make(chan struct{})}
	now, records := time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC), 0
	r := cachedResolver(p, &now, &records)
	const n = 20
	results := make(chan string, n)
	for range n {
		go func() {
			v, _ := r.Resolve(context.Background(), "vault:kv/hook")
			results <- v
		}()
	}
	// A caller that leaves early is cancelled and doesn't fail the rest.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := r.Resolve(ctx, "vault:kv/hook")
	failure(t, err, "cancelled")
	close(p.gate)
	for range n {
		if v := <-results; v != "v-kv/hook" {
			t.Fatalf("a shared call answered %q", v)
		}
	}
	if p.count() != 1 {
		t.Fatalf("%d provider calls for %d concurrent resolutions", p.count(), n)
	}
}

// A call in flight across a Reset doesn't store what it answers.
func TestResetDiscardsACallInFlight(t *testing.T) {
	p := &countingProvider{gate: make(chan struct{})}
	now, records := time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC), 0
	r := cachedResolver(p, &now, &records)
	done := make(chan struct{})
	go func() { _, _ = r.Resolve(context.Background(), "vault:kv/hook"); close(done) }()
	for p.count() == 0 {
		runtime.Gosched()
	}
	r.Cache.Reset()
	close(p.gate)
	<-done
	p.mu.Lock()
	p.gate = nil
	p.mu.Unlock()
	_, _ = r.Resolve(context.Background(), "vault:kv/hook")
	if p.count() != 2 {
		t.Fatalf("an answer from before Reset was cached")
	}
}
