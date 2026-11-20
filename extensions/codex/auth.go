package codex

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/protocol"
)

// AuthCheckName is doctor's codex-auth: with secrets checked, whether each
// codex binding's model credential works.
const AuthCheckName = "codex-auth"

// openAIAPI is the API a binding without a provider reaches.
const openAIAPI = "https://api.openai.com/v1"

// apiKeyVar is the variable `codex exec` signs in with when its home holds
// no login.
const apiKeyVar = "CODEX_API_KEY"

// The credential codes doctor reports for a binding. They name no value
// and carry no provider text.
const (
	credOK            = "ok"
	credUnauthorised  = "unauthorised"
	credUnreachable   = "unreachable"
	credNotConfigured = "not_configured"
	credNotChecked    = "not_checked"
	credUnresolved    = "secret_unresolved"
	credUnexpected    = "unexpected_status"
	credRateLimited   = "rate_limited"
)

var credStatus = map[string]string{
	credOK: "ok", credNotChecked: "info",
	credUnreachable: "warn", credUnexpected: "warn", credRateLimited: "warn",
	credUnauthorised: "fail", credNotConfigured: "fail", credUnresolved: "fail",
}

var credRank = map[string]int{"ok": 0, "info": 1, "warn": 2, "fail": 3}

// AuthOptions are what the credential check reaches; zero values are
// OpenAI's API and a 15-second client.
type AuthOptions struct {
	OpenAIURL string
	HTTP      *http.Client
	// Secrets resolves the secrets a binding selects (sdk.Host's
	// BindingSecrets).
	Secrets func(ctx context.Context, scope domain.ID, binding string, names []string) (map[string]string, error)
}

// CheckAuth is codex-auth, run only when doctor checks secrets: for each
// codex binding, one free authenticated request (listing the endpoint's
// models) with the key its runs would be given, to the endpoint they would
// reach: the provider's, else OpenAI's. A container binding is checked for
// the secrets it would be given, with no codex on the daemon's machine. A
// binding that signs in with `codex login` on the daemon's machine isn't
// checked: a ChatGPT sign-in has no free request to make.
func CheckAuth(ctx context.Context, cfg protocol.CheckConfig, o AuthOptions) protocol.CheckRunResult {
	res := protocol.CheckRunResult{OK: true, Status: "ok", Code: credOK}
	worst := ""
	for _, scope := range cfg.Scopes {
		for _, b := range scope.Bindings {
			if b.Executor != Name {
				continue
			}
			c, err := ParseConfig(b.Config)
			if err != nil {
				continue
			}
			cx, cancel := context.WithTimeout(ctx, 20*time.Second)
			code, detail := credential(cx, domain.ID(scope.ID), b, c, o)
			cancel()
			if worst == "" || credRank[credStatus[code]] > credRank[credStatus[worst]] {
				worst = code
			}
			res.Items = append(res.Items, fmt.Sprintf("%s/%s: %s (%s)", scope.ID, b.Name, code, detail))
		}
	}
	if worst == "" {
		res.Status, res.Code, res.Detail = "info", credNotConfigured, "no codex bindings"
		return res
	}
	res.Status, res.Code = credStatus[worst], worst
	res.OK = res.Status != "fail"
	return res
}

// credential checks one binding's credential: its code and what was
// checked against where (never a value).
func credential(ctx context.Context, scope domain.ID, b protocol.CheckBinding, c Config, o AuthOptions) (string, string) {
	secrets := map[string]string{}
	if len(b.SecretNames) > 0 {
		if o.Secrets == nil {
			return credNotChecked, "the host can't resolve the binding's secrets"
		}
		v, err := o.Secrets(ctx, scope, b.Name, b.SecretNames)
		if err != nil {
			return credUnresolved, "a selected secret didn't resolve"
		}
		secrets = v
	}
	vars := map[string]string{}
	if err := c.bindingVars(vars, secrets); err != nil {
		return credNotConfigured, "a selected secret maps to a reserved variable"
	}
	if p := c.Provider; p != nil {
		return probe(ctx, o, strings.TrimRight(p.BaseURL, "/")+"/models", vars[providerKeyVar], "provider key")
	}
	for _, arg := range c.ExtraArgs {
		if strings.Contains(arg, "model_provider") {
			return credNotChecked, "extra_args chooses the model provider"
		}
	}
	base := strings.TrimRight(vars["OPENAI_BASE_URL"], "/")
	if base == "" {
		base = o.OpenAIURL
	}
	if base == "" {
		base = openAIAPI
	}
	if key := vars[apiKeyVar]; key != "" {
		return probe(ctx, o, strings.TrimRight(base, "/")+"/models", key, apiKeyVar)
	}
	hint := ""
	if vars["OPENAI_API_KEY"] != "" {
		hint = "; codex exec doesn't read OPENAI_API_KEY, so map the key to " + apiKeyVar
	}
	if b.Isolation == protocol.IsolationContainer {
		return credNotConfigured, "no credential: a container binding signs in with a selected secret" + hint
	}
	return credNotChecked, "signs in with `codex login` on the daemon's machine, which has no free check (run `codex login status`)" + hint
}

// probe lists the endpoint's models with key as the bearer token (none
// when empty) and classifies the answer by status alone: the provider's own
// text is never read.
func probe(ctx context.Context, o AuthOptions, target, key, kind string) (string, string) {
	host := "the configured endpoint"
	if u, err := url.Parse(target); err == nil && u.Host != "" {
		host = u.Host
	}
	detail := host + ", " + kind
	if key == "" {
		detail = host + ", no key"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return credUnreachable, detail
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	req.Header.Set("User-Agent", "open-conduit")
	client := o.HTTP
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return credUnreachable, detail
	}
	_ = resp.Body.Close()
	switch s := resp.StatusCode; {
	case s >= 200 && s < 300:
		return credOK, detail
	case s == http.StatusUnauthorized || s == http.StatusForbidden:
		return credUnauthorised, detail
	case s == http.StatusTooManyRequests:
		return credRateLimited, detail
	default:
		return credUnexpected, fmt.Sprintf("%s, HTTP %d", detail, s)
	}
}
