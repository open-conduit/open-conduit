package claudecode_test

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
	"github.com/open-conduit/open-conduit/extension/inproc"
	"github.com/open-conduit/open-conduit/extensions/claudecode"
	"github.com/open-conduit/open-conduit/protocol"
)

// TestProviderConfig: a provider needs a usable base URL and a selected
// secret, and a binding with one may not also set the variables it owns.
func TestProviderConfig(t *testing.T) {
	e := newHarness(t).executor()
	ok := `{"provider":{"base_url":"https://openrouter.ai/api","auth_token_secret":"or"},"model":"anthropic/claude-opus-5-5","models":["openai/gpt-5"]}`
	if err := e.ValidateConfig(json.RawMessage(ok), []string{"or"}); err != nil {
		t.Fatalf("valid provider refused: %v", err)
	}
	if err := e.ValidateConfig(json.RawMessage(`{"provider":{"base_url":"http://localhost:11434"}}`), nil); err != nil {
		t.Fatalf("a local provider without a secret refused: %v", err)
	}
	for _, c := range []struct {
		config  string
		secrets []string
		path    string
		code    string
	}{
		{ok, nil, "/provider/auth_token_secret", "secret_not_selected"},
		{`{"provider":{"base_url":"openrouter.ai/api"}}`, nil, "/provider/base_url", "invalid_base_url"},
		{`{"provider":{"base_url":"ftp://openrouter.ai"}}`, nil, "/provider/base_url", "invalid_base_url"},
		{`{"provider":{"base_url":"https://user:pw@openrouter.ai"}}`, nil, "/provider/base_url", "invalid_base_url"},
		{`{"provider":{"base_url":"https://openrouter.ai/api?key=x"}}`, nil, "/provider/base_url", "invalid_base_url"},
		{`{"provider":{}}`, nil, "/provider/base_url", "invalid_base_url"},
		{`{"provider":{"base_url":"https://openrouter.ai/api"},"env":{"ANTHROPIC_BASE_URL":"https://x"}}`, nil, "/env/ANTHROPIC_BASE_URL", "provider_env"},
		{`{"provider":{"base_url":"https://openrouter.ai/api"},"secret_env":{"k":"ANTHROPIC_API_KEY"}}`, []string{"k"}, "/secret_env/k", "provider_env"},
		{`{"provider":{"base_url":"https://openrouter.ai/api","auth_token_secret":"k"},"secret_env":{"k":"OTHER"}}`, []string{"k"}, "/secret_env/k", "provider_env"},
		{`{"provider":{"base_url":"https://openrouter.ai/api","token":"x"}}`, nil, "/provider/token", "unknown_field"},
	} {
		err := e.ValidateConfig(json.RawMessage(c.config), c.secrets)
		ds := extension.ConfigDiagnostics(err)
		if !errors.Is(err, claudecode.ErrConfig) || len(ds) != 1 || ds[0].Path != c.path || ds[0].Code != c.code || strings.Contains(ds[0].Message, "pw@") {
			t.Errorf("%s: %v %+v; want %s %s", c.config, err, ds, c.path, c.code)
		}
	}
	// Without a provider, env may still set the variables itself.
	if err := e.ValidateConfig(json.RawMessage(`{"env":{"ANTHROPIC_BASE_URL":"https://x"}}`), nil); err != nil {
		t.Errorf("env without a provider refused: %v", err)
	}
}

// TestProviderEnvironment: a provider's turns get its base URL and the
// selected secret as the bearer token, never under the secret's own name,
// with the API key emptied; one without a secret sends a placeholder, so the
// binding's Claude login never goes to the provider.
func TestProviderEnvironment(t *testing.T) {
	h := newHarness(t)
	e := h.executor()
	r, _ := h.request("work/1", "exec-1", 1, map[string]any{"provider": map[string]string{"base_url": "https://openrouter.ai/api", "auth_token_secret": "or"}})
	r.Context.Secrets = map[string]string{"or": "sk-or-secret"}
	if res := h.run(e, r); res.Outcome.Kind == domain.OutcomeFailed {
		t.Fatalf("turn failed: %+v", res.Outcome)
	}
	env := envLines(t, h)
	for _, want := range []string{"ANTHROPIC_BASE_URL=https://openrouter.ai/api", "ANTHROPIC_AUTH_TOKEN=sk-or-secret", "ANTHROPIC_API_KEY="} {
		if !slices.Contains(env, want) {
			t.Errorf("environment lacks %s:\n%s", want, strings.Join(env, "\n"))
		}
	}
	if slices.Contains(env, "OR=sk-or-secret") {
		t.Errorf("the provider secret is under its own name too")
	}

	r, _ = h.request("work/2", "exec-2", 1, map[string]any{"provider": map[string]string{"base_url": "http://localhost:11434"}})
	if res := h.run(e, r); res.Outcome.Kind == domain.OutcomeFailed {
		t.Fatalf("turn failed: %+v", res.Outcome)
	}
	if env := envLines(t, h); !slices.Contains(env, "ANTHROPIC_AUTH_TOKEN=open-conduit") || !slices.Contains(env, "ANTHROPIC_BASE_URL=http://localhost:11434") {
		t.Errorf("a provider without a secret:\n%s", strings.Join(env, "\n"))
	}
}

func envLines(t *testing.T, h *harness) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(h.fake, "env"))
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

// TestProviderSandboxHost: a sandboxed launch asks for its provider's host
// (with the port the URL names) instead of api.anthropic.com, which a
// binding without one still asks for.
func TestProviderSandboxHost(t *testing.T) {
	h := newHarness(t)
	e := h.executor()
	b := &recordingBroker{}
	for i, p := range []map[string]string{nil, {"base_url": "https://openrouter.ai/api"}, {"base_url": "http://localhost:11434/"}} {
		extra := map[string]any{}
		if p != nil {
			extra["provider"] = p
		}
		work := "work/" + string(rune('a'+i))
		r, _ := h.request(work, "exec-"+work, 1, extra)
		r.Context.Sandbox = b
		if res := h.run(e, r); res.Outcome.Kind == domain.OutcomeFailed {
			t.Fatalf("%v: %+v", p, res.Outcome)
		}
	}
	var hosts [][]string
	for _, n := range b.needs {
		hosts = append(hosts, n.Hosts)
	}
	if want := [][]string{{"api.anthropic.com"}, {"openrouter.ai"}, {"localhost:11434"}}; !slices.EqualFunc(hosts, want, slices.Equal) {
		t.Errorf("hosts %v, want %v", hosts, want)
	}
}

// TestProviderUsageDoesNotApply: doctor reads no usage for a binding with a
// provider and says it doesn't apply, and validating it warns; a binding
// without a provider beside it is still read.
func TestProviderUsageDoesNotApply(t *testing.T) {
	x := claudecode.NewExtension()
	var reads []string
	x.StubChecks(func(_ context.Context, dir string) ([]claudecode.UsageWindow, error) {
		reads = append(reads, dir)
		return nil, nil
	}, func(context.Context, string) (string, error) { return "2.1.0 (Claude Code)", nil })
	raw := func(cfg map[string]any) json.RawMessage { b, _ := json.Marshal(cfg); return b }
	routed := map[string]any{"usage": true, "provider": map[string]string{"base_url": "https://openrouter.ai/api"}, "env": map[string]string{"CLAUDE_CONFIG_DIR": "/routed"}}
	cfg := protocol.CheckConfig{Scopes: []protocol.CheckScope{{ID: "eng", Bindings: []protocol.CheckBinding{
		{Name: "routed", Executor: claudecode.Name, Config: raw(routed)},
		{Name: "direct", Executor: claudecode.Name, Config: raw(map[string]any{"usage": true})},
	}}}}
	got := x.Checks()[0].Run(context.Background(), cfg)
	if got.Status != inproc.CheckOK || !slices.Equal(reads, []string{""}) || !slices.Contains(got.Items, "usage doesn't apply to bindings with a provider (its windows are Anthropic's): eng/routed") {
		t.Errorf("check %+v, reads %q", got, reads)
	}
	e := newHarness(t).executor()
	if w := e.ConfigWarnings(raw(routed)); len(w) != 1 || !strings.Contains(w[0], "usage doesn't apply") {
		t.Errorf("warnings %q", w)
	}
	if w := e.ConfigWarnings(raw(map[string]any{"usage": true})); len(w) != 0 {
		t.Errorf("usage without a provider warned: %q", w)
	}
}
