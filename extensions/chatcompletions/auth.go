package chatcompletions

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/protocol"
)

// AuthCheckName is doctor's chat-completions-auth: with secrets checked,
// whether each chat-completions binding's API key works.
const AuthCheckName = "chat-completions-auth"

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

// AuthOptions are what the credential check reaches; a zero HTTP is a
// 15-second client.
type AuthOptions struct {
	// Budget bounds each binding's probe (default 8s); bindings are tried
	// at once.
	Budget time.Duration
	HTTP   *http.Client
	// Secrets resolves the secrets a binding selects (sdk.Host's
	// BindingSecrets).
	Secrets func(ctx context.Context, scope domain.ID, binding string, names []string) (map[string]string, error)
}

// CheckAuth is chat-completions-auth, run only when doctor checks secrets:
// for each binding, one free request listing <base_url>/models with the
// binding's api_key_secret as the bearer token (none without one) and its
// headers, the endpoint and credential its turns use. An endpoint that
// lists its models without a key reports ok for any key.
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
		res.Status, res.Code, res.Detail = "info", credNotConfigured, "no chat-completions bindings"
		return res
	}
	res.Status, res.Code = credStatus[worst], worst
	res.OK = res.Status != "fail"
	return res
}

// credential checks one binding's key: its code and what was checked
// against where (never a value).
func credential(ctx context.Context, scope domain.ID, b protocol.CheckBinding, c Config, o AuthOptions) (string, string) {
	host := "the configured endpoint"
	if u, err := url.Parse(c.BaseURL); err == nil && u.Host != "" {
		host = u.Host
	}
	key, detail := "", host+", no key"
	if c.APIKeySecret != "" {
		if o.Secrets == nil {
			return credNotChecked, "the host can't resolve the binding's secrets"
		}
		v, err := o.Secrets(ctx, scope, b.Name, []string{c.APIKeySecret})
		if err != nil || v[c.APIKeySecret] == "" {
			return credUnresolved, "api_key_secret didn't resolve"
		}
		key, detail = v[c.APIKeySecret], host+", api_key_secret"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/models", nil)
	if err != nil {
		return credUnreachable, detail
	}
	for name, value := range c.Headers {
		req.Header.Set(name, value)
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
