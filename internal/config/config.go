// Package config parses, validates and resolves version 1 configuration (B05).
// Validation never touches the network, resolves secrets or runs commands.
package config

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
)

// MaxSize bounds configuration, secrets files and command secret output reads.
const MaxSize = 1 << 20

var (
	idPattern  = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,62}$`)
	opPattern  = regexp.MustCompile(`^[a-z][a-z0-9_.-]*$`)
	envPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// Diagnostic paths are JSON pointers without the leading slash. Messages are
// safe remedies and never echo values or secret reference text.
type Diagnostic struct{ Path, Code, Message string }

func (d Diagnostic) String() string { return d.Path + ": " + d.Code + ": " + d.Message }

type ValidationError struct{ Diagnostics []Diagnostic }

func (e *ValidationError) Error() string {
	parts := make([]string, len(e.Diagnostics))
	for i, d := range e.Diagnostics {
		parts[i] = d.String()
	}
	return "invalid configuration: " + strings.Join(parts, "; ")
}
func invalid(diags []Diagnostic) error {
	sort.Slice(diags, func(i, j int) bool {
		if diags[i].Path != diags[j].Path {
			return diags[i].Path < diags[j].Path
		}
		return diags[i].Code < diags[j].Code
	})
	out := diags[:0]
	for i, d := range diags {
		if i == 0 || d != diags[i-1] {
			out = append(out, d)
		}
	}
	return &ValidationError{Diagnostics: out}
}

// ValidSecretRef reports whether ref is an accepted reference form; it never
// classifies a literal as a reference.
func ValidSecretRef(ref string) bool {
	scheme, rest, ok := strings.Cut(ref, ":")
	switch {
	case !ok:
		return false
	case scheme == "keyring", scheme == "file":
		return idPattern.MatchString(rest)
	case scheme == "env":
		return envPattern.MatchString(rest)
	case scheme == "cmd":
		return rest != "" && !strings.ContainsAny(rest, "\r\n")
	}
	return false
}

type Settings struct {
	Concurrency, AdmissionLimit, ReconcileSeconds, TimeoutSeconds, KeptExecutions int
	RetryDelaysSeconds                                                            []int
	CausalMaxDepth, CausalPerMinute                                               int
	// JobConcurrency bounds running job attempts (shell and other non-agent
	// executors) apart from Concurrency, which then bounds agent turns (#13).
	JobConcurrency int
	// Webhooks is the stored webhook listener and tunnel (settings.webhooks);
	// serve flags override it for one run. Zero means reconcile-only.
	Webhooks Webhooks
	// Budgets are the usage limits the host enforces at admission
	// (settings.budgets); none means no limit.
	Budgets []Budget
}

// Budget is one usage limit: at most Max of Unit per Work, per scope or
// across every scope (Per), over the Work's life or each UTC day or week
// (Period, "" for no window). Scope narrows it to Work in that one scope.
// WarnAt is the fraction of Max at which a warning event fires.
type Budget struct {
	Unit, Per, Period, Scope string
	Max, WarnAt              float64
}

// DefaultBudgetWarnAt is the warning threshold of a budget that sets none.
const DefaultBudgetWarnAt = 0.8

// MaxBudgets bounds settings.budgets.
const MaxBudgets = 64

// Webhooks records how serve exposes the webhook listener. Tunnel is ngrok,
// cloudflared or static; "" with PublicURL means static, as with the flags.
type Webhooks struct {
	Addr, Tunnel, TunnelDomain, TunnelName, TunnelHostname, PublicURL string
}
type Authorization struct {
	Connections []string `json:"connections"`
	Operations  []string `json:"operations"`
	ReadScopes  []string `json:"read_scopes"`
	// FileScopes are the other scopes whose trackers this scope's agents
	// may file issues in and comment on (optional; empty grants none).
	FileScopes []string `json:"file_scopes,omitempty"`
}
type Resource struct {
	Provider string          `json:"provider"`
	Config   json.RawMessage `json:"config"`
}

// Sandbox is a binding's process sandbox (B09): the provider and its
// opaque policy, checked by the provider.
type Sandbox struct {
	Provider string          `json:"provider"`
	Config   json.RawMessage `json:"config"`
}

// Container is a binding's container sandbox: the provider and its config,
// the grant (extension.ContainerConfig).
type Container struct {
	Provider string          `json:"provider"`
	Config   json.RawMessage `json:"config"`
}
type Binding struct {
	Name, Executor, Connection string
	Requires                   domain.Capabilities
	Operations, SecretNames    []string
	Resource                   *Resource
	// Sandbox runs the binding's executor in a process sandbox (B09).
	Sandbox *Sandbox
	// Container runs the binding's executor in a container. It can't be
	// combined with Sandbox.
	Container *Container
	// TimeoutSeconds overrides settings.timeout_seconds for attempts of this
	// binding (05-02): one deadline model for every executor, with a
	// per-binding value for long jobs or short turns. Zero inherits.
	TimeoutSeconds int
	Config         json.RawMessage
	// Grants are what the binding offers a Work beyond its operations and
	// config, which the Work opts into by name when it is created (for
	// example the shell an operator turns on for one setup session).
	Grants []Grant
}

// domainGrants are the binding's grants as domain.BindingGrant.
func (b Binding) domainGrants() []domain.BindingGrant {
	var out []domain.BindingGrant
	for _, g := range b.Grants {
		out = append(out, domain.BindingGrant{Name: g.Name, Label: g.Label, Operations: slices.Clone(g.Operations), Config: slices.Clone(g.Config)})
	}
	return out
}

// Operation reports whether the binding grants op itself or offers it in
// one of its grants: the ceiling a Work's toolbelt is checked against.
func (b Binding) Operation(op string) bool {
	return slices.Contains(b.Operations, op) || slices.ContainsFunc(b.Grants, func(g Grant) bool { return slices.Contains(g.Operations, op) })
}

// Grant is one grant a binding offers: more operations, within the scope
// authorization like the binding's own, and executor config whose top-level
// fields replace the binding config's.
type Grant struct {
	Name, Label string
	Operations  []string
	Config      json.RawMessage
}
type Route struct {
	Name, Handler, Binding, ActivePolicy, UnsupportedFallback, ErrorFallback string
	Config                                                                   json.RawMessage
}
type Extension struct {
	Name       string          `json:"name"`
	Type       string          `json:"type"`
	Connection string          `json:"connection,omitempty"`
	Config     json.RawMessage `json:"config"`
	// SecretNames selects scope secret names; never reference text.
	SecretNames []string `json:"secret_names"`
}

// Entries are either whole definitions or tombstones (Disabled with Name only).
type BindingEntry struct {
	Binding
	Disabled bool
}
type RouteEntry struct {
	Route
	Disabled bool
}
type ExtensionEntry struct {
	Extension
	Disabled bool
}
type Connection struct {
	ID, Type   string
	Config     json.RawMessage
	SecretRefs map[string]string
}
type Limits struct{ Concurrency, AdmissionLimit int } // zero means not narrowed
type ScopeConfig struct {
	ID, Name, Parent string
	Enabled          bool
	Limits           Limits
	Authorization    *Authorization // nil inherits
	Bindings         []BindingEntry
	Routes           []RouteEntry
	Extensions       []ExtensionEntry
	// SecretRefs overlays by name; an empty value is a false overlay.
	SecretRefs map[string]string
	// Source is the included folder this scope came from ("" for the config
	// file's own scopes). Only a stored generation records it.
	Source string
}

// Document is a strictly parsed version 1 configuration with defaults applied.
type Document struct {
	Version     int
	Settings    Settings
	Connections []Connection
	Scopes      []ScopeConfig
	// Includes are the folders a config file composes in (ParseFile only);
	// Compose replaces them with their scopes.
	Includes []Include
}

// Include is one folder whose .open-conduit/config.json adds scopes under
// Parent, a scope of the config file itself.
type Include struct{ Path, Parent string }

// Parse decodes a stored generation or a config file without includes
// strictly: duplicate keys, unknown fields, explicit null, bad types and
// unsupported versions all fail with *ValidationError. A scope may carry the
// source folder a stored generation recorded for it.
func Parse(data []byte) (*Document, error) { return parse(data, false) }

// ParseFile decodes a config file as an operator wrote it: it may list
// includes, and its scopes may not claim a source.
func ParseFile(data []byte) (*Document, error) { return parse(data, true) }

func parse(data []byte, file bool) (*Document, error) {
	root, diags := decode(data)
	if root == nil {
		return nil, invalid(diags)
	}
	c := &checker{diags: diags, file: file}
	doc := c.document(root)
	if len(c.diags) > 0 {
		return nil, invalid(c.diags)
	}
	return doc, nil
}
func (c *checker) document(n *node) *Document {
	doc := &Document{}
	if !c.object(n, "", []string{"version", "settings", "connections", "scopes"}, "includes") {
		return doc
	}
	if is := n.field("includes"); is != nil {
		if !c.file {
			c.fail("includes", "unknown_field", "includes belong in the config file; validate the file with its directory")
		} else if c.typed(is, "includes", kArray, "array") {
			for i, item := range is.items {
				p := ptr("includes", i)
				if !c.object(item, p, []string{"path", "parent"}) {
					continue
				}
				var inc Include
				if path, ok := c.str(item.field("path"), p+"/path", nil); ok && path == "" {
					c.fail(p+"/path", "invalid_value", "include path must be a folder")
				} else {
					inc.Path = path
				}
				inc.Parent, _ = c.str(item.field("parent"), p+"/parent", idPattern)
				doc.Includes = append(doc.Includes, inc)
			}
		}
	}
	if v := n.field("version"); v != nil && c.typed(v, "version", kNumber, "integer") {
		if v.num.String() != "1" {
			c.fail("version", "unsupported_version", "version must be exactly 1; convert the document explicitly")
		}
		doc.Version = 1
	}
	doc.Settings = c.settings(n.field("settings"))
	if cs := n.field("connections"); cs != nil && c.typed(cs, "connections", kArray, "array") {
		for i, item := range cs.items {
			doc.Connections = append(doc.Connections, c.connection(item, ptr("connections", i)))
		}
	}
	if ss := n.field("scopes"); ss != nil && c.typed(ss, "scopes", kArray, "array") {
		if len(ss.items) == 0 {
			c.fail("scopes", "invalid_value", "at least one scope is required")
		}
		for i, item := range ss.items {
			doc.Scopes = append(doc.Scopes, c.scope(item, ptr("scopes", i)))
		}
	}
	return doc
}
func (c *checker) settings(n *node) Settings {
	s := Settings{4, 10, 900, 3600, 20, []int{300, 600}, 32, 128, 4, Webhooks{}, nil}
	fields := []struct {
		key string
		dst *int
	}{{"concurrency", &s.Concurrency}, {"admission_limit", &s.AdmissionLimit}, {"reconcile_seconds", &s.ReconcileSeconds},
		{"timeout_seconds", &s.TimeoutSeconds}, {"kept_executions", &s.KeptExecutions},
		{"causal_max_depth", &s.CausalMaxDepth}, {"causal_per_minute", &s.CausalPerMinute},
		{"job_concurrency", &s.JobConcurrency}}
	names := []string{"retry_delays_seconds", "webhooks", "budgets"}
	for _, f := range fields {
		names = append(names, f.key)
	}
	if n == nil || !c.object(n, "settings", nil, names...) {
		return s
	}
	for _, f := range fields {
		if v, ok := c.integer(n.field(f.key), ptr("settings", f.key)); ok {
			*f.dst = v
		}
	}
	if r := n.field("retry_delays_seconds"); r != nil && c.typed(r, "settings/retry_delays_seconds", kArray, "array") {
		if len(r.items) > 100 {
			c.fail("settings/retry_delays_seconds", "invalid_value", "at most 100 retry delays are allowed")
		}
		s.RetryDelaysSeconds = []int{}
		for i, item := range r.items {
			if v, ok := c.integer(item, ptr("settings/retry_delays_seconds", i)); ok {
				s.RetryDelaysSeconds = append(s.RetryDelaysSeconds, v)
			}
		}
	}
	s.Webhooks = c.webhooks(n.field("webhooks"))
	s.Budgets = c.budgets(n.field("budgets"))
	return s
}

// budgets checks settings.budgets offline; that a named scope exists is
// checked with the scope graph.
func (c *checker) budgets(n *node) []Budget {
	const path = "settings/budgets"
	if n == nil || !c.typed(n, path, kArray, "array") {
		return nil
	}
	if len(n.items) > MaxBudgets {
		c.fail(path, "invalid_value", fmt.Sprintf("at most %d budgets are allowed", MaxBudgets))
	}
	out := []Budget{}
	seen := map[[4]string]bool{}
	for i, item := range n.items {
		p := ptr(path, i)
		if !c.object(item, p, []string{"unit", "per", "max"}, "period", "warn_at", "scope") {
			continue
		}
		b := Budget{WarnAt: DefaultBudgetWarnAt}
		b.Unit, _ = c.str(item.field("unit"), p+"/unit", extension.UnitPattern)
		b.Per, _ = c.enum(item.field("per"), p+"/per", "work", "scope", "all")
		if v := item.field("period"); v != nil {
			b.Period, _ = c.enum(v, p+"/period", "day", "week")
		}
		if v := item.field("scope"); v != nil {
			b.Scope, _ = c.str(v, p+"/scope", idPattern)
		}
		if v, ok := c.number(item.field("max"), p+"/max"); ok {
			if v <= 0 {
				c.fail(p+"/max", "invalid_value", "max must be greater than 0")
			}
			b.Max = v
		}
		if v := item.field("warn_at"); v != nil {
			if w, ok := c.number(v, p+"/warn_at"); ok {
				if w <= 0 || w >= 1 {
					c.fail(p+"/warn_at", "invalid_value", "warn_at is a fraction of max, greater than 0 and less than 1")
				}
				b.WarnAt = w
			}
		}
		if b.Per == "all" && b.Scope != "" {
			c.fail(p+"/scope", "invalid_value", "a per: all budget counts every scope; use per: scope to limit one scope")
		}
		key := [4]string{b.Unit, b.Per, b.Period, b.Scope}
		if seen[key] {
			c.fail(p, "duplicate_value", "another budget already limits this unit, per and period for the same scope; merge them")
		}
		seen[key] = true
		out = append(out, b)
	}
	return out
}

// webhooks checks settings.webhooks offline: the same combinations serve
// accepts as flags, without binding the address or starting a tunnel.
func (c *checker) webhooks(n *node) Webhooks {
	const path = "settings/webhooks"
	var w Webhooks
	fields := []struct {
		key string
		dst *string
	}{{"addr", &w.Addr}, {"tunnel_domain", &w.TunnelDomain}, {"tunnel_name", &w.TunnelName}, {"tunnel_hostname", &w.TunnelHostname}, {"public_url", &w.PublicURL}}
	names := []string{"tunnel"}
	for _, f := range fields {
		names = append(names, f.key)
	}
	if n == nil || !c.object(n, path, []string{"addr"}, names...) {
		return w
	}
	for _, f := range fields {
		if v, ok := c.str(n.field(f.key), ptr(path, f.key), nil); ok {
			*f.dst = v
		}
	}
	if t := n.field("tunnel"); t != nil {
		w.Tunnel, _ = c.enum(t, path+"/tunnel", "ngrok", "cloudflared", "static")
	}
	if a := n.field("addr"); a != nil && a.kind == kString {
		if _, port, err := net.SplitHostPort(w.Addr); err != nil || port == "" {
			c.fail(path+"/addr", "invalid_value", "use host:port, for example 127.0.0.1:8787")
		}
	}
	kind := w.Tunnel
	if kind == "" && w.PublicURL != "" {
		kind = "static"
	}
	if w.TunnelDomain != "" && kind != "ngrok" {
		c.fail(path+"/tunnel_domain", "invalid_value", "tunnel_domain is for the ngrok tunnel")
	}
	if (w.TunnelName != "" || w.TunnelHostname != "") && kind != "cloudflared" {
		c.fail(path+"/tunnel_name", "invalid_value", "tunnel_name and tunnel_hostname are for the cloudflared tunnel")
	} else if kind == "cloudflared" && (w.TunnelName == "") != (w.TunnelHostname == "") {
		c.fail(path+"/tunnel_name", "invalid_value", "a named cloudflared tunnel needs both tunnel_name and tunnel_hostname")
	}
	if w.PublicURL != "" && kind != "static" {
		c.fail(path+"/public_url", "invalid_value", "public_url is for the static tunnel")
	} else if kind == "static" {
		if u, err := url.Parse(w.PublicURL); err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil {
			c.fail(path+"/public_url", "invalid_value", "the static tunnel needs an http(s) public URL without credentials")
		}
	}
	return w
}
func (c *checker) secretRefs(n *node, path string, overlay bool) map[string]string {
	if n == nil || !c.typed(n, path, kObject, "object") {
		return nil
	}
	refs := map[string]string{}
	for _, k := range n.keys {
		p, v := ptr(path, k), n.obj[k]
		if !idPattern.MatchString(k) {
			c.fail(p, "invalid_value", "secret name does not match the identifier pattern")
			continue
		}
		switch {
		case overlay && v.kind == kBool && !v.b:
			refs[k] = ""
		case v.kind == kString && ValidSecretRef(v.str):
			refs[k] = v.str
		case v.kind == kString || v.kind == kBool:
			c.fail(p, "invalid_value", "use keyring:name, file:name, env:NAME or cmd:command (no literals or line breaks)")
		case v.kind != kNull:
			c.fail(p, "invalid_type", "expected secret reference string")
		}
	}
	return refs
}
func (c *checker) connection(n *node, path string) Connection {
	var out Connection
	if !c.object(n, path, []string{"id", "type", "config"}, "secret_refs") {
		return out
	}
	out.ID, _ = c.str(n.field("id"), path+"/id", idPattern)
	out.Type, _ = c.str(n.field("type"), path+"/type", idPattern)
	out.Config = c.opaque(n.field("config"), path+"/config")
	out.SecretRefs = c.secretRefs(n.field("secret_refs"), path+"/secret_refs", false)
	return out
}
func (c *checker) scope(n *node, path string) ScopeConfig {
	out := ScopeConfig{Enabled: true}
	optional := []string{"parent", "enabled", "limits", "authorization", "bindings", "routes", "extensions"}
	switch {
	case c.fragment:
		// A folder's scopes select secret names their parent provides; the
		// references (and any cmd: they run) stay in the config file.
	case c.file:
		optional = append(optional, "secret_refs")
	default:
		optional = append(optional, "secret_refs", "source")
	}
	if !c.object(n, path, []string{"id", "name"}, optional...) {
		return out
	}
	if s := n.field("source"); s != nil {
		out.Source, _ = c.str(s, path+"/source", nil)
	}
	out.ID, _ = c.str(n.field("id"), path+"/id", idPattern)
	if name, ok := c.str(n.field("name"), path+"/name", nil); ok && name == "" {
		c.fail(path+"/name", "invalid_value", "scope name must be nonempty")
	} else {
		out.Name = name
	}
	if p := n.field("parent"); p != nil {
		out.Parent, _ = c.str(p, path+"/parent", idPattern)
	}
	if e := n.field("enabled"); e != nil {
		out.Enabled, _ = c.boolean(e, path+"/enabled")
	}
	if l := n.field("limits"); l != nil && c.object(l, path+"/limits", nil, "concurrency", "admission_limit") {
		out.Limits.Concurrency, _ = c.integer(l.field("concurrency"), path+"/limits/concurrency")
		out.Limits.AdmissionLimit, _ = c.integer(l.field("admission_limit"), path+"/limits/admission_limit")
	}
	if a := n.field("authorization"); a != nil {
		p := path + "/authorization"
		out.Authorization = &Authorization{[]string{}, []string{}, []string{}, []string{}}
		if c.object(a, p, []string{"connections", "operations", "read_scopes"}, "file_scopes") {
			out.Authorization = &Authorization{
				Connections: c.list(a.field("connections"), p+"/connections", idPattern),
				Operations:  c.list(a.field("operations"), p+"/operations", opPattern),
				ReadScopes:  c.list(a.field("read_scopes"), p+"/read_scopes", idPattern),
				FileScopes:  c.list(a.field("file_scopes"), p+"/file_scopes", idPattern),
			}
		}
	}
	entries := func(key string, each func(*node, string)) {
		if list := n.field(key); list != nil && c.typed(list, path+"/"+key, kArray, "array") {
			for i, item := range list.items {
				each(item, ptr(path+"/"+key, i))
			}
		}
	}
	entries("bindings", func(item *node, p string) { out.Bindings = append(out.Bindings, c.binding(item, p)) })
	entries("routes", func(item *node, p string) { out.Routes = append(out.Routes, c.route(item, p)) })
	entries("extensions", func(item *node, p string) { out.Extensions = append(out.Extensions, c.extension(item, p)) })
	if out.Bindings == nil && n.field("bindings") != nil {
		out.Bindings = []BindingEntry{}
	}
	if out.Routes == nil && n.field("routes") != nil {
		out.Routes = []RouteEntry{}
	}
	if out.Extensions == nil && n.field("extensions") != nil {
		out.Extensions = []ExtensionEntry{}
	}
	out.SecretRefs = c.secretRefs(n.field("secret_refs"), path+"/secret_refs", true)
	return out
}

// tombstone checks {"name","disabled":true}; ok reports whether n is one.
func (c *checker) tombstone(n *node, path string) (string, bool) {
	d := n.field("disabled")
	if d == nil {
		return "", false
	}
	c.object(n, path, []string{"name", "disabled"})
	if v, ok := c.boolean(d, path+"/disabled"); ok && !v {
		c.fail(path+"/disabled", "invalid_value", "disabled must be true; omit it to define the entry")
	}
	name, _ := c.str(n.field("name"), path+"/name", idPattern)
	return name, true
}
func (c *checker) binding(n *node, path string) BindingEntry {
	if !c.typed(n, path, kObject, "object") {
		return BindingEntry{}
	}
	if name, ok := c.tombstone(n, path); ok {
		return BindingEntry{Binding: Binding{Name: name}, Disabled: true}
	}
	var b Binding
	if !c.object(n, path, []string{"name", "executor", "requires", "operations", "secret_names", "config"}, "connection", "resource", "sandbox", "container", "timeout_seconds", "grants") {
		return BindingEntry{Binding: b}
	}
	b.Name, _ = c.str(n.field("name"), path+"/name", idPattern)
	b.Executor, _ = c.str(n.field("executor"), path+"/executor", idPattern)
	if conn := n.field("connection"); conn != nil {
		b.Connection, _ = c.str(conn, path+"/connection", idPattern)
	}
	if r := n.field("requires"); r != nil && c.object(r, path+"/requires", nil, "interrupt", "inject", "resume", "workspace", "toolbelt") {
		p := path + "/requires"
		b.Requires.Interrupt, _ = c.boolean(r.field("interrupt"), p+"/interrupt")
		b.Requires.Inject, _ = c.boolean(r.field("inject"), p+"/inject")
		b.Requires.Resume, _ = c.boolean(r.field("resume"), p+"/resume")
		b.Requires.Workspace, _ = c.boolean(r.field("workspace"), p+"/workspace")
		if t := r.field("toolbelt"); t != nil {
			b.Requires.Toolbelt, _ = c.enum(t, p+"/toolbelt", "none", "cli", "mcp", "sdk")
		}
	}
	b.Operations = c.list(n.field("operations"), path+"/operations", opPattern)
	b.SecretNames = c.list(n.field("secret_names"), path+"/secret_names", idPattern)
	if r := n.field("resource"); r != nil && c.object(r, path+"/resource", []string{"provider", "config"}) {
		b.Resource = &Resource{}
		b.Resource.Provider, _ = c.str(r.field("provider"), path+"/resource/provider", idPattern)
		b.Resource.Config = c.opaque(r.field("config"), path+"/resource/config")
	}
	if r := n.field("sandbox"); r != nil && c.object(r, path+"/sandbox", []string{"provider", "config"}) {
		b.Sandbox = &Sandbox{}
		b.Sandbox.Provider, _ = c.str(r.field("provider"), path+"/sandbox/provider", idPattern)
		b.Sandbox.Config = c.opaque(r.field("config"), path+"/sandbox/config")
	}
	if r := n.field("container"); r != nil && c.object(r, path+"/container", []string{"provider", "config"}) {
		b.Container = &Container{}
		b.Container.Provider, _ = c.str(r.field("provider"), path+"/container/provider", idPattern)
		b.Container.Config = c.opaque(r.field("config"), path+"/container/config")
	}
	if t := n.field("timeout_seconds"); t != nil {
		b.TimeoutSeconds, _ = c.integer(t, path+"/timeout_seconds")
	}
	b.Config = c.opaque(n.field("config"), path+"/config")
	if g := n.field("grants"); g != nil && c.typed(g, path+"/grants", kArray, "array") {
		for i, item := range g.items {
			p := ptr(path+"/grants", i)
			if !c.object(item, p, []string{"name"}, "label", "operations", "config") {
				continue
			}
			var grant Grant
			grant.Name, _ = c.str(item.field("name"), p+"/name", idPattern)
			if l := item.field("label"); l != nil {
				grant.Label, _ = c.str(l, p+"/label", nil)
			}
			grant.Operations = c.list(item.field("operations"), p+"/operations", opPattern)
			if cfg := item.field("config"); cfg != nil {
				grant.Config = c.opaque(cfg, p+"/config")
			}
			if slices.ContainsFunc(b.Grants, func(o Grant) bool { return o.Name == grant.Name }) {
				c.fail(p+"/name", "duplicate_name", "grant name is repeated in this binding; keep one")
			}
			b.Grants = append(b.Grants, grant)
		}
	}
	return BindingEntry{Binding: b}
}
func (c *checker) route(n *node, path string) RouteEntry {
	if !c.typed(n, path, kObject, "object") {
		return RouteEntry{}
	}
	if name, ok := c.tombstone(n, path); ok {
		return RouteEntry{Route: Route{Name: name}, Disabled: true}
	}
	var r Route
	if !c.object(n, path, []string{"name", "handler", "binding", "active_policy", "config"}, "unsupported_fallback", "error_fallback") {
		return RouteEntry{Route: r}
	}
	r.Name, _ = c.str(n.field("name"), path+"/name", idPattern)
	r.Handler, _ = c.str(n.field("handler"), path+"/handler", idPattern)
	r.Binding, _ = c.str(n.field("binding"), path+"/binding", idPattern)
	r.ActivePolicy, _ = c.enum(n.field("active_policy"), path+"/active_policy", "queue", "drop", "inject", "interrupt")
	if f := n.field("unsupported_fallback"); f != nil {
		r.UnsupportedFallback, _ = c.enum(f, path+"/unsupported_fallback", "queue", "drop")
	}
	if f := n.field("error_fallback"); f != nil {
		r.ErrorFallback, _ = c.enum(f, path+"/error_fallback", "queue", "drop")
	}
	r.Config = c.opaque(n.field("config"), path+"/config")
	return RouteEntry{Route: r}
}
func (c *checker) extension(n *node, path string) ExtensionEntry {
	if !c.typed(n, path, kObject, "object") {
		return ExtensionEntry{}
	}
	if name, ok := c.tombstone(n, path); ok {
		return ExtensionEntry{Extension: Extension{Name: name}, Disabled: true}
	}
	var e Extension
	if !c.object(n, path, []string{"name", "type", "config", "secret_names"}, "connection") {
		return ExtensionEntry{Extension: e}
	}
	e.Name, _ = c.str(n.field("name"), path+"/name", idPattern)
	e.Type, _ = c.str(n.field("type"), path+"/type", idPattern)
	if conn := n.field("connection"); conn != nil {
		e.Connection, _ = c.str(conn, path+"/connection", idPattern)
	}
	e.Config = c.opaque(n.field("config"), path+"/config")
	e.SecretNames = c.list(n.field("secret_names"), path+"/secret_names", idPattern)
	return ExtensionEntry{Extension: e}
}

// normalized renders the canonical document; redact replaces reference text.
func (d *Document) normalized(redact bool) any {
	refs := func(m map[string]string) map[string]any {
		out := make(map[string]any, len(m))
		for k, v := range m {
			switch {
			case v == "":
				out[k] = false
			case redact:
				out[k] = "[redacted]"
			default:
				out[k] = v
			}
		}
		return out
	}
	s := d.Settings
	connections := []any{}
	for _, c := range d.Connections {
		m := map[string]any{"id": c.ID, "type": c.Type, "config": c.Config}
		if c.SecretRefs != nil {
			m["secret_refs"] = refs(c.SecretRefs)
		}
		connections = append(connections, m)
	}
	scopes := []any{}
	for _, sc := range d.Scopes {
		m := map[string]any{"id": sc.ID, "name": sc.Name, "enabled": sc.Enabled}
		if sc.Parent != "" {
			m["parent"] = sc.Parent
		}
		limits := map[string]any{}
		if sc.Limits.Concurrency > 0 {
			limits["concurrency"] = sc.Limits.Concurrency
		}
		if sc.Limits.AdmissionLimit > 0 {
			limits["admission_limit"] = sc.Limits.AdmissionLimit
		}
		if len(limits) > 0 {
			m["limits"] = limits
		}
		if sc.Authorization != nil {
			m["authorization"] = sc.Authorization
		}
		if sc.Bindings != nil {
			list := []any{}
			for _, b := range sc.Bindings {
				list = append(list, b.normalized())
			}
			m["bindings"] = list
		}
		if sc.Routes != nil {
			list := []any{}
			for _, r := range sc.Routes {
				list = append(list, r.normalized())
			}
			m["routes"] = list
		}
		if sc.Extensions != nil {
			list := []any{}
			for _, e := range sc.Extensions {
				if e.Disabled {
					list = append(list, map[string]any{"name": e.Name, "disabled": true})
				} else {
					list = append(list, e.Extension)
				}
			}
			m["extensions"] = list
		}
		if sc.SecretRefs != nil {
			m["secret_refs"] = refs(sc.SecretRefs)
		}
		if sc.Source != "" {
			m["source"] = sc.Source
		}
		scopes = append(scopes, m)
	}
	settings := map[string]any{
		"concurrency": s.Concurrency, "admission_limit": s.AdmissionLimit, "reconcile_seconds": s.ReconcileSeconds,
		"timeout_seconds": s.TimeoutSeconds, "kept_executions": s.KeptExecutions, "retry_delays_seconds": s.RetryDelaysSeconds,
		"causal_max_depth": s.CausalMaxDepth, "causal_per_minute": s.CausalPerMinute, "job_concurrency": s.JobConcurrency,
	}
	// Only configured budgets join the hash, so configurations without them
	// keep their generation hash.
	if len(s.Budgets) > 0 {
		list := []any{}
		for _, b := range s.Budgets {
			m := map[string]any{"unit": b.Unit, "per": b.Per, "max": b.Max, "warn_at": b.WarnAt}
			if b.Period != "" {
				m["period"] = b.Period
			}
			if b.Scope != "" {
				m["scope"] = b.Scope
			}
			list = append(list, m)
		}
		settings["budgets"] = list
	}
	// Only a stored listener joins the hash, so configurations without one
	// keep their generation hash.
	if w := s.Webhooks; w != (Webhooks{}) {
		hooks := map[string]any{"addr": w.Addr}
		for k, v := range map[string]string{"tunnel": w.Tunnel, "tunnel_domain": w.TunnelDomain, "tunnel_name": w.TunnelName, "tunnel_hostname": w.TunnelHostname, "public_url": w.PublicURL} {
			if v != "" {
				hooks[k] = v
			}
		}
		settings["webhooks"] = hooks
	}
	return map[string]any{
		"version":     d.Version,
		"settings":    settings,
		"connections": connections,
		"scopes":      scopes,
	}
}
func (b BindingEntry) normalized() any {
	if b.Disabled {
		return map[string]any{"name": b.Name, "disabled": true}
	}
	r := b.Requires
	requires := map[string]any{"interrupt": r.Interrupt, "inject": r.Inject, "resume": r.Resume, "workspace": r.Workspace}
	if r.Toolbelt != "" {
		requires["toolbelt"] = r.Toolbelt
	}
	m := map[string]any{"name": b.Name, "executor": b.Executor, "requires": requires,
		"operations": b.Operations, "secret_names": b.SecretNames, "config": b.Config}
	if b.Connection != "" {
		m["connection"] = b.Connection
	}
	if b.Resource != nil {
		m["resource"] = b.Resource
	}
	if b.Sandbox != nil {
		m["sandbox"] = b.Sandbox
	}
	if b.Container != nil {
		m["container"] = b.Container
	}
	if b.TimeoutSeconds > 0 {
		m["timeout_seconds"] = b.TimeoutSeconds
	}
	if len(b.Grants) > 0 {
		grants := make([]any, 0, len(b.Grants))
		for _, g := range b.Grants {
			gm := map[string]any{"name": g.Name, "operations": g.Operations}
			if g.Label != "" {
				gm["label"] = g.Label
			}
			if g.Config != nil {
				gm["config"] = g.Config
			}
			grants = append(grants, gm)
		}
		m["grants"] = grants
	}
	return m
}
func (r RouteEntry) normalized() any {
	if r.Disabled {
		return map[string]any{"name": r.Name, "disabled": true}
	}
	m := map[string]any{"name": r.Name, "handler": r.Handler, "binding": r.Binding, "active_policy": r.ActivePolicy, "config": r.Config}
	if r.UnsupportedFallback != "" {
		m["unsupported_fallback"] = r.UnsupportedFallback
	}
	if r.ErrorFallback != "" {
		m["error_fallback"] = r.ErrorFallback
	}
	return m
}

// Normalize returns the canonical normalized document and its hash.
func (d *Document) Normalize() (json.RawMessage, string) {
	data := canonical(d.normalized(false))
	return data, hashOf(data)
}
func hashOf(data []byte) string { return fmt.Sprintf("sha256:%x", sha256.Sum256(data)) }
