package codex

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
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
	// Budget bounds each binding's probe (default 8s); bindings are tried
	// at once.
	Budget    time.Duration
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
	// Every binding is tried at once, each within its own budget, so one
	// endpoint that never answers costs one budget and not the host's whole
	// deadline for check.run (20s). Items keep the config's order.
	type binding struct {
		scope domain.ID
		b     protocol.CheckBinding
		c     Config
	}
	var bindings []binding
	for _, scope := range cfg.Scopes {
		for _, b := range scope.Bindings {
			if b.Executor != Name {
				continue
			}
			if c, err := ParseConfig(b.Config); err == nil {
				bindings = append(bindings, binding{domain.ID(scope.ID), b, c})
			}
		}
	}
	budget := o.budget()
	codes := make([]string, len(bindings))
	res.Items = make([]string, len(bindings))
	var wg sync.WaitGroup
	for i, j := range bindings {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cx, cancel := context.WithTimeout(ctx, budget)
			defer cancel()
			code, detail := credential(cx, j.scope, j.b, j.c, o)
			codes[i] = code
			res.Items[i] = fmt.Sprintf("%s/%s: %s (%s)", j.scope, j.b.Name, code, detail)
		}()
	}
	wg.Wait()
	worst := ""
	for _, code := range codes {
		if worst == "" || credRank[credStatus[code]] > credRank[credStatus[worst]] {
			worst = code
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
		client = &http.Client{}
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

// probeBudget bounds one binding's credential probe, well inside the host's
// deadline for the whole check.
const probeBudget = 8 * time.Second

func (o AuthOptions) budget() time.Duration {
	if o.Budget > 0 {
		return o.Budget
	}
	return probeBudget
}
