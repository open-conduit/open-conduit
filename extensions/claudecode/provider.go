package claudecode

import (
	"net"
	"net/url"
	"strings"

	"github.com/open-conduit/open-conduit/extension"
)

// Provider points a binding's turns at a model endpoint other than
// Anthropic's: an Anthropic-compatible gateway such as OpenRouter, LiteLLM
// or Ollama. The executor sets Claude Code's environment from it, so a
// binding never writes ANTHROPIC_BASE_URL or ANTHROPIC_AUTH_TOKEN itself.
type Provider struct {
	// BaseURL is the endpoint, set as ANTHROPIC_BASE_URL: an absolute
	// http or https URL with a host and no credentials, query or fragment.
	BaseURL string `json:"base_url"`
	// AuthTokenSecret is the selected secret sent as the bearer token
	// (ANTHROPIC_AUTH_TOKEN). Without it the token is a placeholder, for a
	// local endpoint that needs none.
	AuthTokenSecret string `json:"auth_token_secret,omitempty"`
}

// The variables a provider sets. A binding with a provider may not set
// them through env or secret_env.
const (
	baseURLVar   = "ANTHROPIC_BASE_URL"
	authTokenVar = "ANTHROPIC_AUTH_TOKEN"
	apiKeyVar    = "ANTHROPIC_API_KEY"
)

var providerVars = []string{baseURLVar, authTokenVar, apiKeyVar}

// placeholderToken is the bearer token a provider without a secret sends.
// Claude Code always sends one; a set token also keeps it from falling back
// to the binding's Claude login and sending that to another endpoint.
const placeholderToken = "open-conduit"

// anthropicHost is the API host a binding without a provider reaches.
const anthropicHost = "api.anthropic.com"

// checkProvider adds what is wrong with c's provider to ce.
func (c Config) checkProvider(ce *extension.ConfigError) {
	p := c.Provider
	if p == nil {
		return
	}
	if strings.ContainsRune(p.BaseURL, 0) || strings.ContainsRune(p.AuthTokenSecret, 0) {
		ce.Add("/provider", "invalid_string", "strings must not contain NUL")
		return
	}
	if _, ok := providerHost(p.BaseURL); !ok {
		ce.Add("/provider/base_url", "invalid_base_url", "provider.base_url must be an absolute http or https URL with a host, and no credentials, query or fragment")
	}
	for _, name := range sortedKeys(c.Env) {
		if isProviderVar(name) {
			ce.Add(extension.ConfigPointer("env", name), "provider_env", "provider sets "+name+" itself; remove it from env")
		}
	}
	for _, secret := range sortedKeys(c.SecretEnv) {
		if isProviderVar(c.SecretEnv[secret]) {
			ce.Add(extension.ConfigPointer("secret_env", secret), "provider_env", "provider sets "+c.SecretEnv[secret]+" itself; name the secret in provider.auth_token_secret instead")
		} else if secret == p.AuthTokenSecret {
			ce.Add(extension.ConfigPointer("secret_env", secret), "provider_env", "provider.auth_token_secret is sent as the bearer token; don't map it in secret_env as well")
		}
	}
}

func isProviderVar(name string) bool {
	for _, v := range providerVars {
		if strings.EqualFold(v, name) {
			return true
		}
	}
	return false
}

// providerHost is the outbound host a base URL needs (host, or host:port
// when the URL names a port), and whether the URL is acceptable.
func providerHost(raw string) (string, bool) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" {
		return "", false
	}
	if port := u.Port(); port != "" {
		return net.JoinHostPort(u.Hostname(), port), true
	}
	return u.Hostname(), true
}

// apiHost is the host a binding's turns reach: its provider's, else
// Anthropic's.
func (c Config) apiHost() string {
	if c.Provider != nil {
		if h, ok := providerHost(c.Provider.BaseURL); ok {
			return h
		}
	}
	return anthropicHost
}

// applyProvider sets the provider's variables in vars, over every earlier
// layer: the base URL, the bearer token (the selected secret, else the
// placeholder) and an empty API key, so neither an API key nor the Claude
// login is sent to the provider.
func (c Config) applyProvider(vars map[string]string, secrets map[string]string) {
	p := c.Provider
	if p == nil {
		return
	}
	vars[baseURLVar] = p.BaseURL
	vars[apiKeyVar] = ""
	vars[authTokenVar] = placeholderToken
	if v := secrets[p.AuthTokenSecret]; p.AuthTokenSecret != "" && v != "" {
		vars[authTokenVar] = v
	}
}
