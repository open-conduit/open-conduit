package codex_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/open-conduit/open-conduit/extensions/codex"
)

// TestProviderOverrides: a provider becomes Codex's model_provider and
// model_providers.<id> overrides, its key passed only as the provider's
// env_key, never under the secret's own name; one without a key sets no
// env_key.
func TestProviderOverrides(t *testing.T) {
	f := newFixture(t)
	r := request("work-p", "exec-p", f.config(map[string]any{"model": "openai/gpt-5", "provider": map[string]string{"id": "openrouter", "base_url": "https://openrouter.ai/api/v1", "api_key_secret": "or"}}, nil))
	r.Context.Secrets = map[string]string{"or": "sk-or-test"}
	run(t, f.executor(), r)
	args, env := f.read("call-1"), f.read("env-1")
	for _, want := range []string{`model_provider="openrouter"`, `model_providers.openrouter.name="openrouter"`, `model_providers.openrouter.base_url="https://openrouter.ai/api/v1"`, `model_providers.openrouter.wire_api="responses"`, `model_providers.openrouter.env_key="OC_PROVIDER_API_KEY"`} {
		if !strings.Contains(args, want) {
			t.Errorf("args lack %q:\n%s", want, args)
		}
	}
	if strings.Contains(args, "sk-or-test") || !strings.Contains(env, "OC_PROVIDER_API_KEY=sk-or-test") || strings.Contains(env, "OR=sk-or-test") {
		t.Errorf("the key is not only the provider's env_key:\n%s\n%s", args, env)
	}

	f = newFixture(t)
	run(t, f.executor(), request("work-l", "exec-l", f.config(map[string]any{"provider": map[string]string{"id": "local", "base_url": "http://localhost:11434/v1"}}, nil)))
	if args := f.read("call-1"); !strings.Contains(args, `model_provider="local"`) || strings.Contains(args, "env_key") {
		t.Errorf("a provider without a key:\n%s", args)
	}
}

// TestProviderValidation: a provider Codex can't use is refused with a
// message that says why, a Chat Completions wire protocol in raw overrides
// as well; with a provider, extra_args may not set its overrides.
func TestProviderValidation(t *testing.T) {
	ok := `{"provider":{"id":"openrouter","base_url":"https://openrouter.ai/api/v1","api_key_secret":"or"}}`
	if err := codex.ValidateConfig(json.RawMessage(ok), []string{"or"}); err != nil {
		t.Fatalf("valid provider refused: %v", err)
	}
	if err := codex.ValidateConfig(json.RawMessage(`{"provider":{"id":"litellm","base_url":"http://localhost:4000","wire_api":"responses"}}`), nil); err != nil {
		t.Fatalf("a keyless provider refused: %v", err)
	}
	// Without a provider, raw overrides for another provider still pass.
	if err := codex.ValidateConfig(json.RawMessage(`{"extra_args":["-c","model_provider=\"x\"","-c","model_providers.x.base_url=\"https://x\""]}`), nil); err != nil {
		t.Fatalf("raw provider overrides refused: %v", err)
	}
	for _, c := range []struct {
		config, secrets, want string
	}{
		{ok, "", "does not select"},
		{`{"provider":{"id":"OpenRouter","base_url":"https://openrouter.ai/api/v1"}}`, "", "lower-case"},
		{`{"provider":{"id":"openai","base_url":"https://api.openai.com/v1"}}`, "", "builds in"},
		{`{"provider":{"id":"or","base_url":"openrouter.ai"}}`, "", "absolute http"},
		{`{"provider":{"id":"or","base_url":"https://k:s@openrouter.ai/v1"}}`, "", "absolute http"},
		{`{"provider":{"id":"or","base_url":"https://openrouter.ai/api/v1","wire_api":"chat"}}`, "", "translating proxy"},
		{`{"provider":{"id":"or","base_url":"https://api.example.com/v1/chat/completions"}}`, "", "translating proxy"},
		{`{"provider":{"id":"or","base_url":"https://api.example.com/v1/responses"}}`, "", "not an endpoint"},
		{`{"provider":{"id":"or","base_url":"https://openrouter.ai/api/v1","wire_api":"grpc"}}`, "", "wire_api must be"},
		{`{"extra_args":["-c","model_providers.x.wire_api=\"chat\""]}`, "", "translating proxy"},
		{`{"provider":{"id":"or","base_url":"https://openrouter.ai/api/v1"},"extra_args":["-c","model_provider=\"other\""]}`, "", "set them in provider"},
		{`{"provider":{"id":"or","base_url":"https://openrouter.ai/api/v1"},"extra_args":["--config=model_providers.or.base_url=\"https://x\""]}`, "", "set them in provider"},
		{`{"provider":{"id":"or","base_url":"https://openrouter.ai/api/v1"},"env":{"OC_PROVIDER_API_KEY":"x"}}`, "", "remove it from env"},
		{`{"provider":{"id":"or","base_url":"https://openrouter.ai/api/v1","api_key_secret":"k"},"secret_env":{"k":"OPENAI_API_KEY"}}`, "k", "don't map it"},
	} {
		var secrets []string
		if c.secrets != "" {
			secrets = []string{c.secrets}
		}
		err := codex.ValidateConfig(json.RawMessage(c.config), secrets)
		if err == nil || !strings.Contains(err.Error(), c.want) || strings.Contains(err.Error(), "k:s@") {
			t.Errorf("%s: %v, want %q", c.config, err, c.want)
		}
	}
}
