package codex

import (
	"net/url"
	"regexp"
	"slices"
	"strings"
)

// Provider is a model provider other than Codex's default for a binding's
// turns. The executor writes Codex's model_provider and
// model_providers.<id> overrides from it, so a binding never hand-writes
// them in extra_args.
type Provider struct {
	// ID names the provider in Codex's config: lower-case letters, digits,
	// dashes and underscores, and not one Codex builds in.
	ID string `json:"id"`
	// BaseURL is the API's base, such as https://openrouter.ai/api/v1: an
	// absolute http or https URL with a host and no credentials, query or
	// fragment, and not an endpoint under it.
	BaseURL string `json:"base_url"`
	// APIKeySecret is the selected secret Codex sends as the bearer token,
	// through its env_key. Leave it out for an endpoint that needs none.
	APIKeySecret string `json:"api_key_secret,omitempty"`
	// WireAPI is the protocol Codex speaks to it: "responses", the only
	// one Codex still supports (and the default).
	WireAPI string `json:"wire_api,omitempty"`
}

// providerKeyVar is the variable the provider's key is passed in, named
// as the provider's env_key. A binding with a provider may not set it.
const providerKeyVar = "OC_PROVIDER_API_KEY"

// wireResponses is the only wire protocol Codex speaks.
const wireResponses = "responses"

var providerID = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// builtinProviders are the provider IDs Codex defines itself; its config
// can't replace them, so a provider with one of these IDs would be ignored.
var builtinProviders = []string{"openai", "ollama", "lmstudio"}

// Messages for a provider Codex can't use. They name no configured value.
const (
	errChatWire = "Codex speaks only the Responses API; a provider that offers only Chat Completions needs a translating proxy such as LiteLLM in front of it, and that proxy's URL as base_url"
	errEndpoint = "provider.base_url must be the API's base (such as https://openrouter.ai/api/v1), not an endpoint under it"
)

// checkProvider reports what is wrong with c's provider, "" when nothing.
func (c Config) checkProvider() string {
	p := c.Provider
	if p == nil {
		return ""
	}
	for _, s := range []string{p.ID, p.BaseURL, p.APIKeySecret, p.WireAPI} {
		if strings.ContainsRune(s, 0) {
			return "strings must not contain NUL"
		}
	}
	switch {
	case !providerID.MatchString(p.ID):
		return "provider.id must be lower-case letters, digits, dashes and underscores"
	case slices.Contains(builtinProviders, p.ID):
		return "provider.id must not be one Codex builds in (" + strings.Join(builtinProviders, ", ") + "), which its config can't replace; choose another id"
	case p.WireAPI == "chat":
		return errChatWire
	case p.WireAPI != "" && p.WireAPI != wireResponses:
		return "provider.wire_api must be responses, or left out"
	}
	u, err := url.Parse(p.BaseURL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" {
		return "provider.base_url must be an absolute http or https URL with a host, and no credentials, query or fragment"
	}
	path := strings.TrimRight(u.Path, "/")
	switch {
	case strings.HasSuffix(path, "/chat/completions"):
		return errChatWire
	case strings.HasSuffix(path, "/responses"):
		return errEndpoint
	}
	if _, ok := c.Env[providerKeyVar]; ok {
		return "provider sets " + providerKeyVar + " itself; remove it from env"
	}
	for secret, name := range c.SecretEnv {
		if name == providerKeyVar || secret == p.APIKeySecret {
			return "provider.api_key_secret is passed to Codex itself; don't map it in secret_env"
		}
	}
	return ""
}

// providerKey reports whether a selected secret is the provider's key,
// which only the provider's variable carries.
func (c Config) providerKey(secret string) bool {
	return c.Provider != nil && c.Provider.APIKeySecret != "" && secret == c.Provider.APIKeySecret
}

// providerArgs are the -c overrides that select c's provider: the
// provider itself, its base URL, the Responses wire protocol and, with a
// key, the variable the key is in.
func (c Config) providerArgs() []string {
	p := c.Provider
	if p == nil {
		return nil
	}
	prefix := "model_providers." + p.ID + "."
	args := []string{
		"-c", "model_provider=" + tomlString(p.ID),
		"-c", prefix + "name=" + tomlString(p.ID),
		"-c", prefix + "base_url=" + tomlString(p.BaseURL),
		"-c", prefix + "wire_api=" + tomlString(wireResponses),
	}
	if p.APIKeySecret != "" {
		args = append(args, "-c", prefix+"env_key="+tomlString(providerKeyVar))
	}
	return args
}

// overrideConflict reports what is wrong with one extra_args -c override
// (its key and value): one that names a Chat Completions wire protocol,
// which Codex no longer speaks, or, with a provider, one that sets the
// provider or its settings.
func (c Config) overrideConflict(key, value string) string {
	if strings.HasPrefix(key, "model_providers.") && strings.HasSuffix(key, ".wire_api") && strings.Contains(value, "chat") {
		return errChatWire
	}
	if c.Provider != nil && (key == "model_provider" || key == "model_providers" || key == "model_providers."+c.Provider.ID || strings.HasPrefix(key, "model_providers."+c.Provider.ID+".")) {
		return "extra_args may not set model_provider or the provider's model_providers settings when provider is set; set them in provider"
	}
	return ""
}
