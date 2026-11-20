package claudecode

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension/inproc"
	"github.com/open-conduit/open-conduit/protocol"
)

// AuthCheckName is doctor's claude-code-auth: with secrets checked, whether
// each claude-code binding's model credential works.
const AuthCheckName = "claude-code-auth"

// anthropicAPI is the API a binding without a provider or a base URL of
// its own reaches.
const anthropicAPI = "https://" + anthropicHost

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

// credStatus is each code's doctor status.
var credStatus = map[string]string{
	credOK: inproc.CheckOK, credNotChecked: inproc.CheckInfo,
	credUnreachable: inproc.CheckWarn, credUnexpected: inproc.CheckWarn, credRateLimited: inproc.CheckWarn,
	credUnauthorised: inproc.CheckFail, credNotConfigured: inproc.CheckFail, credUnresolved: inproc.CheckFail,
}

var credRank = map[string]int{inproc.CheckOK: 0, inproc.CheckInfo: 1, inproc.CheckWarn: 2, inproc.CheckFail: 3}

// credentialEnvs are the variables a credential reaches claude in, in the
// order claude prefers them.
var credentialEnvs = []string{bearerEnv, blankedEnv, "CLAUDE_CODE_OAUTH_TOKEN"}

// cloudEnvs switch claude to a cloud provider's credentials, which are the
// cloud's to verify.
var cloudEnvs = map[string]string{"CLAUDE_CODE_USE_BEDROCK": "Amazon Bedrock", "CLAUDE_CODE_USE_VERTEX": "Google Vertex AI", "CLAUDE_CODE_USE_FOUNDRY": "Microsoft Foundry"}

// authCheck is claude-code-auth, run only when doctor resolves secrets: for
// each claude-code binding, one free authenticated request with the
// credential its runs would be given, to the endpoint they would reach.
// An API key or a provider's token lists the endpoint's models; an OAuth
// token (a selected CLAUDE_CODE_OAUTH_TOKEN, or the Claude login on the
// daemon's machine) reads its subscription usage, as Claude Code's /usage
// does. A container binding is checked for the secrets it would be given,
// with no claude on the daemon's machine. Bedrock and Vertex are not
// checked.
func (x *Extension) authCheck(ctx context.Context, cfg protocol.CheckConfig) inproc.CheckResult {
	res := inproc.CheckResult{Name: AuthCheckName, Status: inproc.CheckOK, Code: credOK}
	// Every binding is tried at once, each within its own budget, so one
	// endpoint that never answers costs one budget and not the host's whole
	// deadline for the whole run (20s). Items keep the config's order.
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
	budget := x.authBudget()
	codes := make([]string, len(bindings))
	res.Items = make([]string, len(bindings))
	var wg sync.WaitGroup
	for i, j := range bindings {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cx, cancel := context.WithTimeout(ctx, budget)
			defer cancel()
			code, detail := x.credential(cx, j.scope, j.b, j.c)
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
		res.Status, res.Code, res.Detail = inproc.CheckInfo, credNotConfigured, "no claude-code bindings"
		return res
	}
	res.Status, res.Code = credStatus[worst], worst
	return res
}

// credential tries one binding's credential: its code and what was
// checked against where (never a value).
func (x *Extension) credential(ctx context.Context, scope domain.ID, b protocol.CheckBinding, c Config) (string, string) {
	secrets := map[string]string{}
	if len(b.SecretNames) > 0 {
		if x.h.BindingSecrets == nil {
			return credNotChecked, "the host can't resolve the binding's secrets"
		}
		v, err := x.h.BindingSecrets(ctx, scope, b.Name, b.SecretNames)
		if err != nil {
			return credUnresolved, "a selected secret didn't resolve"
		}
		secrets = v
	}
	vars := map[string]string{}
	if err := c.bindingVars(vars, secrets); err != nil {
		return credNotConfigured, "a selected secret maps to a reserved variable"
	}
	for _, name := range sortedKeys(cloudEnvs) {
		if v := vars[name]; v != "" && v != "0" && !strings.EqualFold(v, "false") {
			return credNotChecked, cloudEnvs[name] + " credentials are the cloud's to verify"
		}
	}
	base := strings.TrimRight(vars[endpointEnv], "/")
	if base == "" {
		base = strings.TrimRight(x.anthropicAPI(), "/")
	}
	host := endpointHost(base)
	switch {
	case vars[bearerEnv] != "":
		return x.probe(ctx, base+"/v1/models", host, "bearer token", func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+vars[bearerEnv]) })
	case vars[blankedEnv] != "":
		return x.probe(ctx, base+"/v1/models", host, "API key", func(r *http.Request) { r.Header.Set("x-api-key", vars[blankedEnv]) })
	}
	oauth, kind := vars["CLAUDE_CODE_OAUTH_TOKEN"], "OAuth token"
	if oauth == "" {
		for _, name := range credentialEnvs {
			if slices.Contains(c.PassEnv, name) {
				return credNotChecked, name + " comes from the daemon's environment through pass_env"
			}
		}
		if b.Isolation == protocol.IsolationContainer {
			return credNotConfigured, "no credential: a container binding signs in with a selected secret"
		}
		token, err := x.login(ctx, vars["CLAUDE_CONFIG_DIR"])
		if err != nil {
			return credNotConfigured, "no selected credential and no Claude login on the daemon's machine"
		}
		oauth, kind = token, "Claude login"
	}
	if vars[endpointEnv] != "" {
		return credNotChecked, "an OAuth credential sent to a base URL of the binding's own"
	}
	code, detail := x.probe(ctx, base+"/api/oauth/usage", host, kind, func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer "+oauth)
		r.Header.Set("anthropic-beta", "oauth-2025-04-20")
	})
	return code, detail
}

// probe makes one GET and classifies its answer by status alone: the
// provider's own text is never read.
func (x *Extension) probe(ctx context.Context, target, host, kind string, auth func(*http.Request)) (string, string) {
	detail := host + ", " + kind
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return credUnreachable, detail
	}
	auth(req)
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("User-Agent", "open-conduit")
	client := x.authHTTP
	if client == nil {
		client = &http.Client{}
	}
	resp, err := client.Do(req)
	if err != nil {
		return credUnreachable, detail
	}
	_ = resp.Body.Close()
	code := credCode(resp.StatusCode, strings.HasSuffix(target, "/api/oauth/usage"))
	if code == credUnexpected {
		detail += fmt.Sprintf(", HTTP %d", resp.StatusCode)
	}
	return code, detail
}

// credCode is an HTTP status's credential code. A 403 from the usage
// endpoint is a token it authenticated but whose scope can't read usage
// (a `claude setup-token` token has only inference's), so it counts as
// working; anywhere else 403 is a refused credential.
func credCode(status int, usage bool) string {
	switch {
	case status >= 200 && status < 300:
		return credOK
	case status == http.StatusForbidden && usage:
		return credOK
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return credUnauthorised
	case status == http.StatusTooManyRequests:
		return credRateLimited
	}
	return credUnexpected
}

func (x *Extension) anthropicAPI() string {
	if x.apiURL != "" {
		return x.apiURL
	}
	return anthropicAPI
}

func (x *Extension) login(ctx context.Context, dir string) (string, error) {
	if x.loginToken != nil {
		return x.loginToken(ctx, dir)
	}
	return LoginToken(ctx, dir)
}

// endpointHost is the host (and port) a URL names, for doctor's items.
func endpointHost(raw string) string {
	if u, err := url.Parse(raw); err == nil && u.Host != "" {
		return u.Host
	}
	return "the configured endpoint"
}

// probeBudget bounds one binding's credential probe, well inside the host's
// deadline for the whole doctor entry.
const probeBudget = 8 * time.Second

func (x *Extension) authBudget() time.Duration {
	if x.probeBudget > 0 {
		return x.probeBudget
	}
	return probeBudget
}
