package bootstrap

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/alsiola/open-conduit/domain"
	"github.com/alsiola/open-conduit/extension"
	bb "github.com/alsiola/open-conduit/extensions/bitbucket"
	"github.com/alsiola/open-conduit/extensions/jira"
	"github.com/alsiola/open-conduit/extensions/linear"
	"github.com/alsiola/open-conduit/extensions/tracker"
	"github.com/alsiola/open-conduit/internal/config"
	"github.com/alsiola/open-conduit/internal/runtime"
)

// Installed integration parity (06-01): the Linear and Jira trackers (one
// shared kit, extensions/tracker) and the Bitbucket Cloud code host are
// composed like GitHub: each enabled scope's entry registers an ingress pair
// (webhook push on the listener when it names a webhook_secret, and
// reconcile), its route handler selects input, the tracker projector
// converges claim/status labels, workflow transitions and marked comments,
// and the tools join every generation's toolbelt. Nothing here is a
// tracker-specific branch in the runtime: labels and transitions stay in the
// kit's projector.

// trackerTypes are the tracker extension types (handler = type).
var trackerTypes = []string{linear.Type, jira.Type}

// trackerSource is one registered tracker or code-host entry.
type trackerSource struct {
	name, typ string
	scope     domain.ID
	namespace string
	config    json.RawMessage
	// Tracker entries.
	settings  tracker.Settings
	service   tracker.Service
	ingress   extension.Ingress
	projector *tracker.Projector
	// Bitbucket entries.
	bitbucket *bb.Source
	bbIngress *bb.Ingress
}

func (s *trackerSource) webhookSecretName() string {
	if s.bitbucket != nil {
		return s.bitbucket.Settings.WebhookSecret
	}
	return s.settings.WebhookSecret
}

func (s *trackerSource) manageWebhook() bool {
	if s.bitbucket != nil {
		return s.bitbucket.Settings.ManageWebhook
	}
	return s.settings.ManageWebhook
}

func (s *trackerSource) dryRun() bool {
	if s.bitbucket != nil {
		return s.bitbucket.Settings.DryRun
	}
	return s.settings.Projection.DryRun
}

type trackerSources struct {
	mu      sync.Mutex
	sources map[string]*trackerSource // by source namespace
	failed  map[string]string         // registration failure code by namespace
	last    time.Time
	running bool
}

// trackerRegistry registers the connection types, the ingress extension
// types and the route-config validators of Linear, Jira and Bitbucket.
func trackerRegistry(reg *config.Registry) {
	if reg.Connections == nil {
		reg.Connections = map[string]func(json.RawMessage) error{}
	}
	if reg.Extensions == nil {
		reg.Extensions = map[string]config.ExtensionType{}
	}
	if reg.HandlerConfig == nil {
		reg.HandlerConfig = map[string]func(json.RawMessage) error{}
	}
	reg.Connections[linear.Type] = linear.ValidateConnection
	reg.Connections[jira.Type] = jira.ValidateConnection
	reg.Connections[bb.Type] = bb.ValidateConnection
	reg.Extensions[linear.Type] = config.ExtensionType{Ingress: true, Validate: linear.ValidateExtension}
	reg.Extensions[jira.Type] = config.ExtensionType{Ingress: true, Validate: jira.ValidateExtension}
	reg.Extensions[bb.Type] = config.ExtensionType{Ingress: true, Validate: bb.ValidateExtension}
	reg.HandlerConfig[linear.Type] = tracker.ValidateRoute
	reg.HandlerConfig[jira.Type] = tracker.ValidateRoute
}

// trackerOperations are the tool operations of the tracker extensions.
func trackerOperations() []string {
	var out []string
	for _, t := range trackerTypes {
		out = append(out, tracker.Operations(t)...)
	}
	return append(out, bb.Operations()...)
}

func trackerHandlers(h map[string]extension.RouteHandler) {
	for _, t := range trackerTypes {
		h[t] = tracker.Route{Type: t}
	}
	h[bb.Type] = bb.Route{}
}

func (d *daemon) connection(s *config.Snapshot, id, typ string) (*config.Connection, error) {
	if id == "" {
		return nil, githubCode(typ + "_connection_required")
	}
	for i := range s.Connections {
		if s.Connections[i].ID == id {
			if s.Connections[i].Type != typ {
				return nil, githubCode(typ + "_connection_type")
			}
			return &s.Connections[i], nil
		}
	}
	return nil, githubCode(typ + "_connection_type")
}

// registerTrackers registers every enabled scope's Linear, Jira and
// Bitbucket entry not yet registered. Registrations are immutable for one
// runtime instance: a changed or removed entry reports restart_required.
func (d *daemon) registerTrackers(ctx context.Context, s *config.Snapshot) {
	d.trackers.mu.Lock()
	defer d.trackers.mu.Unlock()
	if d.trackers.sources == nil {
		d.trackers.sources = map[string]*trackerSource{}
		d.trackers.failed = map[string]string{}
	}
	for _, id := range s.Order {
		scope := s.Scopes[id]
		if scope == nil || !scope.Enabled {
			continue
		}
		for _, x := range scope.Extensions {
			var ns, entryScope string
			switch x.Type {
			case linear.Type:
				st, err := linear.ParseExtension(x.Config, x.SecretNames)
				if err != nil {
					continue
				}
				ns, entryScope = st.Namespace, st.Scope
			case jira.Type:
				st, err := jira.ParseExtension(x.Config, x.SecretNames)
				if err != nil {
					continue
				}
				ns, entryScope = st.Namespace, st.Scope
			case bb.Type:
				st, err := bb.ParseExtension(x.Config, x.SecretNames)
				if err != nil {
					continue
				}
				ns, entryScope = st.Namespace, st.Scope
			default:
				continue
			}
			if entryScope != string(id) {
				continue
			}
			if prev, ok := d.trackers.sources[ns]; ok {
				if string(prev.config) != string(x.Config) {
					d.report(x.Type, domain.ID(ns), "restart_required")
				}
				continue
			}
			if err := d.addTracker(ctx, s, id, x); err != nil {
				code := errorCode(err)
				var named githubCode
				if errors.As(err, &named) {
					code = string(named)
				}
				d.report(x.Type, domain.ID(ns), code)
				d.trackers.failed[ns] = code
			} else {
				delete(d.trackers.failed, ns)
			}
		}
	}
}

func (d *daemon) addTracker(ctx context.Context, s *config.Snapshot, scope domain.ID, x config.Extension) error {
	conn, err := d.connection(s, x.Connection, x.Type)
	if err != nil {
		return err
	}
	secrets := tracker.SecretFunc(d.connectionSecrets(conn.ID))
	name := x.Type + "/" + string(scope) + "/" + x.Name
	interval := time.Duration(s.Settings.ReconcileSeconds) * time.Second
	src := &trackerSource{name: name, typ: x.Type, scope: scope, config: append(json.RawMessage(nil), x.Config...)}
	switch x.Type {
	case linear.Type:
		cfg, err := linear.ParseConnection(conn.Config)
		if err != nil {
			return githubCode("linear_connection_invalid")
		}
		st, _ := linear.ParseExtension(x.Config, x.SecretNames)
		if d.o.DryRun {
			st.Projection.DryRun = true
		}
		client, err := linear.NewClient(cfg, st, secrets, linear.ClientOptions{Now: d.now, DryRun: st.Projection.DryRun})
		if err != nil {
			return err
		}
		src.settings, src.service = st.Settings, client
	case jira.Type:
		cfg, err := jira.ParseConnection(conn.Config)
		if err != nil {
			return githubCode("jira_connection_invalid")
		}
		st, _ := jira.ParseExtension(x.Config, x.SecretNames)
		if d.o.DryRun {
			st.Projection.DryRun = true
		}
		client, err := jira.NewClient(cfg, st, secrets, jira.ClientOptions{Now: d.now, DryRun: st.Projection.DryRun})
		if err != nil {
			return err
		}
		src.settings, src.service = st.Settings, client
	case bb.Type:
		cfg, err := bb.ParseConnection(conn.Config)
		if err != nil {
			return githubCode("bitbucket_connection_invalid")
		}
		st, _ := bb.ParseExtension(x.Config, x.SecretNames)
		if d.o.DryRun {
			st.DryRun = true
		}
		client := bb.NewClient(cfg, secrets, bb.ClientOptions{Now: d.now})
		in := bb.NewIngress(st, client, d.now, bb.IngressOptions{Generations: d.trackerGenerations(st.TrackerNS)})
		src.namespace, src.bbIngress, src.ingress = st.Namespace, in, in
		src.bitbucket = &bb.Source{Settings: st, Client: client, Outbox: bb.NewOutbox(d.coord, st, d.now)}
	}
	if src.bitbucket == nil {
		src.namespace = src.settings.Namespace
		src.ingress = tracker.NewIngress(src.settings, src.service, d.now, tracker.IngressOptions{})
		projector, err := tracker.NewProjector(tracker.ProjectorOptions{Store: d.coord, Service: src.service, Settings: src.settings, Now: d.now})
		if err != nil {
			return err
		}
		src.projector = projector
	}
	if err := d.ingress.Register(ctx, name, extension.Source{Namespace: src.namespace, ScopeID: scope}, src.ingress, interval); err != nil {
		return err
	}
	d.trackers.sources[src.namespace] = src
	d.log.Info(x.Type+" ingress registered", "name", name, "namespace", src.namespace)
	return nil
}

// trackerGenerations resolves a tracker issue's reopen generation through
// the registered tracker entry of that namespace (looked up at call time,
// so registration order does not matter). Without one, the base key is used.
func (d *daemon) trackerGenerations(ns string) bb.Generations {
	if ns == "" {
		return nil
	}
	return func(ctx context.Context, key string, at time.Time) (string, error) {
		d.trackers.mu.Lock()
		src := d.trackers.sources[ns]
		d.trackers.mu.Unlock()
		if src == nil || src.service == nil {
			return "", nil
		}
		reopens, err := src.service.Reopens(ctx, key)
		if err != nil {
			return "", err
		}
		return tracker.GenerationAt(reopens, at), nil
	}
}

func (d *daemon) trackerList() []*trackerSource {
	d.trackers.mu.Lock()
	defer d.trackers.mu.Unlock()
	out := make([]*trackerSource, 0, len(d.trackers.sources))
	for _, s := range d.trackers.sources {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

// projectTrackers starts one bounded projection pass per tracker entry, at
// most every few seconds, on a single background goroutine; passes never
// overlap and never stall routing or starts.
func (d *daemon) projectTrackers(ctx context.Context) {
	d.trackers.mu.Lock()
	if d.trackers.running || len(d.trackers.sources) == 0 || d.now().Sub(d.trackers.last) < 5*time.Second {
		d.trackers.mu.Unlock()
		return
	}
	d.trackers.running, d.trackers.last = true, d.now()
	d.trackers.mu.Unlock()
	sources := d.trackerList()
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		defer func() {
			d.trackers.mu.Lock()
			d.trackers.running = false
			d.trackers.mu.Unlock()
		}()
		for _, src := range sources {
			if ctx.Err() != nil {
				return
			}
			if src.projector == nil {
				continue
			}
			res, err := src.projector.Converge(ctx, passLimit)
			code := strings.Join(res.Codes, ",")
			if err != nil && code == "" {
				code = tracker.ErrorCode(err)
			}
			d.report(src.typ+"_projection", domain.ID(src.name), code)
		}
	}()
}

// trackerToolSources is a tracker type's tools' view of the registered entries.
func (d *daemon) trackerToolSources(typ string) func() []tracker.Source {
	return func() []tracker.Source {
		var out []tracker.Source
		for _, s := range d.trackerList() {
			if s.typ == typ && s.projector != nil {
				out = append(out, tracker.Source{Settings: s.settings, Service: s.service, Projector: s.projector})
			}
		}
		return out
	}
}

func (d *daemon) bitbucketSources() []bb.Source {
	var out []bb.Source
	for _, s := range d.trackerList() {
		if s.bitbucket != nil {
			out = append(out, *s.bitbucket)
		}
	}
	return out
}

// openTrackerTools prepares the tracker and Bitbucket tools.
func (d *daemon) openTrackerTools() error {
	d.trackerTools = map[string]*tracker.Tools{}
	for _, t := range trackerTypes {
		tools, err := tracker.NewTools(tracker.ToolsOptions{Type: t, Store: d.coord, Sources: d.trackerToolSources(t), Now: d.now})
		if err != nil {
			return err
		}
		d.trackerTools[t] = tools
	}
	var err error
	d.bitbucketTools, err = bb.NewTools(bb.ToolsOptions{Store: d.coord, Sources: d.bitbucketSources, Now: d.now})
	return err
}

func (d *daemon) registerTrackerTools(belt *runtime.Toolbelt) error {
	for _, t := range trackerTypes {
		if err := belt.Register(d.trackerTools[t].Registration()); err != nil {
			return err
		}
	}
	return belt.Register(d.bitbucketTools.Registration())
}

// trackerSecret resolves an entry's webhook_secret from its scope's
// effective references in the current generation.
func (d *daemon) trackerSecret(src *trackerSource) func(context.Context) (string, error) {
	return func(ctx context.Context) (string, error) {
		name := src.webhookSecretName()
		snapshot, _ := d.configs.Current()
		if name == "" || snapshot == nil || snapshot.Scopes[src.scope] == nil {
			return "", tracker.ErrCredential
		}
		values, err := d.resolver.ResolveNames(ctx, snapshot.Scopes[src.scope], []string{name})
		if err != nil {
			return "", tracker.ErrCredential
		}
		return values[name], nil
	}
}

// trackerWebhook returns the handler of a registered tracker or Bitbucket
// entry route (POST /<type>/<scope>/<entry>) that names a webhook_secret.
func (d *daemon) trackerWebhook(name string) http.Handler {
	var src *trackerSource
	for _, s := range d.trackerList() {
		if s.name == name && s.webhookSecretName() != "" {
			src = s
		}
	}
	if src == nil {
		return nil
	}
	push := func(ctx context.Context, delivery extension.Delivery) error {
		_, err := d.ingress.Push(ctx, src.name, delivery)
		if err == nil {
			d.poke()
		}
		return err
	}
	if src.bitbucket != nil {
		return bb.WebhookHandler(src.bitbucket.Settings, d.trackerSecret(src), push, d.now)
	}
	return tracker.WebhookHandler(src.typ, src.service, d.trackerSecret(src), push, d.now)
}

func isTrackerRoute(name string) bool {
	for _, t := range append(append([]string{}, trackerTypes...), bb.Type) {
		if strings.HasPrefix(name, t+"/") {
			return true
		}
	}
	return false
}

type hookJob struct {
	src *trackerSource
	url string
}

// trackerHookJobs lists the manage_webhook entries whose hook needs pointing
// at the current public URL. Callers hold d.hooks.mu.
func (d *daemon) trackerHookJobs(public string) []hookJob {
	var jobs []hookJob
	for _, src := range d.trackerList() {
		if !src.manageWebhook() || src.webhookSecretName() == "" {
			continue
		}
		st := d.hooks.hooks[src.namespace]
		target := public + "/" + src.name
		if st != nil && (st.url == target && (st.code == "ok" || st.code == "dry_run") || d.now().Before(st.next)) {
			continue
		}
		jobs = append(jobs, hookJob{src, target})
	}
	return jobs
}

// ensureTrackerHook points one entry's service webhook at url.
func (d *daemon) ensureTrackerHook(ctx context.Context, j hookJob, existing string) (string, string) {
	secret, err := d.trackerSecret(j.src)(ctx)
	if err != nil {
		return existing, "credential_unavailable"
	}
	dry := d.o.DryRun || j.src.dryRun()
	var id string
	if j.src.bitbucket != nil {
		id, err = j.src.bitbucket.Client.EnsureWebhook(ctx, j.src.bitbucket.Settings, j.url, secret, existing, dry)
	} else if dry {
		id = existing
		if id == "" {
			id = "dry-run"
		}
	} else {
		id, err = j.src.service.EnsureWebhook(ctx, j.url, secret, existing)
	}
	if err != nil {
		return existing, trackerHookCode(err)
	}
	if dry {
		return id, "dry_run"
	}
	return id, "ok"
}

func trackerHookCode(err error) string {
	var limit *tracker.RateLimitError
	switch {
	case errors.Is(err, tracker.ErrCredential), errors.Is(err, jira.ErrHookPermission), tracker.IsStatus(err, http.StatusForbidden):
		return "hook_permission_denied"
	case errors.As(err, &limit):
		return "rate_limited"
	}
	return "hook_update_failed"
}

// trackerWebhookItems are the webhook doctor lines of tracker entries.
func (d *daemon) trackerWebhookItems(hooks map[string]hookState) []string {
	var out []string
	for _, src := range d.trackerList() {
		item := src.namespace + ": "
		if src.webhookSecretName() == "" {
			item += "no webhook_secret (not mounted; reconcile only)"
		} else {
			item += "POST /" + src.name
			if h, ok := hooks[src.namespace]; ok {
				item += "; hook " + h.code
			} else if src.manageWebhook() {
				item += "; hook pending"
			}
		}
		out = append(out, item)
	}
	return out
}

type paused interface {
	PausedUntil() (time.Time, bool)
}

// trackerCheck reports registration, rate-limit, reconcile and projection
// state per Linear, Jira and Bitbucket entry. It makes no service calls.
func (d *daemon) trackerCheck(ctx context.Context) runtime.Check {
	check := runtime.Check{Name: "trackers", Status: runtime.CheckOK, Code: "ok"}
	raise := func(status, code string) {
		if rank(status) > rank(check.Status) {
			check.Status, check.Code = status, code
		}
	}
	d.trackers.mu.Lock()
	failed := map[string]string{}
	for ns, code := range d.trackers.failed {
		failed[ns] = code
	}
	d.trackers.mu.Unlock()
	sources := d.trackerList()
	if len(sources) == 0 && len(failed) == 0 {
		check.Status, check.Code, check.Detail = runtime.CheckInfo, "not_configured", "no Linear, Jira or Bitbucket entries are configured"
		return check
	}
	for ns, code := range failed {
		raise(runtime.CheckFail, code)
		check.Items = append(check.Items, ns+": not registered ("+code+")")
	}
	scans := map[string]runtime.IngressStatus{}
	if statuses, err := d.ingress.Inspect(ctx); err == nil {
		for _, s := range statuses {
			scans[s.Name] = s
		}
	}
	for _, src := range sources {
		var notes []string
		var client any = src.service
		if src.bitbucket != nil {
			client = src.bitbucket.Client
		}
		if p, ok := client.(paused); ok {
			if until, on := p.PausedUntil(); on {
				raise(runtime.CheckWarn, "rate_limited")
				notes = append(notes, "rate limited until "+until.UTC().Format(time.RFC3339))
			}
		}
		if s, ok := scans[src.name]; ok {
			switch {
			case s.LastError != "":
				raise(runtime.CheckWarn, "reconcile_failing")
				notes = append(notes, fmt.Sprintf("reconcile failing (%s, %d failures)", s.LastError, s.Failures))
			case s.LastComplete.IsZero():
				notes = append(notes, "first reconcile pending")
			default:
				notes = append(notes, "reconciled "+d.now().Sub(s.LastComplete).Truncate(time.Second).String()+" ago")
			}
		}
		if code := d.reportedCode(src.typ+"_projection", src.name); code != "" {
			raise(runtime.CheckWarn, "projection_failing")
			notes = append(notes, "projection "+code)
		}
		if src.bitbucket != nil && src.bitbucket.Settings.TrackerNS != "" {
			d.trackers.mu.Lock()
			_, linked := d.trackers.sources[src.bitbucket.Settings.TrackerNS]
			d.trackers.mu.Unlock()
			if !linked {
				raise(runtime.CheckWarn, "tracker_missing")
				notes = append(notes, "tracker "+src.bitbucket.Settings.TrackerNS+" is not registered (PRs of reopened issues key the first generation)")
			}
		}
		if d.o.DryRun || src.dryRun() {
			notes = append(notes, "dry run (no "+src.typ+" writes)")
		}
		check.Items = append(check.Items, src.typ+" "+src.namespace+": "+strings.Join(notes, "; "))
	}
	sort.Strings(check.Items)
	return check
}

// trackerAuthCheck (check_secrets only) resolves each entry's credential
// privately and signs in: the connection test. It reports codes, never the
// reference or the token.
func (d *daemon) trackerAuthCheck(ctx context.Context) runtime.Check {
	check := runtime.Check{Name: "trackers_auth", Status: runtime.CheckOK, Code: "verified"}
	sources := d.trackerList()
	if len(sources) == 0 {
		check.Status, check.Code = runtime.CheckInfo, "not_configured"
		return check
	}
	for _, src := range sources {
		cx, cancel := context.WithTimeout(ctx, 20*time.Second)
		var who string
		var err error
		if src.bitbucket != nil {
			who, err = src.bitbucket.Client.Verify(cx)
		} else {
			who, err = src.service.Verify(cx)
		}
		cancel()
		item := src.typ + " " + src.namespace + ": "
		if err != nil {
			code := "verify_failed"
			switch tracker.ErrorCode(err) {
			case "credential":
				code = "credential_rejected"
			case "rate_limited":
				code = "rate_limited"
			case "not_found":
				code = "unreachable"
			}
			if rank(runtime.CheckFail) > rank(check.Status) {
				check.Status, check.Code = runtime.CheckFail, code
			}
			check.Items = append(check.Items, item+code)
			continue
		}
		check.Items = append(check.Items, item+who)
	}
	sort.Strings(check.Items)
	return check
}
