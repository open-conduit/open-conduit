// Package sentry is the Sentry source as an installed package on
// protocol/sdk: it uses only the public contracts, so a third party could
// have written it. An entry watches one Sentry organization's issues: a
// verified webhook (an internal integration's issue and issue alert
// deliveries) pushes them, and a reconcile over the organization's
// unresolved issues is the backstop. One Work per issue (the issue's ID is
// the correlation key), so a burst of alerts on one issue reaches the same
// Work. The toolbelt operations read the issue's latest event and stack
// trace and comment on or resolve the issue the calling Work was opened
// for, with the entry's connection.
package sentry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/protocol"
	"github.com/open-conduit/open-conduit/protocol/sdk"
)

// Name is the package's name, its extension, connection and route type.
const Name = "sentry"

// Version is this package build's own version.
const Version = "0.1.0"

// WorkKind is the kind of Work a Sentry route creates.
const WorkKind = "sentry.issue"

// SecretToken is the connection secret holding a Sentry auth token (an
// internal integration's token, with Issue & Event read and write).
const SecretToken = "auth_token"

// DefaultInterval is the reconcile interval when an entry sets none.
const DefaultInterval = 300

// Connection is a sentry connection's config.
type Connection struct {
	APIURL string `json:"api_url,omitempty"`
}

// Settings is a sentry entry's config.
type Settings struct {
	// Org is the organization slug.
	Org string `json:"org"`
	// Namespace is the source namespace (default "sentry/<org>").
	Namespace string `json:"namespace,omitempty"`
	// Projects limits the entry to these project slugs (all when empty).
	Projects []string `json:"projects,omitempty"`
	// WebhookSecret names the entry secret holding the integration's
	// client secret, which signs its webhooks. Without it the entry is
	// reconcile only.
	WebhookSecret string `json:"webhook_secret,omitempty"`
	// IntervalSeconds is how often reconcile runs.
	IntervalSeconds int `json:"interval_seconds,omitempty"`
}

// ParseSettings parses and checks an entry's config against the secret
// names it may resolve.
func ParseSettings(raw json.RawMessage, secrets []string) (Settings, error) {
	var s Settings
	if err := strictJSON(raw, &s); err != nil {
		return s, err
	}
	if s.Org == "" || strings.ContainsAny(s.Org, "/ ") {
		return s, errors.New("a sentry entry needs an org slug")
	}
	if s.Namespace == "" {
		s.Namespace = Name + "/" + s.Org
	}
	if !protocol.SourceNamespace(Name, s.Namespace) {
		return s, errors.New("a sentry namespace must be sentry or begin with sentry/")
	}
	if s.WebhookSecret != "" && secrets != nil && !contains(secrets, s.WebhookSecret) {
		return s, errors.New("webhook_secret must be one of the entry's secret_names")
	}
	if s.IntervalSeconds == 0 {
		s.IntervalSeconds = DefaultInterval
	}
	if s.IntervalSeconds < 60 {
		return s, errors.New("interval_seconds must be at least 60")
	}
	return s, nil
}

// ParseConnection parses a connection's config.
func ParseConnection(raw json.RawMessage) (Connection, error) {
	var c Connection
	if err := strictJSON(raw, &c); err != nil {
		return c, err
	}
	if c.APIURL != "" && !strings.HasPrefix(c.APIURL, "https://") && !strings.HasPrefix(c.APIURL, "http://") {
		return c, errors.New("api_url must be an http or https URL")
	}
	return c, nil
}

// RouteConfig is a sentry route's config.
type RouteConfig struct {
	// Triggers are what opens Work for an issue that has none: "opened",
	// "regressed" and "alert" (default all three). Later changes to an
	// issue always reach its Work.
	Triggers []string `json:"triggers,omitempty"`
	// Levels limits new Work to these issue levels (all when empty).
	Levels []string `json:"levels,omitempty"`
}

// ParseRoute parses a route's config.
func ParseRoute(raw json.RawMessage) (RouteConfig, error) {
	var c RouteConfig
	if err := strictJSON(raw, &c); err != nil {
		return c, err
	}
	for _, t := range c.Triggers {
		if t != "opened" && t != "regressed" && t != "alert" {
			return c, fmt.Errorf("unknown trigger %q", t)
		}
	}
	if len(c.Triggers) == 0 {
		c.Triggers = []string{"opened", "regressed", "alert"}
	}
	return c, nil
}

func strictJSON(raw json.RawMessage, v any) error {
	if len(raw) == 0 {
		raw = []byte("{}")
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if x == y {
			return true
		}
	}
	return false
}

// Options configure Build.
type Options struct {
	Now  func() time.Time
	HTTP *http.Client
}

type entry struct {
	name, scope, config string
	settings            Settings
	client              *Client
	ingress             *Ingress
}

type state struct {
	o       Options
	host    atomic.Pointer[sdk.Host]
	mu      sync.Mutex
	entries map[string]*entry // by source name
}

func (st *state) list() []*entry {
	st.mu.Lock()
	defer st.mu.Unlock()
	out := make([]*entry, 0, len(st.entries))
	for _, e := range st.entries {
		out = append(out, e)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].name < out[b].name })
	return out
}

func families() []protocol.Family {
	var out []protocol.Family
	for _, f := range Families() {
		out = append(out, protocol.Family{Type: f.Type, ReDerivable: f.ReDerivable, Loss: f.Loss})
	}
	return out
}

// Build returns the served extension.
func Build(o Options) *sdk.Extension {
	if o.Now == nil {
		o.Now = time.Now
	}
	st := &state{o: o, entries: map[string]*entry{}}
	ext := sdk.New(Name, sdk.Version(Version),
		sdk.Summary("Sentry issues and issue alerts as Work, with operations to read, comment on and resolve the issue."))
	ext.Manifest.Provides.Connections = []protocol.TypeDecl{{Type: Name}}
	ext.Manifest.Provides.Webhooks = true
	ext.Manifest.Provides.Render = true
	ext.Manifest.Provides.Schemas = []protocol.SchemaDecl{{Name: "sentry.issue", Versions: []int{1}}, {Name: "sentry.alert", Versions: []int{1}}}
	ext.IngressType(Name, func(name string) (extension.Ingress, bool) {
		st.mu.Lock()
		defer st.mu.Unlock()
		e, ok := st.entries[name]
		if !ok {
			return nil, false
		}
		return e.ingress, true
	}, families()...)
	ext.Route(Name, Route{}, sdk.RouteConfig())
	ext.WorkKind(WorkKind)
	tools{st}.register(ext)
	ext.Check(Name, func(context.Context, *sdk.Host, bool) (string, error) {
		var items []string
		for _, e := range st.list() {
			hook := "webhook " + e.name
			if e.settings.WebhookSecret == "" {
				hook = "reconcile only"
			}
			items = append(items, fmt.Sprintf("%s: org %s, %s", e.settings.Namespace, e.settings.Org, hook))
		}
		if len(items) == 0 {
			return "no Sentry entries are configured", nil
		}
		return strings.Join(items, "; "), nil
	})
	ext.Validate = func(_ context.Context, p protocol.ValidateParams) error {
		switch p.Kind {
		case protocol.ValidateConnection:
			_, err := ParseConnection(p.Config)
			return err
		case protocol.ValidateExtension:
			_, err := ParseSettings(p.Config, p.SecretNames)
			return err
		case protocol.ValidateRoute:
			_, err := ParseRoute(p.Config)
			return err
		}
		return nil
	}
	ext.Configure = st.configure
	ext.Webhooks = func(path string) (http.Handler, bool) {
		name := strings.TrimPrefix(path, "/")
		st.mu.Lock()
		e := st.entries[name]
		st.mu.Unlock()
		if e == nil || e.settings.WebhookSecret == "" {
			return nil, false
		}
		secret := func(ctx context.Context) (string, error) {
			values, err := st.host.Load().Secrets(ctx, domain.ID(e.scope), e.settings.WebhookSecret)
			if err != nil || values[e.settings.WebhookSecret] == "" {
				return "", ErrCredential
			}
			return values[e.settings.WebhookSecret], nil
		}
		push := func(ctx context.Context, d extension.Delivery) error {
			_, err := st.host.Load().Deliver(ctx, e.name, protocol.FromDelivery(d))
			return err
		}
		return WebhookHandler(secret, push, o.Now), true
	}
	ext.Render = Render
	return ext
}

// Manifest is the package manifest.
func Manifest() protocol.Manifest { return Build(Options{}).Manifest }

// configure applies one generation; an unchanged entry keeps its client.
func (st *state) configure(_ context.Context, h *sdk.Host, p protocol.ConfigureParams) (protocol.ConfigureResult, error) {
	var out protocol.ConfigureResult
	st.host.Store(h)
	st.mu.Lock()
	prev := st.entries
	st.mu.Unlock()
	next := map[string]*entry{}
	var decl []protocol.EventFamily
	for _, f := range families() {
		decl = append(decl, protocol.EventFamily{Type: f.Type, ReDerivable: f.ReDerivable, Loss: f.Loss})
	}
	for _, e := range p.Entries {
		if e.Type != Name {
			continue
		}
		settings, err := ParseSettings(e.Config, e.Secrets)
		if err != nil {
			return out, protocol.Errorf(protocol.CodeInvalid, "invalid sentry entry")
		}
		if e.Connection == nil || e.Connection.Type != Name {
			return out, protocol.Errorf(protocol.CodeInvalid, "a sentry entry needs a sentry connection")
		}
		conn, err := ParseConnection(e.Connection.Config)
		if err != nil {
			return out, protocol.Errorf(protocol.CodeInvalid, "invalid sentry connection")
		}
		label := e.Name
		if label == "" {
			label = "entry"
		}
		name := Name + "/" + e.ScopeID + "/" + label
		digest, _ := json.Marshal(e)
		if old, ok := prev[name]; ok && old.config == string(digest) {
			next[name] = old
		} else {
			scope, connID := domain.ID(e.ScopeID), e.Connection.ID
			token := func(ctx context.Context) (string, error) {
				values, err := st.host.Load().ConnectionSecrets(ctx, scope, connID, SecretToken)
				if err != nil || values[SecretToken] == "" {
					return "", ErrCredential
				}
				return values[SecretToken], nil
			}
			client := NewClient(conn.APIURL, settings.Org, token, st.o.HTTP)
			next[name] = &entry{name: name, scope: e.ScopeID, config: string(digest), settings: settings, client: client,
				ingress: &Ingress{Settings: settings, Client: client, Now: st.o.Now}}
		}
		out.Sources = append(out.Sources, protocol.SourceDecl{Name: name, Source: protocol.Source{Namespace: settings.Namespace, ScopeID: e.ScopeID}, IntervalSeconds: settings.IntervalSeconds, Families: decl})
		if settings.WebhookSecret != "" {
			out.WebhookPaths = append(out.WebhookPaths, "/"+name)
		}
	}
	st.mu.Lock()
	st.entries = next
	st.mu.Unlock()
	return out, nil
}

// Route opens one Work per Sentry issue. An issue's later Events reach the
// Work it already has; an issue without Work opens one only on a trigger the
// route admits.
type Route struct{}

func (Route) Match(_ context.Context, in extension.RouteInput) (extension.RouteDecision, error) {
	e := in.Event.Event
	if e.CorrelationKey == nil || !protocol.SourceNamespace(Name, e.Source) {
		return extension.RouteDecision{Reason: "not a Sentry event"}, nil
	}
	trigger := ""
	switch e.Type {
	case TypeOpened:
		trigger = "opened"
	case TypeRegressed:
		trigger = "regressed"
	case TypeAlert:
		trigger = "alert"
	case TypeResolved, TypeIgnored:
	default:
		return extension.RouteDecision{Reason: "not a Sentry issue event"}, nil
	}
	if in.Work != nil {
		return extension.RouteDecision{Match: true, Reason: "a change to an issue that has Work"}, nil
	}
	cfg, err := ParseRoute(in.Route.Config.Data)
	if err != nil {
		return extension.RouteDecision{}, extension.ErrInvalid
	}
	if trigger == "" || !contains(cfg.Triggers, trigger) {
		return extension.RouteDecision{Reason: "not a trigger this route admits"}, nil
	}
	var p struct {
		ShortID string `json:"short_id"`
		Title   string `json:"title"`
		Level   string `json:"level"`
		URL     string `json:"url"`
	}
	_ = json.Unmarshal(e.Data.Data, &p)
	if len(cfg.Levels) > 0 && !contains(cfg.Levels, p.Level) {
		return extension.RouteDecision{Reason: "an issue level this route doesn't admit"}, nil
	}
	issue := e.CorrelationKey.ExternalID
	return extension.RouteDecision{Match: true, CreateID: domain.ID("sentry/" + strings.TrimPrefix(e.Source, Name+"/") + "/" + issue), Kind: WorkKind,
		Display: Display(issue, p.ShortID, p.Title, p.URL), Reason: "a new " + trigger + " the route admits"}, nil
}

// Display is what an issue's Work shows.
func Display(issue, shortID, title, url string) *domain.WorkDisplay {
	ref := shortID
	if ref == "" {
		ref = issue
	}
	d := &domain.WorkDisplay{Title: strings.TrimSpace(ref + " " + title), Ref: ref}
	if strings.HasPrefix(url, "https://") || strings.HasPrefix(url, "http://") {
		d.Links = []domain.Link{{Label: "Sentry issue", URL: url}}
	}
	return d
}

// Render is one line for an Event in run history.
func Render(e domain.Event) (string, bool) {
	switch e.Type {
	case TypeOpened, TypeRegressed, TypeResolved, TypeIgnored:
		var p IssuePayload
		if json.Unmarshal(e.Data.Data, &p) != nil {
			return "", false
		}
		return fmt.Sprintf("Sentry %s %s: %s", p.ShortID, strings.TrimPrefix(e.Type, "sentry.issue."), p.Title), true
	case TypeAlert:
		var p AlertPayload
		if json.Unmarshal(e.Data.Data, &p) != nil {
			return "", false
		}
		return fmt.Sprintf("Sentry alert %q fired: %s", p.Rule, p.Title), true
	}
	return "", false
}
