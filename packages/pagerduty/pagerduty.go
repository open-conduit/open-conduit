// Package pagerduty is the PagerDuty source as an installed package on
// protocol/sdk: it uses only the public contracts, so a third party could
// have written it. An entry watches a PagerDuty account's incidents (all,
// or some services'): a verified V3 webhook pushes them, and a reconcile
// over open incidents is the backstop. One Work per incident, so the
// incident's later status changes reach the same Work, which completes
// once the incident resolves. The toolbelt operations read the incident and
// add a note to it; acknowledging it is off unless the operator allows it.
package pagerduty

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/mail"
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
const Name = "pagerduty"

// Version is this package build's own version.
const Version = "0.1.0"

// WorkKind is the kind of Work a PagerDuty route creates.
const WorkKind = "pagerduty.incident"

// SecretToken is the connection secret holding a PagerDuty REST API key.
const SecretToken = "api_token"

// DefaultInterval is the reconcile interval when an entry sets none.
const DefaultInterval = 300

// Connection is a pagerduty connection's config.
type Connection struct {
	APIURL string `json:"api_url,omitempty"`
	// From is the email of the PagerDuty user notes and acknowledgements
	// are made as (PagerDuty requires one for writes with an account key).
	From string `json:"from,omitempty"`
}

// Settings is a pagerduty entry's config.
type Settings struct {
	// Scope is the scope the entry is in (the host checks it).
	Scope string `json:"scope"`
	// Namespace is the source namespace (default "pagerduty").
	Namespace string `json:"namespace,omitempty"`
	// Services limits the entry to these service IDs (all when empty).
	Services []string `json:"services,omitempty"`
	// WebhookSecret names the entry secret holding the webhook
	// subscription's signing secret. Without it the entry is reconcile only.
	WebhookSecret string `json:"webhook_secret,omitempty"`
	// IntervalSeconds is how often reconcile runs.
	IntervalSeconds int `json:"interval_seconds,omitempty"`
	// AllowAcknowledge lets agents acknowledge their incident with
	// pagerduty.incident.acknowledge. Acknowledging stops escalation to the
	// next responder, so it is off unless the operator turns it on.
	AllowAcknowledge bool `json:"allow_acknowledge,omitempty"`
}

// ParseSettings parses and checks an entry's config against the secret
// names it may resolve.
func ParseSettings(raw json.RawMessage, secrets []string) (Settings, error) {
	var s Settings
	if err := strictJSON(raw, &s); err != nil {
		return s, err
	}
	if s.Scope == "" {
		return s, errors.New("a pagerduty entry needs its scope")
	}
	if s.Namespace == "" {
		s.Namespace = Name
	}
	if !protocol.SourceNamespace(Name, s.Namespace) {
		return s, errors.New("a pagerduty namespace must be pagerduty or begin with pagerduty/")
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
	if c.From != "" {
		if _, err := mail.ParseAddress(c.From); err != nil {
			return c, errors.New("from must be an email address")
		}
	}
	return c, nil
}

// RouteConfig is a pagerduty route's config.
type RouteConfig struct {
	// Urgencies limits new Work to incidents of these urgencies ("high",
	// "low"; all when empty). An incident's later changes always reach its
	// Work.
	Urgencies []string `json:"urgencies,omitempty"`
}

// ParseRoute parses a route's config.
func ParseRoute(raw json.RawMessage) (RouteConfig, error) {
	var c RouteConfig
	if err := strictJSON(raw, &c); err != nil {
		return c, err
	}
	for _, u := range c.Urgencies {
		if u != "high" && u != "low" {
			return c, fmt.Errorf("unknown urgency %q", u)
		}
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
		sdk.Summary("PagerDuty incidents as Work, with operations to read the incident, add a note and (when allowed) acknowledge it."))
	ext.Manifest.Provides.Connections = []protocol.TypeDecl{{Type: Name}}
	ext.Manifest.Provides.Webhooks = true
	ext.Manifest.Provides.Render = true
	ext.Manifest.Provides.Schemas = []protocol.SchemaDecl{{Name: "pagerduty.incident", Versions: []int{1}}}
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
			ack := "agents may not acknowledge"
			if e.settings.AllowAcknowledge {
				ack = "agents may acknowledge"
			}
			items = append(items, fmt.Sprintf("%s: %s, %s", e.settings.Namespace, hook, ack))
		}
		if len(items) == 0 {
			return "no PagerDuty entries are configured", nil
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
		if err != nil || settings.Scope != e.ScopeID {
			return out, protocol.Errorf(protocol.CodeInvalid, "invalid pagerduty entry")
		}
		if e.Connection == nil || e.Connection.Type != Name {
			return out, protocol.Errorf(protocol.CodeInvalid, "a pagerduty entry needs a pagerduty connection")
		}
		conn, err := ParseConnection(e.Connection.Config)
		if err != nil {
			return out, protocol.Errorf(protocol.CodeInvalid, "invalid pagerduty connection")
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
			client := NewClient(conn.APIURL, conn.From, token, st.o.HTTP)
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

// Route opens one Work per triggered incident. An incident's later status
// changes reach the Work it already has; an acknowledged or resolved
// incident without Work opens none.
type Route struct{}

func (Route) Match(_ context.Context, in extension.RouteInput) (extension.RouteDecision, error) {
	e := in.Event.Event
	if e.Type == TypeAbsent {
		// Reconcile no longer lists the incident (a missed webhook): its Work
		// hears it, and nothing new opens.
		if in.Work != nil && e.CorrelationKey != nil && protocol.SourceNamespace(Name, e.CorrelationKey.Source) {
			return extension.RouteDecision{Match: true, Reason: "an incident with Work is no longer open"}, nil
		}
		return extension.RouteDecision{Reason: "not an open PagerDuty incident with Work"}, nil
	}
	if e.CorrelationKey == nil || !protocol.SourceNamespace(Name, e.Source) {
		return extension.RouteDecision{Reason: "not a PagerDuty event"}, nil
	}
	switch e.Type {
	case TypeTriggered, TypeAcknowledged, TypeResolved:
	default:
		return extension.RouteDecision{Reason: "not a PagerDuty incident event"}, nil
	}
	if in.Work != nil {
		return extension.RouteDecision{Match: true, Reason: "a change to an incident that has Work"}, nil
	}
	if e.Type != TypeTriggered {
		return extension.RouteDecision{Reason: "only a triggered incident opens Work"}, nil
	}
	cfg, err := ParseRoute(in.Route.Config.Data)
	if err != nil {
		return extension.RouteDecision{}, extension.ErrInvalid
	}
	var p IncidentPayload
	_ = json.Unmarshal(e.Data.Data, &p)
	if len(cfg.Urgencies) > 0 && !contains(cfg.Urgencies, p.Urgency) {
		return extension.RouteDecision{Reason: "an urgency this route doesn't admit"}, nil
	}
	id := e.CorrelationKey.ExternalID
	return extension.RouteDecision{Match: true, CreateID: domain.ID(e.Source + "/" + id), Kind: WorkKind,
		Display: Display(p), Reason: "a triggered incident the route admits"}, nil
}

// Display is what an incident's Work shows.
func Display(p IncidentPayload) *domain.WorkDisplay {
	ref := p.Incident
	if p.Number > 0 {
		ref = fmt.Sprintf("#%d", p.Number)
	}
	d := &domain.WorkDisplay{Title: strings.TrimSpace(ref + " " + p.Title), Ref: ref}
	if strings.HasPrefix(p.URL, "https://") || strings.HasPrefix(p.URL, "http://") {
		d.Links = []domain.Link{{Label: "PagerDuty incident", URL: p.URL}}
	}
	return d
}

// Render is one line for an Event in run history.
func Render(e domain.Event) (string, bool) {
	switch e.Type {
	case TypeTriggered, TypeAcknowledged, TypeResolved:
		var p IncidentPayload
		if json.Unmarshal(e.Data.Data, &p) != nil {
			return "", false
		}
		return fmt.Sprintf("PagerDuty incident #%d %s: %s", p.Number, p.Status, p.Title), true
	}
	return "", false
}
