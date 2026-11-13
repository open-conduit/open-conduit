// Package webhook turns signed HTTP POSTs into Work, one Work for each
// delivery. It is an installed package on protocol/sdk and uses only the
// public contracts. An entry declares hooks, each served at a signed token
// URL. Every accepted POST is an Event. A hook's delivery key (a header or
// a JSON pointer into the body) makes a sender's retry the same delivery.
// Its correlation key, when set, sends a later delivery with the same value
// to the Work the first one opened, as a follow-up. Without one, each
// delivery is a Work of its own.
//
// A job webhook (the schedule extension) is the other kind of POST: it
// triggers the job's one standing Work, so deliveries coalesce into one
// run. Use this package when each delivery is a task of its own.
package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
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

// Name is the package's name, its extension and route type.
const Name = "webhook"

// Version is this package build's own version.
const Version = "0.1.0"

// WorkKind is the kind of Work a webhook route creates.
const WorkKind = "webhook.delivery"

// Settings is a webhook entry's config.
type Settings struct {
	// Scope is the scope the entry is in (the host checks it).
	Scope string `json:"scope"`
	// WebhookSecret names the entry secret the hooks' URL tokens are
	// derived from. Rotating it revokes every URL of the entry.
	WebhookSecret string `json:"webhook_secret"`
	// Hooks are the entry's endpoints.
	Hooks []Hook `json:"hooks"`
}

// Hook is one endpoint of an entry.
type Hook struct {
	// Name is the hook's name, in its URL and its Work's title.
	Name string `json:"name"`
	// DeliveryKey names where a delivery's identity is read
	// ("header:Idempotency-Key", "body:/id"). A POST with the same value
	// is the same delivery, so a sender's retry makes no second Work. A
	// POST without it is refused. Without a delivery key each POST is a
	// new delivery.
	DeliveryKey string `json:"delivery_key,omitempty"`
	// CorrelationKey names where a delivery's thread is read
	// ("body:/ticket/id"). A later delivery with the same value is input
	// of the Work the first one opened. A delivery without it, or a hook
	// without a correlation key, opens Work of its own.
	CorrelationKey string `json:"correlation_key,omitempty"`
	// Version is signed into the hook's URL token (0 when unset). Bumping
	// it revokes this hook's URL alone, keeping its name, Work and threads;
	// doctor --check-secrets prints the new URL.
	Version int `json:"version,omitempty"`
	// FanOut names the targets one delivery goes to. Ingress accepts one
	// Event for each, with its own delivery ID and thread, so a route that
	// takes a target gives it Work of its own: one POST can start Work for
	// several bindings. Without it a delivery is one Event.
	FanOut []string `json:"fan_out,omitempty"`
}

// MaxFanOut bounds a hook's targets. Each target's Event carries the whole
// body, and every Event of a delivery is accepted in one ingress page.
const MaxFanOut = 4

// Key is a parsed delivery or correlation key: a header, or a JSON
// pointer into the body.
type Key struct {
	Header  string
	Pointer string
}

// Set reports whether a key is configured.
func (k Key) Set() bool { return k.Header != "" || k.Pointer != "" }

var (
	hookPattern   = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)
	headerPattern = regexp.MustCompile("^[A-Za-z0-9!#$%&'*+.^_`|~-]{1,128}$")
)

// ParseKey parses "header:<name>" or "body:<JSON pointer>". The empty
// string is no key.
func ParseKey(s string) (Key, error) {
	if s == "" {
		return Key{}, nil
	}
	if name, ok := strings.CutPrefix(s, "header:"); ok {
		if !headerPattern.MatchString(name) {
			return Key{}, fmt.Errorf("%q is not a header name", name)
		}
		return Key{Header: http.CanonicalHeaderKey(name)}, nil
	}
	if ptr, ok := strings.CutPrefix(s, "body:"); ok {
		if _, err := splitPointer(ptr); err != nil {
			return Key{}, err
		}
		return Key{Pointer: ptr}, nil
	}
	return Key{}, fmt.Errorf("%q must be header:<name> or body:<JSON pointer>", s)
}

// Keys parses a hook's two keys; the hook has been validated.
func (h Hook) Keys() (delivery, correlation Key) {
	delivery, _ = ParseKey(h.DeliveryKey)
	correlation, _ = ParseKey(h.CorrelationKey)
	return delivery, correlation
}

// Hook finds a hook by name.
func (s Settings) Hook(name string) (Hook, bool) {
	for _, h := range s.Hooks {
		if h.Name == name {
			return h, true
		}
	}
	return Hook{}, false
}

// ParseSettings parses and checks an entry's config against the secret
// names it may resolve (nil skips that check).
func ParseSettings(raw json.RawMessage, secrets []string) (Settings, error) {
	var s Settings
	if err := strictJSON(raw, &s); err != nil {
		return s, err
	}
	if s.Scope == "" {
		return s, errors.New("a webhook entry needs its scope")
	}
	if s.WebhookSecret == "" {
		return s, errors.New("a webhook entry needs webhook_secret")
	}
	if secrets != nil && !contains(secrets, s.WebhookSecret) {
		return s, errors.New("webhook_secret must be one of the entry's secret_names")
	}
	if len(s.Hooks) == 0 {
		return s, errors.New("a webhook entry needs at least one hook")
	}
	seen := map[string]bool{}
	for _, h := range s.Hooks {
		if !hookPattern.MatchString(h.Name) {
			return s, fmt.Errorf("hook name %q must be lower case letters, digits, _ and -, at most 64", h.Name)
		}
		if seen[h.Name] {
			return s, fmt.Errorf("hook %q is declared twice", h.Name)
		}
		seen[h.Name] = true
		if _, err := ParseKey(h.DeliveryKey); err != nil {
			return s, fmt.Errorf("hook %s: delivery_key: %w", h.Name, err)
		}
		if _, err := ParseKey(h.CorrelationKey); err != nil {
			return s, fmt.Errorf("hook %s: correlation_key: %w", h.Name, err)
		}
		if h.Version < 0 {
			return s, fmt.Errorf("hook %s: version must not be negative", h.Name)
		}
		if err := checkTargets(h.FanOut); err != nil {
			return s, fmt.Errorf("hook %s: fan_out: %w", h.Name, err)
		}
	}
	return s, nil
}

func checkTargets(targets []string) error {
	if len(targets) > MaxFanOut {
		return fmt.Errorf("at most %d targets", MaxFanOut)
	}
	seen := map[string]bool{}
	for _, t := range targets {
		if !hookPattern.MatchString(t) {
			return fmt.Errorf("target %q must be lower case letters, digits, _ and -, at most 64", t)
		}
		if seen[t] {
			return fmt.Errorf("target %q is listed twice", t)
		}
		seen[t] = true
	}
	return nil
}

// RouteConfig is a webhook route's config.
type RouteConfig struct {
	// Hooks limits the route to these hooks (every hook of the scope's
	// webhook entries when empty).
	Hooks []string `json:"hooks,omitempty"`
	// Targets limits the route to these fan-out targets. A route with
	// targets takes only a fan-out hook's deliveries for them; one without
	// takes every delivery of its hooks, whatever the target.
	Targets []string `json:"targets,omitempty"`
}

// ParseRoute parses a route's config.
func ParseRoute(raw json.RawMessage) (RouteConfig, error) {
	var c RouteConfig
	if err := strictJSON(raw, &c); err != nil {
		return c, err
	}
	for _, h := range c.Hooks {
		if !hookPattern.MatchString(h) {
			return c, fmt.Errorf("hook name %q is invalid", h)
		}
	}
	for _, t := range c.Targets {
		if !hookPattern.MatchString(t) {
			return c, fmt.Errorf("target name %q is invalid", t)
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

// Namespace is an entry's source namespace, and its webhook mount below
// the listener: webhook/<scope>/<entry>.
func Namespace(scope, entry string) string { return Name + "/" + scope + "/" + entry }

// Options configure Build.
type Options struct {
	Now func() time.Time
}

type entry struct {
	namespace, scope, config string
	settings                 Settings
	ingress                  *Ingress
}

type state struct {
	o       Options
	host    atomic.Pointer[sdk.Host]
	mu      sync.Mutex
	entries map[string]*entry // by namespace
	public  string
}

func (st *state) list() ([]*entry, string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	out := make([]*entry, 0, len(st.entries))
	for _, e := range st.entries {
		out = append(out, e)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].namespace < out[b].namespace })
	return out, st.public
}

func (st *state) secret(e *entry) func(context.Context) (string, error) {
	return func(ctx context.Context) (string, error) {
		h := st.host.Load()
		if h == nil {
			return "", ErrCredential
		}
		values, err := h.Secrets(ctx, domain.ID(e.scope), e.settings.WebhookSecret)
		if err != nil || values[e.settings.WebhookSecret] == "" {
			return "", ErrCredential
		}
		return values[e.settings.WebhookSecret], nil
	}
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
	ext := sdk.New(Name, sdk.Version(Version), sdk.Command(protocol.HostRunCommand(Name)...),
		sdk.Summary("Signed webhooks where each delivery is Work of its own, deduplicated by a delivery key and threaded by a correlation key."))
	ext.Manifest.Provides.Webhooks = true
	ext.Manifest.Provides.Render = true
	ext.Manifest.Provides.Schemas = []protocol.SchemaDecl{{Name: "webhook.delivery", Versions: []int{1}}}
	ext.IngressType(Name, func(name string) (extension.Ingress, bool) {
		st.mu.Lock()
		defer st.mu.Unlock()
		e, ok := st.entries[name]
		if !ok {
			return nil, false
		}
		return e.ingress, true
	}, families()...)
	ext.Manifest.Title = "Webhook"
	ext.Manifest.Provides.Extensions[0].Fields = []protocol.FieldDecl{
		{Name: "scope", Help: "the scope the entry is in", Required: true},
		{Name: "webhook_secret", Help: "names a selected scope secret the hooks' URL tokens are derived from; rotating it revokes every hook's URL", Required: true},
		{Name: "hooks", Help: `[{"name": "tickets", "delivery_key": "header:Idempotency-Key", "correlation_key": "body:/ticket/id", "fan_out": ["triage", "audit"], "version": 0}]: each hook, where a delivery's identity (a retry is the same delivery) and thread (one Work, a successor once it finishes) are read, the targets each delivery goes to, and a version signed into its URL (bump it to revoke that URL alone)`, Required: true},
	}
	ext.Route(Name, Route{}, sdk.RouteConfig())
	ext.Manifest.Provides.Routes[len(ext.Manifest.Provides.Routes)-1].Fields = []protocol.FieldDecl{
		{Name: "hooks", Help: "the hooks the route takes (every hook of the scope's webhook entries when empty)"},
		{Name: "targets", Help: "the fan-out targets the route takes (every target when empty)"},
	}
	ext.WorkKind(WorkKind)
	registerTools(ext, o.Now)
	ext.Check(Name, func(ctx context.Context, _ *sdk.Host, _ bool) (string, error) { return st.check(ctx, false) })
	ext.Check(Name+"-urls", func(ctx context.Context, _ *sdk.Host, _ bool) (string, error) { return st.check(ctx, true) }, sdk.Deep())
	ext.Validate = func(_ context.Context, p protocol.ValidateParams) error {
		switch p.Kind {
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
		entries, _ := st.list()
		for _, e := range entries {
			rest, ok := strings.CutPrefix(path, "/"+e.namespace+"/")
			if !ok {
				continue
			}
			push := func(ctx context.Context, d extension.Delivery) (Receipt, error) {
				accepted, added, err := st.host.Load().DeliverAdded(ctx, e.namespace, protocol.FromDelivery(d))
				return ReceiptOf(accepted, added, err)
			}
			return http.StripPrefix("/"+e.namespace, WebhookHandler(e.namespace, e.settings, st.secret(e), push, o.Now)), rest != ""
		}
		return nil, false
	}
	ext.Render = RenderEvent
	return ext
}

// Manifest is the package manifest.
func Manifest() protocol.Manifest { return Build(Options{}).Manifest }

// configure applies one generation; an unchanged entry keeps its ingress.
func (st *state) configure(_ context.Context, h *sdk.Host, p protocol.ConfigureParams) (protocol.ConfigureResult, error) {
	var out protocol.ConfigureResult
	st.host.Store(h)
	st.mu.Lock()
	prev := st.entries
	st.mu.Unlock()
	next := map[string]*entry{}
	decl := []protocol.EventFamily{}
	for _, f := range families() {
		decl = append(decl, protocol.EventFamily{Type: f.Type, ReDerivable: f.ReDerivable, Loss: f.Loss})
	}
	for _, e := range p.Entries {
		if e.Type != Name {
			continue
		}
		settings, err := ParseSettings(e.Config, e.Secrets)
		if err != nil || settings.Scope != e.ScopeID {
			return out, protocol.Errorf(protocol.CodeInvalid, "invalid webhook entry")
		}
		label := e.Name
		if label == "" {
			label = "entry"
		}
		ns := Namespace(e.ScopeID, label)
		digest, _ := json.Marshal(e)
		if old, ok := prev[ns]; ok && old.config == string(digest) {
			next[ns] = old
		} else {
			next[ns] = &entry{namespace: ns, scope: e.ScopeID, config: string(digest), settings: settings,
				ingress: &Ingress{Namespace: ns, Settings: settings, Now: st.o.Now}}
		}
		out.Sources = append(out.Sources, protocol.SourceDecl{Name: ns, Source: protocol.Source{Namespace: ns, ScopeID: e.ScopeID}, IntervalSeconds: 3600, Families: decl})
		out.WebhookPaths = append(out.WebhookPaths, "/"+ns)
	}
	st.mu.Lock()
	st.entries, st.public = next, strings.TrimRight(p.PublicURL, "/")
	st.mu.Unlock()
	return out, nil
}

// check lists each hook's path (doctor's webhook check); with urls
// (webhook-urls, doctor --check-secrets) it prints each hook's signed URL,
// which is a capability derived from the entry's secret.
func (st *state) check(ctx context.Context, deep bool) (string, error) {
	entries, public := st.list()
	var items []string
	var failed error
	for _, e := range entries {
		for _, h := range e.settings.Hooks {
			token := "<token>"
			if deep {
				secret, err := st.secret(e)(ctx)
				if err != nil {
					items = append(items, fmt.Sprintf("%s: hook %s: webhook secret unavailable", e.namespace, h.Name))
					failed = errors.New("a webhook secret is unavailable")
					continue
				}
				token = Token(secret, e.namespace, h.Name, h.Version)
			}
			line := fmt.Sprintf("%s: hook %s: POST %s/%s/%s/%s", e.namespace, h.Name, public, e.namespace, h.Name, token)
			if !deep {
				line += " (doctor --check-secrets prints the URL)"
			}
			items = append(items, line)
		}
	}
	if len(items) == 0 {
		return "no webhook entries are configured", nil
	}
	return strings.Join(items, "; "), failed
}
