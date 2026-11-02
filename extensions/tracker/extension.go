package tracker

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
	"github.com/open-conduit/open-conduit/extensions/tracker/playbooks"
	"github.com/open-conduit/open-conduit/protocol"
)

// A tracker (Linear, Jira) as an in-process extension, composed by the
// host through the public contract alone (#357). Each enabled scope's entry
// registers an ingress (webhook push on the listener when it names a
// webhook_secret, and reconcile), its route handler selects input, the
// projector converges claim/status labels, workflow transitions and marked
// comments, the tools join every generation's toolbelt, and a manage_webhook
// entry has its service webhook pointed at the public URL. Labels and
// transitions stay in the kit: nothing here is a tracker branch in the core.

// Kind is what one tracker type gives the kit: its type, validators and how
// an entry opens its service.
type Kind struct {
	// Type is the extension name, entry, connection and route type, and
	// event family and operation prefix.
	Type string
	// ValidateConnection and ValidateExtension check config at load.
	ValidateConnection func(json.RawMessage) error
	ValidateExtension  func(json.RawMessage, []string) error
	// Open builds an entry's settings and service from its connection's
	// config and its own; dryRun forces a dry projection. A *CodeError
	// names a fixed diagnostic.
	Open func(conn, entry json.RawMessage, secretNames []string, secrets SecretFunc, now func() time.Time, dryRun bool) (Settings, Service, error)
	// HookPermission is the service's error for a webhook it may not
	// manage, reported as hook_permission_denied.
	HookPermission error
}

// CodeError is a fixed, secret-free registration diagnostic.
type CodeError string

func (e CodeError) Error() string { return string(e) }

// reopens is the in-process registry of the tracker entries' services by
// namespace, through which a code host (Bitbucket) keys a pull request to
// a reopened issue's generation. Both link the kit; a tracker in another
// process is not found, and the code host keys the first generation.
var reopens = struct {
	sync.Mutex
	m map[string]Service
}{m: map[string]Service{}}

// Generation resolves a tracker issue's reopen generation at a time through
// the tracker entry registered for namespace ns. ok is false when no such
// entry is registered in this process.
func Generation(ctx context.Context, ns, key string, at time.Time) (gen string, ok bool, err error) {
	reopens.Lock()
	svc := reopens.m[ns]
	reopens.Unlock()
	if svc == nil {
		return "", false, nil
	}
	r, err := svc.Reopens(ctx, key)
	if err != nil {
		return "", true, err
	}
	return GenerationAt(r, at), true, nil
}

// Registered reports whether a tracker entry of namespace ns is registered
// in this process.
func Registered(ns string) bool {
	reopens.Lock()
	defer reopens.Unlock()
	return reopens.m[ns] != nil
}

// source is one registered entry.
type source struct {
	name      string
	scope     domain.ID
	config    json.RawMessage
	settings  Settings
	service   Service
	ingress   *Ingress
	projector *Projector
}

// Extension is one tracker type's in-process extension.
type Extension struct {
	k     Kind
	h     inproc.Host
	tools *Tools

	mu      sync.Mutex
	sources map[string]*source // by namespace, for the daemon's lifetime
	failed  map[string]string  // registration failure code by namespace
	public  string
	hooks   Hooks
	codes   map[string]string
	next    int // the entry the next projection pass starts at
}

// NewExtension returns the extension of one tracker kind.
func NewExtension(k Kind) *Extension {
	return &Extension{k: k, sources: map[string]*source{}, failed: map[string]string{}, codes: map[string]string{}}
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
	_ inproc.Playbooker = (*Extension)(nil)
	_ inproc.Stopper    = (*Extension)(nil)
)

func (x *Extension) Name() string { return x.k.Type }

func (x *Extension) Types() []inproc.ConfigType {
	return []inproc.ConfigType{
		{Kind: protocol.ValidateConnection, Type: x.k.Type, Validate: func(c json.RawMessage, _ []string) error { return x.k.ValidateConnection(c) }},
		{Kind: protocol.ValidateExtension, Type: x.k.Type, Ingress: true, Validate: x.k.ValidateExtension},
		{Kind: protocol.ValidateRoute, Type: x.k.Type, Validate: func(c json.RawMessage, _ []string) error { return ValidateRoute(c) }},
	}
}

func (x *Extension) Operations() []string { return Operations(x.k.Type) }

func (x *Extension) Declarations() []extension.Operation { return Declared(x.k.Type) }

func (x *Extension) Routes() map[string]extension.RouteHandler {
	return map[string]extension.RouteHandler{x.k.Type: Route{Type: x.k.Type}}
}

// Playbooks are the tracker playbooks every tracker shares.
func (x *Extension) Playbooks() map[string]string { return playbooks.All() }

func (x *Extension) Start(_ context.Context, h inproc.Host) error {
	x.h = h
	tools, err := NewTools(ToolsOptions{Type: x.k.Type, Store: h.Store, Sources: x.toolSources, Now: x.now})
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
	for _, t := range []string{"register", "projection", "webhook"} {
		if c := x.codes[t]; c != "" {
			codes = append(codes, c)
		}
	}
	x.mu.Unlock()
	if x.h.Report != nil {
		x.h.Report(strings.Join(codes, ","))
	}
}

// Configure registers every enabled scope's entry not yet registered.
// Registrations are immutable for one runtime instance: a changed entry
// reports restart_required.
func (x *Extension) Configure(ctx context.Context, p protocol.ConfigureParams) {
	var restart []string
	for _, e := range p.Entries {
		if e.Type != x.k.Type {
			continue
		}
		var common struct {
			Namespace, Scope string
		}
		if json.Unmarshal(e.Config, &common) != nil || common.Scope != e.ScopeID || x.k.ValidateExtension(e.Config, e.Secrets) != nil {
			continue
		}
		x.mu.Lock()
		prev, ok := x.sources[common.Namespace]
		x.mu.Unlock()
		if ok {
			if string(prev.config) != string(e.Config) {
				restart = append(restart, common.Namespace)
			}
			continue
		}
		code := ""
		if err := x.add(e, common.Namespace); err != nil {
			var named CodeError
			if code = ErrorCode(err); errors.As(err, &named) {
				code = string(named)
			}
		}
		x.mu.Lock()
		if code != "" {
			x.failed[common.Namespace] = code
		} else {
			delete(x.failed, common.Namespace)
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
	if len(restart) > 0 {
		code = "restart_required"
	} else if len(failed) > 0 {
		sort.Strings(failed)
		code = failed[0]
	}
	x.report("register", code)
}

func (x *Extension) add(e protocol.ConfigEntry, ns string) error {
	if e.Connection == nil {
		return CodeError(x.k.Type + "_connection_required")
	}
	scope, conn := domain.ID(e.ScopeID), e.Connection.ID
	secrets := func(ctx context.Context, name string) (string, error) {
		values, err := x.h.Secrets(ctx, scope, conn, []string{name})
		if err != nil {
			return "", ErrCredential
		}
		return values[name], nil
	}
	settings, service, err := x.k.Open(e.Connection.Config, e.Config, e.Secrets, secrets, x.now, x.h.DryRun)
	if err != nil {
		return err
	}
	projector, err := NewProjector(ProjectorOptions{Store: x.h.Store, Service: service, Settings: settings, Now: x.now})
	if err != nil {
		return err
	}
	src := &source{name: x.k.Type + "/" + e.ScopeID + "/" + e.Name, scope: scope, config: append(json.RawMessage(nil), e.Config...),
		settings: settings, service: service, ingress: NewIngress(settings, service, x.now, IngressOptions{}), projector: projector}
	x.mu.Lock()
	x.sources[ns] = src
	x.mu.Unlock()
	reopens.Lock()
	reopens.m[ns] = service
	reopens.Unlock()
	return nil
}

// Stop withdraws the extension's entries from the reopen registry, each
// only while the registry still holds its own service, so a code host
// stops finding a stopped daemon's tracker.
func (x *Extension) Stop() {
	x.mu.Lock()
	defer x.mu.Unlock()
	reopens.Lock()
	defer reopens.Unlock()
	for ns, s := range x.sources {
		if reopens.m[ns] == s.service {
			delete(reopens.m, ns)
		}
	}
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
		out = append(out, Source{Settings: s.settings, Service: s.service, Projector: s.projector})
	}
	return out
}

func (x *Extension) Sources() []inproc.Source {
	var out []inproc.Source
	for _, s := range x.list() {
		out = append(out, inproc.Source{Name: s.name, Source: extension.Source{Namespace: s.settings.Namespace, ScopeID: s.scope}, Ingress: s.ingress})
	}
	return out
}

// Webhook serves an entry's webhook (POST /<type>/<scope>/<entry>) when it
// names a webhook_secret.
func (x *Extension) Webhook(path string) http.Handler {
	for _, s := range x.list() {
		if s.name == x.k.Type+"/"+path && s.settings.WebhookSecret != "" {
			src := s
			push := func(ctx context.Context, d extension.Delivery) error { return x.h.Deliver(ctx, src.name, d) }
			return WebhookHandler(x.k.Type, src.service, x.secret(src), push, x.now)
		}
	}
	return nil
}

// secret resolves an entry's webhook_secret among its selected secrets.
func (x *Extension) secret(s *source) func(context.Context) (string, error) {
	return func(ctx context.Context) (string, error) {
		name := s.settings.WebhookSecret
		if name == "" || x.h.Secrets == nil {
			return "", ErrCredential
		}
		values, err := x.h.Secrets(ctx, s.scope, "", []string{name})
		if err != nil || values[name] == "" {
			return "", ErrCredential
		}
		return values[name], nil
	}
}

// Tasks: a bounded projection pass every few seconds, and the managed
// webhooks pointed at the public URL, a failure retried after a minute.
func (x *Extension) Tasks() []inproc.Task {
	return []inproc.Task{
		{Name: "projection", Pace: 5 * time.Second, Run: x.project},
		{Name: "webhooks", Pace: time.Second, Run: x.manageHooks},
	}
}

const passLimit = 100

// project converges the entries in turn, starting after the one the last
// pass reached, so a pass its deadline cuts short never starves the later
// entries; one the deadline interrupted keeps its last code.
func (x *Extension) project(ctx context.Context) error {
	sources := x.list()
	x.mu.Lock()
	start := x.next
	x.mu.Unlock()
	for i := range sources {
		k := (start + i) % len(sources)
		s := sources[k]
		if ctx.Err() != nil {
			break
		}
		res, err := s.projector.Converge(ctx, passLimit)
		code := strings.Join(res.Codes, ",")
		if err != nil && code == "" {
			code = ErrorCode(err)
		}
		if ctx.Err() == nil {
			x.hooks.SetProjection(s.settings.Namespace, code)
		}
		x.mu.Lock()
		x.next = k + 1
		x.mu.Unlock()
	}
	var codes []string
	for _, s := range sources {
		if c := x.hooks.Projection(s.settings.Namespace); c != "" {
			codes = append(codes, c)
		}
	}
	x.report("projection", strings.Join(codes, ","))
	return nil
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
		target := public + "/" + s.name
		if !s.settings.ManageWebhook || s.settings.WebhookSecret == "" || !x.hooks.Due(s.settings.Namespace, target, x.now()) {
			continue
		}
		cx, cancel := context.WithTimeout(ctx, 30*time.Second)
		existing := x.hooks.ID(s.settings.Namespace)
		id, c := x.ensureHook(cx, s, target, existing)
		cancel()
		x.hooks.Set(s.settings.Namespace, target, id, c, x.now())
		if c != "ok" && c != "dry_run" {
			code = c
		}
	}
	x.report("webhook", code)
	return nil
}

func (x *Extension) ensureHook(ctx context.Context, s *source, url, existing string) (string, string) {
	secret, err := x.secret(s)(ctx)
	if err != nil {
		return existing, "credential_unavailable"
	}
	if x.h.DryRun || s.settings.Projection.DryRun {
		if existing == "" {
			existing = "dry-run"
		}
		return existing, "dry_run"
	}
	id, err := s.service.EnsureWebhook(ctx, url, secret, existing)
	if err != nil {
		return existing, HookCode(err, x.k.HookPermission)
	}
	return id, "ok"
}

// HookCode is the diagnostic of a failed webhook update.
func HookCode(err, permission error) string {
	var limit *RateLimitError
	switch {
	case errors.Is(err, ErrCredential), permission != nil && errors.Is(err, permission), IsStatus(err, http.StatusForbidden):
		return "hook_permission_denied"
	case errors.As(err, &limit):
		return "rate_limited"
	}
	return "hook_update_failed"
}

type paused interface {
	PausedUntil() (time.Time, bool)
}

func (x *Extension) Checks() []inproc.Check {
	return []inproc.Check{{Run: x.check}, {Deep: true, Run: x.authCheck}}
}

// check reports registration, rate-limit, projection and webhook state per
// entry. Reconcile health is the host's reconcile_lag check. It makes no
// service calls.
func (x *Extension) check(context.Context, protocol.CheckConfig) inproc.CheckResult {
	check := inproc.CheckResult{Name: x.k.Type, Status: inproc.CheckOK, Code: "ok"}
	x.mu.Lock()
	failed := map[string]string{}
	for ns, c := range x.failed {
		failed[ns] = c
	}
	x.mu.Unlock()
	sources := x.list()
	if len(sources) == 0 && len(failed) == 0 {
		check.Status, check.Code, check.Detail = inproc.CheckInfo, "not_configured", "no "+x.k.Type+" entries are configured"
		return check
	}
	for ns, code := range failed {
		Raise(&check, inproc.CheckFail, code)
		check.Items = append(check.Items, ns+": not registered ("+code+")")
	}
	for _, s := range sources {
		var notes []string
		if p, ok := s.service.(paused); ok {
			if until, on := p.PausedUntil(); on {
				Raise(&check, inproc.CheckWarn, "rate_limited")
				notes = append(notes, "rate limited until "+until.UTC().Format(time.RFC3339))
			}
		}
		if code := x.hooks.Projection(s.settings.Namespace); code != "" {
			Raise(&check, inproc.CheckWarn, "projection_failing")
			notes = append(notes, "projection "+code)
		}
		notes = append(notes, x.hooks.Item(&check, s.settings.Namespace, s.name, s.settings.WebhookSecret != "", s.settings.ManageWebhook))
		if x.h.DryRun || s.settings.Projection.DryRun {
			notes = append(notes, "dry run (no "+x.k.Type+" writes)")
		}
		check.Items = append(check.Items, s.settings.Namespace+": "+strings.Join(notes, "; "))
	}
	sort.Strings(check.Items)
	return check
}

// authCheck (check_secrets only) signs each entry in: the connection test.
// It reports codes, never the reference or the token.
func (x *Extension) authCheck(ctx context.Context, _ protocol.CheckConfig) inproc.CheckResult {
	check := inproc.CheckResult{Name: x.k.Type + "_auth", Status: inproc.CheckOK, Code: "verified"}
	sources := x.list()
	if len(sources) == 0 {
		check.Status, check.Code = inproc.CheckInfo, "not_configured"
		return check
	}
	for _, s := range sources {
		cx, cancel := context.WithTimeout(ctx, 20*time.Second)
		who, err := s.service.Verify(cx)
		cancel()
		check.Items = append(check.Items, VerifyItem(&check, s.settings.Namespace, who, err))
	}
	sort.Strings(check.Items)
	return check
}

// VerifyItem is a doctor line for one entry's sign-in, raising check to
// fail on an error.
func VerifyItem(check *inproc.CheckResult, ns, who string, err error) string {
	if err == nil {
		return ns + ": " + who
	}
	code := "verify_failed"
	switch ErrorCode(err) {
	case "credential":
		code = "credential_rejected"
	case "rate_limited":
		code = "rate_limited"
	case "not_found":
		code = "unreachable"
	}
	Raise(check, inproc.CheckFail, code)
	return ns + ": " + code
}

// Raise sets check's status and code when status is worse than its own.
func Raise(check *inproc.CheckResult, status, code string) {
	rank := map[string]int{inproc.CheckOK: 0, inproc.CheckInfo: 1, inproc.CheckWarn: 2, inproc.CheckFail: 3}
	if rank[status] > rank[check.Status] {
		check.Status, check.Code = status, code
	}
}

// Hooks is the state of an extension's managed service webhooks and its
// projection codes, by entry namespace.
type Hooks struct {
	mu         sync.Mutex
	hooks      map[string]*hook
	projection map[string]string
}

type hook struct {
	url, id, code string
	next          time.Time
}

// Due reports whether a namespace's hook needs pointing at target now.
func (h *Hooks) Due(ns, target string, now time.Time) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	st := h.hooks[ns]
	return st == nil || !(st.url == target && (st.code == "ok" || st.code == "dry_run") || now.Before(st.next))
}

// ID is a namespace's hook ID, "" for none.
func (h *Hooks) ID(ns string) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	if st := h.hooks[ns]; st != nil {
		return st.id
	}
	return ""
}

// Set records a namespace's hook; a failure is retried after a minute.
func (h *Hooks) Set(ns, url, id, code string, now time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.hooks == nil {
		h.hooks = map[string]*hook{}
	}
	st := &hook{url: url, id: id, code: code}
	if code != "ok" && code != "dry_run" {
		st.next = now.Add(time.Minute)
	}
	h.hooks[ns] = st
}

// SetProjection records a namespace's last projection code.
func (h *Hooks) SetProjection(ns, code string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.projection == nil {
		h.projection = map[string]string{}
	}
	h.projection[ns] = code
}

// Projection is a namespace's last projection code.
func (h *Hooks) Projection(ns string) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.projection[ns]
}

// Item is a doctor note on an entry's webhook, warning check on a failed
// hook update.
func (h *Hooks) Item(check *inproc.CheckResult, ns, name string, secret, manage bool) string {
	if !secret {
		return "no webhook_secret (not mounted; reconcile only)"
	}
	item := "POST /" + name
	h.mu.Lock()
	st := h.hooks[ns]
	h.mu.Unlock()
	switch {
	case st != nil:
		item += "; hook " + st.code
		if st.code != "ok" && st.code != "dry_run" {
			Raise(check, inproc.CheckWarn, st.code)
		}
	case manage:
		item += "; hook pending"
	}
	return item
}
