package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
)

// ErrDenied is returned when current configuration does not authorize a call.
var ErrDenied = errors.New("configuration authority denied")

// Builtins are always registered toolbelt operations.
var Builtins = []string{"emit", "create_work", "wait", "declare_outcome", "read_context"}

// Registry is supplied in-process by first-party registration.
type Registry struct {
	Executors   map[string]extension.Executor
	Handlers    map[string]bool
	Extensions  map[string]ExtensionType
	Connections map[string]func(config json.RawMessage) error // nil func accepts any object
	Operations  []string
	Resources   map[string]bool
	// ResourceConfig optionally validates a registered provider's binding
	// resource config at load/apply time. Its error text is never echoed.
	ResourceConfig map[string]func(json.RawMessage) error
	// HandlerConfig optionally validates a registered route handler's opaque
	// route config at load/apply time (05-01). Its error text is never echoed.
	HandlerConfig map[string]func(json.RawMessage) error
	// HandlerBinding optionally checks the binding a route with this handler
	// binds (extension.RouteBinding). Its error is a fixed, secret-free
	// reason, shown in the diagnostic.
	HandlerBinding map[string]func(extension.RouteBinding) error
	// HandlerDescribe optionally says in words which events a route with
	// this handler and config takes, for the operator's view of the config.
	HandlerDescribe map[string]func(json.RawMessage) string
	// ResourceSettings optionally lists a resource provider's config fields
	// that are safe to show the operator (extension.Setting). Executors
	// declare theirs by implementing extension.SettingsDescriber.
	ResourceSettings map[string]func(json.RawMessage) []extension.Setting
}

// ExtensionType Ingress entries must target their containing scope.
type ExtensionType struct {
	Ingress  bool
	Validate func(config json.RawMessage, secretNames []string) error
	// Describe optionally lists what an entry's config sets up (a schedule's
	// jobs and when they run), for the operator's view of the config. It
	// must not reveal secrets.
	Describe func(config json.RawMessage) []Entry
	// Settings optionally lists the entry's config fields that are safe to
	// show the operator, with typed values. Fields it leaves out are shown
	// by name alone.
	Settings func(config json.RawMessage) []extension.Setting
}

// Entry is one thing an extension entry sets up, named and described.
type Entry struct{ Name, Detail string }

type EffectiveScope struct {
	ID, ParentID                domain.ID
	Name                        string
	Enabled                     bool
	Concurrency, AdmissionLimit int
	Authorization               Authorization
	Bindings                    []Binding
	Routes                      []Route
	Extensions                  []Extension
	SecretRefs                  map[string]string
	// Source is the included folder the scope came from, or "".
	Source string
}

// Snapshot is one immutable validated generation candidate. Normalized
// contains secret reference text: never log or return it.
type Snapshot struct {
	Hash       string
	Normalized json.RawMessage
	Settings   Settings
	Order      []domain.ID
	Scopes     map[domain.ID]*EffectiveScope
	// Connections contain secret reference text: never log or return them.
	Connections []Connection
	redacted    json.RawMessage
}

// slots implement B04 stable slots: replacement keeps the ancestor position.
type slot[T any] struct {
	value        T
	disabled     bool
	scope, index int
}
type slots[T any] struct {
	names  []string
	byName map[string]slot[T]
}

func (s slots[T]) clone() slots[T] {
	return slots[T]{slices.Clone(s.names), maps.Clone(s.byName)}
}
func (s *slots[T]) put(name string, v slot[T]) {
	if s.byName == nil {
		s.byName = map[string]slot[T]{}
	}
	if _, ok := s.byName[name]; !ok {
		s.names = append(s.names, name)
	}
	s.byName[name] = v
}
func (s slots[T]) active() []T {
	var out []T
	for _, n := range s.names {
		if v := s.byName[n]; !v.disabled {
			out = append(out, v.value)
		}
	}
	return out
}

type effective struct {
	enabled     bool
	concurrency int
	admission   int
	auth        Authorization
	refs        map[string]string
	bindings    slots[Binding]
	routes      slots[Route]
	extensions  slots[Extension]
}

type validator struct {
	doc         *Document
	reg         Registry
	diags       []Diagnostic
	connections map[string]bool
	index       map[string]int
	state       map[string]*effective
	visiting    map[string]bool
	broken      map[string]bool
}

func (v *validator) fail(path, code, msg string) {
	v.diags = append(v.diags, Diagnostic{path, code, msg})
}

// Validate runs parse → defaults → scope graph → inheritance → registration
// and owner checks → references/authorization → capabilities, returning every
// independent diagnostic as *ValidationError.
func Validate(data []byte, reg Registry) (*Snapshot, error) {
	doc, err := Parse(data)
	if err != nil {
		return nil, err
	}
	return ValidateDocument(doc, reg)
}
func ValidateDocument(doc *Document, reg Registry) (*Snapshot, error) {
	v := &validator{doc: doc, reg: reg, connections: map[string]bool{}, index: map[string]int{},
		state: map[string]*effective{}, visiting: map[string]bool{}, broken: map[string]bool{}}
	for i, c := range doc.Connections {
		p := ptr("connections", i)
		if v.connections[c.ID] {
			v.fail(p+"/id", "duplicate_id", "connection id is already defined; use a unique id")
		}
		v.connections[c.ID] = true
		check, ok := reg.Connections[c.Type]
		if !ok {
			v.fail(p+"/type", "unknown_registration", "connection type is not registered; use a registered type")
		} else if check != nil && check(c.Config) != nil {
			v.fail(p+"/config", "invalid_config", "connection config rejected by its registered owner; fix it against the owner schema")
		}
	}
	for i, s := range doc.Scopes {
		if _, ok := v.index[s.ID]; ok {
			v.fail(ptr("scopes", i)+"/id", "duplicate_id", "scope id is already defined; use a unique id")
			continue
		}
		v.index[s.ID] = i
	}
	for _, s := range doc.Scopes {
		v.resolve(s.ID)
	}
	if len(v.diags) > 0 {
		return nil, invalid(v.diags)
	}
	return v.snapshot(), nil
}

// resolve returns nil for cyclic scopes or descendants of a cycle.
func (v *validator) resolve(id string) *effective {
	if e, ok := v.state[id]; ok || v.broken[id] {
		return e
	}
	i := v.index[id]
	sc, path := v.doc.Scopes[i], ptr("scopes", i)
	if v.visiting[id] {
		v.fail(path+"/parent", "scope_cycle", "scope parents form a cycle; choose an acyclic parent")
		v.broken[id] = true
		return nil
	}
	var parent *effective
	if sc.Parent != "" {
		if _, ok := v.index[sc.Parent]; !ok {
			v.fail(path+"/parent", "unknown_scope", "parent scope is not defined; define it or remove the parent")
		} else {
			v.visiting[id] = true
			parent = v.resolve(sc.Parent)
			delete(v.visiting, id)
			if parent == nil {
				v.broken[id] = true
				return nil
			}
		}
	}
	e := &effective{enabled: true, concurrency: v.doc.Settings.Concurrency, admission: v.doc.Settings.AdmissionLimit,
		auth: Authorization{[]string{}, []string{}, []string{}}, refs: map[string]string{}}
	if parent != nil {
		e = &effective{parent.enabled, parent.concurrency, parent.admission, parent.auth, maps.Clone(parent.refs),
			parent.bindings.clone(), parent.routes.clone(), parent.extensions.clone()}
	}
	e.enabled = e.enabled && sc.Enabled
	if a := sc.Authorization; a != nil {
		categories := []struct {
			key                string
			requested, current []string
			known              func(string) bool
		}{
			{"connections", a.Connections, e.auth.Connections, func(s string) bool { return v.connections[s] }},
			{"operations", a.Operations, e.auth.Operations, v.operation},
			{"read_scopes", a.ReadScopes, e.auth.ReadScopes, func(s string) bool { _, ok := v.index[s]; return ok }},
		}
		for _, c := range categories {
			p := path + "/authorization/" + c.key
			if parent != nil && !subset(c.requested, c.current) {
				v.fail(p, "authority_widening", "child authorization must be a subset of the parent's effective "+c.key)
			}
			if unknown := missing(c.requested, c.known); len(unknown) > 0 {
				v.fail(p, "unknown_reference", "authorization names an undefined or unregistered "+c.key+" entry: "+strings.Join(unknown, ", "))
			}
		}
		e.auth = *a
	}
	if l := sc.Limits.Concurrency; l > 0 {
		if l > e.concurrency {
			v.fail(path+"/limits/concurrency", "limit_widening", "scope limits may only narrow ancestors")
		}
		e.concurrency = min(l, e.concurrency)
	}
	if l := sc.Limits.AdmissionLimit; l > 0 {
		if l > e.admission {
			v.fail(path+"/limits/admission_limit", "limit_widening", "scope limits may only narrow ancestors")
		}
		e.admission = min(l, e.admission)
	}
	for k, ref := range sc.SecretRefs {
		if ref == "" {
			delete(e.refs, k)
		} else {
			e.refs[k] = ref
		}
	}
	names := map[string]bool{}
	entry := func(category string, j int, name string, disabled bool, exists bool) {
		p := fmt.Sprintf("%s/%s/%d", path, category, j)
		if names[category+"\x00"+name] {
			v.fail(p+"/name", "duplicate_name", "entry name is repeated in this scope; keep one")
		}
		names[category+"\x00"+name] = true
		if disabled && !exists {
			v.fail(p, "unknown_disabled_entry", "tombstone names no inherited entry; remove it")
		}
	}
	for j, b := range sc.Bindings {
		_, exists := e.bindings.byName[b.Name]
		entry("bindings", j, b.Name, b.Disabled, exists)
		e.bindings.put(b.Name, slot[Binding]{b.Binding, b.Disabled, i, j})
	}
	for j, r := range sc.Routes {
		_, exists := e.routes.byName[r.Name]
		entry("routes", j, r.Name, r.Disabled, exists)
		e.routes.put(r.Name, slot[Route]{r.Route, r.Disabled, i, j})
	}
	for j, x := range sc.Extensions {
		_, exists := e.extensions.byName[x.Name]
		entry("extensions", j, x.Name, x.Disabled, exists)
		e.extensions.put(x.Name, slot[Extension]{x.Extension, x.Disabled, i, j})
	}
	v.check(i, sc.ID, e)
	v.state[id] = e
	return e
}

// check revalidates every effective entry. Context-free registration checks run
// only where an entry is declared; inherited entries resolve against this
// scope's ceiling, secrets and bindings.
func (v *validator) check(i int, scopeID string, e *effective) {
	at := func(category, name string, s int, j int) (string, bool) {
		if s == i {
			return fmt.Sprintf("scopes/%d/%s/%d", i, category, j), true
		}
		return ptr(fmt.Sprintf("scopes/%d/%s", i, category), name), false
	}
	secrets := func(names []string) bool {
		return all(names, func(n string) bool { return e.refs[n] != "" })
	}
	for _, name := range e.bindings.names {
		s := e.bindings.byName[name]
		if s.disabled {
			continue
		}
		b := s.value
		p, own := at("bindings", name, s.scope, s.index)
		if own {
			v.registration(p, b)
		}
		if !subset(b.Operations, e.auth.Operations) {
			v.fail(p+"/operations", "operation_denied", "binding operations must be registered and within the scope authorization")
		}
		for k, g := range b.Grants {
			if !subset(g.Operations, e.auth.Operations) {
				v.fail(fmt.Sprintf("%s/grants/%d/operations", p, k), "operation_denied", "grant operations must be registered and within the scope authorization")
			}
		}
		if !secrets(b.SecretNames) {
			v.fail(p+"/secret_names", "unknown_secret", "selected secret name has no effective reference in this scope")
		}
		if b.Connection != "" && !slices.Contains(e.auth.Connections, b.Connection) {
			v.fail(p+"/connection", "connection_denied", "connection is not granted by the scope authorization")
		}
	}
	for _, name := range e.routes.names {
		s := e.routes.byName[name]
		if s.disabled {
			continue
		}
		r := s.value
		p, own := at("routes", name, s.scope, s.index)
		if own && !v.reg.Handlers[r.Handler] {
			v.fail(p+"/handler", "unknown_registration", "route handler is not registered; use a registered handler")
		} else if check := v.reg.HandlerConfig[r.Handler]; own && check != nil && check(r.Config) != nil {
			v.fail(p+"/config", "invalid_config", "route config rejected by its registered handler; fix it against the handler's documented config")
		}
		b, ok := e.bindings.byName[r.Binding]
		if !ok || b.disabled {
			v.fail(p+"/binding", "missing_binding", "route binding is missing or disabled in this scope; bind an enabled binding")
			continue
		}
		if check := v.reg.HandlerBinding[r.Handler]; check != nil {
			if err := check(extension.RouteBinding{Executor: b.value.Executor, Operations: b.value.Operations, Config: b.value.Config, Grants: b.value.domainGrants()}); err != nil {
				v.fail(p+"/binding", "binding_refused", fmt.Sprintf("route handler %s refuses this binding: %s", r.Handler, err))
			}
		}
		exec, ok := v.reg.Executors[b.value.Executor]
		if !ok {
			continue
		}
		c, req := exec.Capabilities(), b.value.Requires
		// An explicit binding requirement already fails at the binding.
		if r.ActivePolicy == "inject" && !c.Inject && !req.Inject || r.ActivePolicy == "interrupt" && !c.Interrupt && !req.Interrupt {
			if r.UnsupportedFallback == "" {
				v.fail(p+"/active_policy", "unsupported_policy", fmt.Sprintf("executor %s has no %s; set unsupported_fallback to queue or drop, or bind a capable executor", b.value.Executor, r.ActivePolicy))
			}
		}
	}
	for _, name := range e.extensions.names {
		s := e.extensions.byName[name]
		if s.disabled {
			continue
		}
		x := s.value
		p, own := at("extensions", name, s.scope, s.index)
		t, registered := v.reg.Extensions[x.Type]
		if own && !registered {
			v.fail(p+"/type", "unknown_registration", "extension type is not registered; use a registered type")
		} else if own && t.Validate != nil && t.Validate(x.Config, x.SecretNames) != nil {
			v.fail(p+"/config", "invalid_config", "extension config rejected by its registered owner; fix it against the owner schema")
		}
		if x.Connection != "" && !slices.Contains(e.auth.Connections, x.Connection) {
			v.fail(p+"/connection", "connection_denied", "connection is not granted by the scope authorization")
		}
		if !secrets(x.SecretNames) {
			v.fail(p+"/secret_names", "unknown_secret", "selected secret name has no effective reference in this scope")
		}
		if registered && t.Ingress {
			var target struct {
				Scope string `json:"scope"`
			}
			if json.Unmarshal(x.Config, &target) != nil || target.Scope != scopeID {
				v.fail(p+"/config/scope", "ingress_target_mismatch", "ingress target scope must be the containing scope; override or disable the inherited entry")
			}
		}
	}
}

// registration performs context-free executor, capability and resource checks.
func (v *validator) registration(p string, b Binding) {
	if r := b.Requires; r.Workspace && b.Resource == nil {
		v.fail(p+"/resource", "resource_required", "workspace requires a bound resource provider; add resource or remove requires.workspace")
	}
	if b.Resource != nil && !v.reg.Resources[b.Resource.Provider] {
		v.fail(p+"/resource/provider", "unknown_registration", "resource provider is not registered; use a registered provider")
	} else if b.Resource != nil {
		if check := v.reg.ResourceConfig[b.Resource.Provider]; check != nil && check(b.Resource.Config) != nil {
			v.fail(p+"/resource/config", "invalid_config", fmt.Sprintf("resource provider %s rejected this config; correct it against the provider's documented config", b.Resource.Provider))
		}
	}
	exec, ok := v.reg.Executors[b.Executor]
	if !ok {
		v.fail(p+"/executor", "unknown_registration", "executor is not registered; use a registered executor")
		return
	}
	c, r := exec.Capabilities(), b.Requires
	for _, f := range []struct {
		key           string
		want, have    bool
		article, tail string
	}{
		{"interrupt", r.Interrupt, c.Interrupt, "an", " and set route unsupported_fallback=queue"},
		{"inject", r.Inject, c.Inject, "an", " and set route unsupported_fallback=queue"},
		{"resume", r.Resume, c.Resume, "a", ""},
		{"workspace", r.Workspace, c.Workspace, "a", ""},
	} {
		if f.want && !f.have {
			v.fail(p+"/requires/"+f.key, "unsupported_capability", fmt.Sprintf("executor %s has no %s; select %s %s executor or remove the requirement%s", b.Executor, f.key, f.article, f.key, f.tail))
		}
	}
	if r.Toolbelt != "" && r.Toolbelt != c.Toolbelt {
		v.fail(p+"/requires/toolbelt", "unsupported_capability", fmt.Sprintf("executor %s uses toolbelt %s; select a matching executor or change requires.toolbelt", b.Executor, c.Toolbelt))
	}
	if err := extension.ValidateBinding(domain.Binding{Name: b.Name, Executor: b.Executor, Requires: r}, exec); errors.Is(err, extension.ErrInvalid) {
		v.fail(p+"/executor", "invalid_registration", "executor registration does not implement its advertised capabilities")
	}
	// Executor config is opaque to core and strictly checked by its owner. The
	// owner's error text is never echoed: it could quote configured values.
	if owner, ok := exec.(ConfigValidator); ok && owner.ValidateConfig(b.Config, b.SecretNames) != nil {
		v.fail(p+"/config", "invalid_config", fmt.Sprintf("executor %s rejected this binding config; correct it against the executor's documented config", b.Executor))
	} else if ok {
		// Each grant's config is checked as the binding a Work that takes
		// it alone runs.
		base := domain.Binding{Config: domain.Payload{Data: b.Config}, Grants: b.domainGrants()}
		for k, g := range b.Grants {
			granted, _ := base.Granted([]string{g.Name})
			if owner.ValidateConfig(granted.Config.Data, b.SecretNames) != nil {
				v.fail(fmt.Sprintf("%s/grants/%d/config", p, k), "invalid_config", fmt.Sprintf("executor %s rejected this binding config with the grant applied; correct it against the executor's documented config", b.Executor))
			}
		}
	}
}

// ConfigValidator is optionally implemented by an executor registration to
// reject its binding config at load/apply time instead of at launch.
type ConfigValidator interface {
	ValidateConfig(config json.RawMessage, secretNames []string) error
}

func (v *validator) operation(op string) bool {
	return slices.Contains(Builtins, op) || slices.Contains(v.reg.Operations, op)
}
func subset(a, b []string) bool { return all(a, func(s string) bool { return slices.Contains(b, s) }) }

// missing returns the entries of a that f does not know, in order.
func missing(a []string, f func(string) bool) []string {
	var out []string
	for _, s := range a {
		if !f(s) {
			out = append(out, s)
		}
	}
	return out
}

func all(a []string, f func(string) bool) bool {
	for _, s := range a {
		if !f(s) {
			return false
		}
	}
	return true
}

func (v *validator) snapshot() *Snapshot {
	normalized, hash := v.doc.Normalize()
	s := &Snapshot{Hash: hash, Normalized: normalized, Settings: v.doc.Settings, Scopes: map[domain.ID]*EffectiveScope{},
		redacted: canonical(v.doc.normalized(true))}
	s.Settings.RetryDelaysSeconds = slices.Clone(s.Settings.RetryDelaysSeconds)
	for _, c := range v.doc.Connections {
		refs := make(map[string]string, len(c.SecretRefs))
		for k, ref := range c.SecretRefs {
			refs[k] = ref
		}
		s.Connections = append(s.Connections, Connection{ID: c.ID, Type: c.Type, Config: slices.Clone(c.Config), SecretRefs: refs})
	}
	for _, sc := range v.doc.Scopes {
		e := v.state[sc.ID]
		id := domain.ID(sc.ID)
		s.Order = append(s.Order, id)
		s.Scopes[id] = &EffectiveScope{ID: id, ParentID: domain.ID(sc.Parent), Name: sc.Name, Enabled: e.enabled,
			Concurrency: e.concurrency, AdmissionLimit: e.admission, Authorization: e.auth,
			Bindings: e.bindings.active(), Routes: e.routes.active(), Extensions: e.extensions.active(), SecretRefs: e.refs, Source: sc.Source}
	}
	return s
}

func (s *Snapshot) Scope(id domain.ID) (*EffectiveScope, bool) {
	e, ok := s.Scopes[id]
	return e, ok
}

// Redacted is the normalized config with reference text replaced by
// "[redacted]"; names and false overlays are kept.
func (s *Snapshot) Redacted() json.RawMessage { return slices.Clone(s.redacted) }

// Binding returns an effective, non-tombstoned binding by name.
func (e *EffectiveScope) Binding(name string) (Binding, bool) {
	for _, b := range e.Bindings {
		if b.Name == name {
			return b, true
		}
	}
	return Binding{}, false
}

// AuthorizeOperation is the current-ceiling check on every toolbelt call:
// authority only narrows from the pinned grant.
func (s *Snapshot) AuthorizeOperation(scope domain.ID, binding, operation string) error {
	e, ok := s.Scopes[scope]
	if !ok || !e.Enabled {
		return ErrDenied
	}
	b, ok := e.Binding(binding)
	if !ok || !b.Operation(operation) || !slices.Contains(e.Authorization.Operations, operation) {
		return ErrDenied
	}
	return nil
}

// AuthorizeRead allows the source scope itself or an enabled read_scopes grant.
func (s *Snapshot) AuthorizeRead(source, target domain.ID) error {
	src, ok := s.Scopes[source]
	if !ok || !src.Enabled {
		return ErrDenied
	}
	if source == target {
		return nil
	}
	dst, ok := s.Scopes[target]
	if !ok || !dst.Enabled || !slices.Contains(src.Authorization.ReadScopes, string(target)) {
		return ErrDenied
	}
	return nil
}

const (
	BindingSchema = "core.config.binding.v1"
	RouteSchema   = "core.config.route.v1"
	ScopeSchema   = "core.config.scope.v1"
)

type bindingMeta struct {
	Connection  string    `json:"connection,omitempty"`
	Operations  []string  `json:"operations"`
	SecretNames []string  `json:"secret_names"`
	Resource    *Resource `json:"resource,omitempty"`
}
type routeMeta struct {
	UnsupportedFallback string `json:"unsupported_fallback,omitempty"`
	ErrorFallback       string `json:"error_fallback,omitempty"`
}

// ScopeMetadata is the opaque domain.Scope Config payload (ScopeSchema). It
// contains no secret reference text.
type ScopeMetadata struct {
	Enabled bool `json:"enabled"`
	Limits  struct {
		Concurrency    int `json:"concurrency"`
		AdmissionLimit int `json:"admission_limit"`
	} `json:"limits"`
	Authorization Authorization          `json:"authorization"`
	Bindings      map[string]bindingMeta `json:"bindings"`
	Routes        map[string]routeMeta   `json:"routes"`
	Extensions    []Extension            `json:"extensions"`
}

// DomainScopes maps effective scopes onto domain.Scope in document order.
func (s *Snapshot) DomainScopes() []domain.Scope {
	out := make([]domain.Scope, 0, len(s.Order))
	for _, id := range s.Order {
		e := s.Scopes[id]
		meta := ScopeMetadata{Enabled: e.Enabled, Authorization: e.Authorization,
			Bindings: map[string]bindingMeta{}, Routes: map[string]routeMeta{}, Extensions: slices.Clone(e.Extensions)}
		meta.Limits.Concurrency, meta.Limits.AdmissionLimit = e.Concurrency, e.AdmissionLimit
		if meta.Extensions == nil {
			meta.Extensions = []Extension{}
		}
		d := domain.Scope{ID: e.ID, Name: e.Name, ParentID: e.ParentID, SecretRefs: maps.Clone(e.SecretRefs)}
		for _, b := range e.Bindings {
			d.Bindings = append(d.Bindings, domain.Binding{Name: b.Name, Executor: b.Executor, Requires: b.Requires, TimeoutSeconds: b.TimeoutSeconds,
				Config: domain.Payload{Schema: BindingSchema, Data: slices.Clone(b.Config)}, Grants: b.domainGrants()})
			meta.Bindings[b.Name] = bindingMeta{b.Connection, b.Operations, b.SecretNames, b.Resource}
		}
		for _, r := range e.Routes {
			d.Routes = append(d.Routes, domain.Route{Name: r.Name, Handler: r.Handler, Binding: r.Binding,
				ActivePolicy: r.ActivePolicy, Config: domain.Payload{Schema: RouteSchema, Data: slices.Clone(r.Config)}})
			meta.Routes[r.Name] = routeMeta{r.UnsupportedFallback, r.ErrorFallback}
		}
		d.Config = domain.Payload{Schema: ScopeSchema, Data: canonical(meta)}
		out = append(out, d)
	}
	return out
}
