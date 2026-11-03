package bitbucket

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/extension/inproc"
	"github.com/open-conduit/open-conduit/extensions/tracker"
	"github.com/open-conduit/open-conduit/protocol"
)

// Bitbucket Cloud as an in-process extension, composed by the host through
// the public contract alone (#357): each enabled scope's entry registers an
// ingress pair (webhook push when it names a webhook_secret, and
// reconcile), its route handler selects input, its tools join every
// generation's toolbelt, and a manage_webhook entry has its repository
// webhook pointed at the public URL. Pull requests of a reopened issue key
// the issue's generation from the Work the tracker entry the entry names
// keeps in the scope (StoreGenerations).

type source struct {
	name    string
	scope   domain.ID
	config  json.RawMessage
	src     Source
	ingress *Ingress
}

// Extension is the Bitbucket extension.
type Extension struct {
	h     inproc.Host
	tools *Tools

	mu      sync.Mutex
	sources map[string]*source // by namespace, for the daemon's lifetime
	failed  map[string]string
	public  string
	hooks   tracker.Hooks
	codes   map[string]string
}

// New returns the Bitbucket extension.
func New() *Extension {
	return &Extension{sources: map[string]*source{}, failed: map[string]string{}, codes: map[string]string{}}
}

var (
	_ inproc.Typed      = (*Extension)(nil)
	_ inproc.Operator   = (*Extension)(nil)
	_ inproc.Declarer   = (*Extension)(nil)
	_ inproc.Router     = (*Extension)(nil)
	_ inproc.Starter    = (*Extension)(nil)
	_ inproc.Configurer = (*Extension)(nil)
	_ inproc.Ingester   = (*Extension)(nil)
	_ inproc.Webhooker  = (*Extension)(nil)
	_ inproc.Tooled     = (*Extension)(nil)
	_ inproc.Tasker     = (*Extension)(nil)
	_ inproc.Checker    = (*Extension)(nil)
)

func (*Extension) Name() string { return Type }

func (*Extension) Types() []inproc.ConfigType {
	return []inproc.ConfigType{
		{Kind: protocol.ValidateConnection, Type: Type, Validate: func(c json.RawMessage, _ []string) error { return ValidateConnection(c) }, Fields: connectionFields()},
		{Kind: protocol.ValidateExtension, Type: Type, Ingress: true, Validate: ValidateExtension, Fields: entryFields()},
	}
}

func (*Extension) Operations() []string { return Operations() }

func (*Extension) Declarations() []extension.Operation { return Declared() }

func (*Extension) Routes() map[string]extension.RouteHandler {
	return map[string]extension.RouteHandler{Type: Route{}}
}

func (x *Extension) Start(_ context.Context, h inproc.Host) error {
	x.h = h
	tools, err := NewTools(ToolsOptions{Store: h.Store, Sources: x.toolSources, Now: x.now})
	if err != nil {
		return err
	}
	x.tools = tools
	return nil
}

func (x *Extension) Registration() extension.Registration {
	if x.tools == nil {
		return extension.Registration{}
	}
	return x.tools.Registration()
}

func (x *Extension) now() time.Time {
	if x.h.Now != nil {
		return x.h.Now()
	}
	return time.Now()
}

func (x *Extension) report(task, code string) {
	x.mu.Lock()
	x.codes[task] = code
	var codes []string
	for _, t := range []string{"register", "webhook"} {
		if c := x.codes[t]; c != "" {
			codes = append(codes, c)
		}
	}
	x.mu.Unlock()
	if x.h.Report != nil {
		x.h.Report(strings.Join(codes, ","))
	}
}

// Configure registers every enabled scope's entry not yet registered; a
// changed entry reports restart_required.
func (x *Extension) Configure(_ context.Context, p protocol.ConfigureParams) {
	restart := false
	for _, e := range p.Entries {
		if e.Type != Type {
			continue
		}
		st, err := ParseExtension(e.Config, e.Secrets)
		if err != nil || st.Scope != e.ScopeID {
			continue
		}
		x.mu.Lock()
		prev, ok := x.sources[st.Namespace]
		x.mu.Unlock()
		if ok {
			restart = restart || string(prev.config) != string(e.Config)
			continue
		}
		code := ""
		if err := x.add(e, st); err != nil {
			var named tracker.CodeError
			if code = tracker.ErrorCode(err); errors.As(err, &named) {
				code = string(named)
			}
		}
		x.mu.Lock()
		if code != "" {
			x.failed[st.Namespace] = code
		} else {
			delete(x.failed, st.Namespace)
		}
		x.mu.Unlock()
	}
	x.mu.Lock()
	x.public = p.PublicURL
	var failed []string
	for _, c := range x.failed {
		failed = append(failed, c)
	}
	x.mu.Unlock()
	code := ""
	if restart {
		code = "restart_required"
	} else if len(failed) > 0 {
		sort.Strings(failed)
		code = failed[0]
	}
	x.report("register", code)
}

func (x *Extension) add(e protocol.ConfigEntry, st Settings) error {
	if e.Connection == nil {
		return tracker.CodeError(Type + "_connection_required")
	}
	cfg, err := ParseConnection(e.Connection.Config)
	if err != nil {
		return tracker.CodeError("bitbucket_connection_invalid")
	}
	if x.h.DryRun {
		st.DryRun = true
	}
	scope, conn := domain.ID(e.ScopeID), e.Connection.ID
	secrets := func(ctx context.Context, name string) (string, error) {
		values, err := x.h.Secrets(ctx, scope, conn, []string{name})
		if err != nil {
			return "", tracker.ErrCredential
		}
		return values[name], nil
	}
	client := NewClient(cfg, secrets, ClientOptions{Now: x.now})
	var gens Generations
	if ns := st.TrackerNS; ns != "" {
		gens = StoreGenerations(x.h.Store, scope, ns, x.now)
	}
	s := &source{name: Type + "/" + e.ScopeID + "/" + e.Name, scope: scope, config: append(json.RawMessage(nil), e.Config...),
		src:     Source{Settings: st, Client: client, Outbox: NewOutbox(x.h.Store, st, x.now)},
		ingress: NewIngress(st, client, x.now, IngressOptions{Generations: gens})}
	x.mu.Lock()
	x.sources[st.Namespace] = s
	x.mu.Unlock()
	return nil
}

func (x *Extension) list() []*source {
	x.mu.Lock()
	defer x.mu.Unlock()
	out := make([]*source, 0, len(x.sources))
	for _, s := range x.sources {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

func (x *Extension) toolSources() []Source {
	var out []Source
	for _, s := range x.list() {
		out = append(out, s.src)
	}
	return out
}

func (x *Extension) Sources() []inproc.Source {
	var out []inproc.Source
	for _, s := range x.list() {
		out = append(out, inproc.Source{Name: s.name, Source: extension.Source{Namespace: s.src.Settings.Namespace, ScopeID: s.scope}, Ingress: s.ingress})
	}
	return out
}

// Webhook serves an entry's webhook (POST /bitbucket/<scope>/<entry>) when
// it names a webhook_secret.
func (x *Extension) Webhook(path string) http.Handler {
	for _, s := range x.list() {
		if s.name == Type+"/"+path && s.src.Settings.WebhookSecret != "" {
			src := s
			push := func(ctx context.Context, d extension.Delivery) error { return x.h.Deliver(ctx, src.name, d) }
			return WebhookHandler(src.src.Settings, x.secret(src), push, x.now)
		}
	}
	return nil
}

func (x *Extension) secret(s *source) func(context.Context) (string, error) {
	return func(ctx context.Context) (string, error) {
		name := s.src.Settings.WebhookSecret
		if name == "" || x.h.Secrets == nil {
			return "", tracker.ErrCredential
		}
		values, err := x.h.Secrets(ctx, s.scope, "", []string{name})
		if err != nil || values[name] == "" {
			return "", tracker.ErrCredential
		}
		return values[name], nil
	}
}

// Tasks: the managed webhooks pointed at the public URL, a failure retried
// after a minute.
func (x *Extension) Tasks() []inproc.Task {
	return []inproc.Task{{Name: "webhooks", Pace: time.Second, Run: x.manageHooks}}
}

func (x *Extension) manageHooks(ctx context.Context) error {
	x.mu.Lock()
	public := x.public
	x.mu.Unlock()
	if public == "" {
		return nil
	}
	code := ""
	for _, s := range x.list() {
		st := s.src.Settings
		target := public + "/" + s.name
		if !st.ManageWebhook || st.WebhookSecret == "" || !x.hooks.Due(st.Namespace, target, x.now()) {
			continue
		}
		cx, cancel := context.WithTimeout(ctx, 30*time.Second)
		existing := x.hooks.ID(st.Namespace)
		id, c := existing, "credential_unavailable"
		if secret, err := x.secret(s)(cx); err == nil {
			dry := x.h.DryRun || st.DryRun
			if id, err = s.src.Client.EnsureWebhook(cx, st, target, secret, existing, dry); err != nil {
				id, c = existing, tracker.HookCode(err, nil)
			} else if dry {
				c = "dry_run"
			} else {
				c = "ok"
			}
		}
		cancel()
		x.hooks.Set(st.Namespace, target, id, c, x.now())
		if c != "ok" && c != "dry_run" {
			code = c
		}
	}
	x.report("webhook", code)
	return nil
}

func (x *Extension) Checks() []inproc.Check {
	return []inproc.Check{{Run: x.check}, {Deep: true, Run: x.authCheck}}
}

// check reports registration, rate-limit, tracker link and webhook state
// per entry. Reconcile health is the host's reconcile_lag check.
func (x *Extension) check(context.Context, protocol.CheckConfig) inproc.CheckResult {
	check := inproc.CheckResult{Name: Type, Status: inproc.CheckOK, Code: "ok"}
	x.mu.Lock()
	failed := map[string]string{}
	for ns, c := range x.failed {
		failed[ns] = c
	}
	x.mu.Unlock()
	sources := x.list()
	if len(sources) == 0 && len(failed) == 0 {
		check.Status, check.Code, check.Detail = inproc.CheckInfo, "not_configured", "no bitbucket entries are configured"
		return check
	}
	for ns, code := range failed {
		tracker.Raise(&check, inproc.CheckFail, code)
		check.Items = append(check.Items, ns+": not registered ("+code+")")
	}
	for _, s := range sources {
		st := s.src.Settings
		var notes []string
		if until, on := s.src.Client.PausedUntil(); on {
			tracker.Raise(&check, inproc.CheckWarn, "rate_limited")
			notes = append(notes, "rate limited until "+until.UTC().Format(time.RFC3339))
		}
		notes = append(notes, x.hooks.Item(&check, st.Namespace, s.name, st.WebhookSecret != "", st.ManageWebhook))
		if x.h.DryRun || st.DryRun {
			notes = append(notes, "dry run (no bitbucket writes)")
		}
		check.Items = append(check.Items, st.Namespace+": "+strings.Join(notes, "; "))
	}
	sort.Strings(check.Items)
	return check
}

// authCheck (check_secrets only) signs each entry in.
func (x *Extension) authCheck(ctx context.Context, _ protocol.CheckConfig) inproc.CheckResult {
	check := inproc.CheckResult{Name: Type + "_auth", Status: inproc.CheckOK, Code: "verified"}
	sources := x.list()
	if len(sources) == 0 {
		check.Status, check.Code = inproc.CheckInfo, "not_configured"
		return check
	}
	for _, s := range sources {
		cx, cancel := context.WithTimeout(ctx, 20*time.Second)
		who, err := s.src.Client.Verify(cx)
		cancel()
		check.Items = append(check.Items, tracker.VerifyItem(&check, s.src.Settings.Namespace, who, err))
	}
	sort.Strings(check.Items)
	return check
}
