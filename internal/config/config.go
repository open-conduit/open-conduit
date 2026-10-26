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
	"sort"
	"strings"

	"github.com/open-conduit/open-conduit/domain"
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
	// Webhooks is the stored webhook listener and tunnel (settings.webhooks);
	// serve flags override it for one run. Zero means reconcile-only.
	Webhooks Webhooks
}

// Webhooks records how serve exposes the webhook listener. Tunnel is ngrok,
// cloudflared or static; "" with PublicURL means static, as with the flags.
type Webhooks struct {
	Addr, Tunnel, TunnelDomain, TunnelName, TunnelHostname, PublicURL string
}
type Authorization struct {
	Connections []string `json:"connections"`
	Operations  []string `json:"operations"`
	ReadScopes  []string `json:"read_scopes"`
}
type Resource struct {
	Provider string          `json:"provider"`
	Config   json.RawMessage `json:"config"`
}
type Binding struct {
	Name, Executor, Connection string
	Requires                   domain.Capabilities
	Operations, SecretNames    []string
	Resource                   *Resource
	// TimeoutSeconds overrides settings.timeout_seconds for attempts of this
	// binding (05-02): one deadline model for every executor, with a
	// per-binding value for long jobs or short turns. Zero inherits.
	TimeoutSeconds int
	Config         json.RawMessage
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
}

// Document is a strictly parsed version 1 configuration with defaults applied.
type Document struct {
	Version     int
	Settings    Settings
	Connections []Connection
	Scopes      []ScopeConfig
}

// Parse decodes strictly: duplicate keys, unknown fields, explicit null, bad
// types and unsupported versions all fail with *ValidationError.
func Parse(data []byte) (*Document, error) {
	root, diags := decode(data)
	if root == nil {
		return nil, invalid(diags)
	}
	c := &checker{diags: diags}
	doc := c.document(root)
	if len(c.diags) > 0 {
		return nil, invalid(c.diags)
	}
	return doc, nil
}
func (c *checker) document(n *node) *Document {
	doc := &Document{}
	if !c.object(n, "", []string{"version", "settings", "connections", "scopes"}) {
		return doc
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
	s := Settings{4, 10, 900, 3600, 20, []int{300, 600}, 32, 128, Webhooks{}}
	fields := []struct {
		key string
		dst *int
	}{{"concurrency", &s.Concurrency}, {"admission_limit", &s.AdmissionLimit}, {"reconcile_seconds", &s.ReconcileSeconds},
		{"timeout_seconds", &s.TimeoutSeconds}, {"kept_executions", &s.KeptExecutions},
		{"causal_max_depth", &s.CausalMaxDepth}, {"causal_per_minute", &s.CausalPerMinute}}
	names := []string{"retry_delays_seconds", "webhooks"}
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
	return s
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
	if !c.object(n, path, []string{"id", "name"}, "parent", "enabled", "limits", "authorization", "bindings", "routes", "extensions", "secret_refs") {
		return out
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
		out.Authorization = &Authorization{[]string{}, []string{}, []string{}}
		if c.object(a, p, []string{"connections", "operations", "read_scopes"}) {
			out.Authorization = &Authorization{
				Connections: c.list(a.field("connections"), p+"/connections", idPattern),
				Operations:  c.list(a.field("operations"), p+"/operations", opPattern),
				ReadScopes:  c.list(a.field("read_scopes"), p+"/read_scopes", idPattern),
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
	if !c.object(n, path, []string{"name", "executor", "requires", "operations", "secret_names", "config"}, "connection", "resource", "timeout_seconds") {
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
	if t := n.field("timeout_seconds"); t != nil {
		b.TimeoutSeconds, _ = c.integer(t, path+"/timeout_seconds")
	}
	b.Config = c.opaque(n.field("config"), path+"/config")
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
		scopes = append(scopes, m)
	}
	settings := map[string]any{
		"concurrency": s.Concurrency, "admission_limit": s.AdmissionLimit, "reconcile_seconds": s.ReconcileSeconds,
		"timeout_seconds": s.TimeoutSeconds, "kept_executions": s.KeptExecutions, "retry_delays_seconds": s.RetryDelaysSeconds,
		"causal_max_depth": s.CausalMaxDepth, "causal_per_minute": s.CausalPerMinute,
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
	if b.TimeoutSeconds > 0 {
		m["timeout_seconds"] = b.TimeoutSeconds
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
