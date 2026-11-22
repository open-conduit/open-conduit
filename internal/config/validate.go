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
var Builtins = []string{"emit", "create_work", "cancel_work", "wait", "declare_outcome", "read_context"}

// Registry is supplied in-process by first-party registration.
type Registry struct {
	Executors   map[string]extension.Executor
	Handlers    map[string]bool
	Extensions  map[string]ExtensionType
	Connections map[string]func(config json.RawMessage) error // nil func accepts any object
	Operations  []string
	// Toolsets are the registered extensions' toolsets by name
	// (extension.Toolset), which operations lists grant as "@<name>".
	Toolsets map[string]extension.Toolset
	// Playbooks are the operations each registered playbook declares it
	// relies on (extension.PlaybookOperations), by playbook name.
	Playbooks map[string][]string
	Resources map[string]bool
	// Packages are the installed and bundled extension packages by name,
	// which settings.extensions grants (PackageWarnings).
	Packages map[string]bool
	// Policy is the operator's policy above the config (--policy), which
	// every config must fit inside; nil is none.
	Policy *Policy
	// ResourceConfig optionally validates a registered provider's binding
	// resource config at load/apply time. Its error text is never echoed.
	ResourceConfig map[string]func(json.RawMessage) error
	// ResourceSandbox optionally refuses a provider's resource config for a
	// binding whose executor runs in a sandbox (B09), given the mode the
	// executor requests ("" for the config's default): a workspace the
	// sandbox could only use by writing outside it. Its error is a fixed,
	// secret-free reason, shown in the diagnostic.
	ResourceSandbox map[string]func(config json.RawMessage, mode string) error
	// ResourceSite optionally says a provider's resource config can only be
	// made at the site its binding's launch is placed at
	// (extension.ResourceSiteUser.NeedsSite), so the binding needs a
	// container provider in Placers.
	ResourceSite map[string]func(config json.RawMessage) bool
	// Placers are the registered container providers that place launches
	// (extension.ContainerPlacer).
	Placers map[string]bool
	// ResourceModeDefault optionally names the mode a provider's resource
	// config sets as its default, "" for none. An executor that requests a
	// mode of its own (ResourceModer) overrides it, and config check warns
	// when the two differ (ModeWarnings).
	ResourceModeDefault map[string]func(config json.RawMessage) string
	// Sandboxes are the registered process sandbox providers (B09), each
	// with an optional strict check of a binding's sandbox config. Its
	// error is a fixed, secret-free reason, shown in the diagnostic.
	Sandboxes map[string]func(json.RawMessage) error
	// Containers are the registered container providers, each with a
	// check of its own part of a binding's container config. The config's
	// shared shape (an image pinned by digest, the grant) is checked by the
	// host whatever the provider.
	Containers map[string]func(json.RawMessage) error
	// Tunnels are the webhook tunnel types packages declare (manifest
	// tunnels), each with its package's check of a tunnel config
	// (settings.webhooks.tunnel). Its error is a fixed, secret-free
	// reason, shown in the diagnostic.
	Tunnels map[string]func(json.RawMessage) error
	// BackupDestinations are the backup destination types packages declare
	// (manifest backup_destinations), each with its package's check of a
	// destination's config.
	BackupDestinations map[string]func(json.RawMessage) error
	// SecretSchemes are the secret reference schemes packages declare
	// (manifest secret_schemes), each with the name of the package that
	// resolves it. A reference of any other scheme but the built-in forms
	// is refused.
	SecretSchemes map[string]string
	// ConnectionOwners are the registration names of the packages that own
	// connection types, by type, so validation can see which package
	// resolves a connection's secrets (a provider's own credentials).
	ConnectionOwners map[string]string
	// ContainerOwners are the registration names of the packages that
	// provide container types, by type, so a binding's container may name
	// only a connection of a type its provider's package declares.
	ContainerOwners map[string]string
	// HandlerConfig optionally validates a registered route handler's opaque
	// route config at load/apply time (05-01). Its error text is never echoed.
	HandlerConfig map[string]func(json.RawMessage) error
	// HandlerWorkKinds optionally names, for a route handler that binds
	// child Work, the kinds a route's config binds, each with what it is
	// for. A binding's work_kinds must each be bound by an enabled route of
	// its scope, so a kind an agent may ask for always has a binding to run
	// it; the description is what an asking agent is shown.
	HandlerWorkKinds map[string]func(json.RawMessage) []extension.WorkKind
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
	// Bindings optionally lists the binding names a valid entry's config
	// refers to (a human gate's bindings). Each must name an enabled
	// binding of every scope the entry applies to.
	Bindings func(config json.RawMessage) []string
	// Operations, for an entry type whose config declares toolbelt
	// operations, lists the ones a valid entry's config declares
	// (extension.EntryOperationDecl), asked after Validate accepts it.
	// Each is the operation <Owner>.<entry>.<name>, grantable in config
	// and registered where the entry is in effect (EntryOperations).
	Operations func(config json.RawMessage) []extension.EntryOperationDecl
	// Owner is the registration name of the extension that owns the type,
	// the root of its entry operations' names.
	Owner string
	// Sandbox says whether an entry of the type names a process sandbox
	// for the processes its owner runs for it (protocol.ExtensionDecl.
	// Sandbox): "required", "optional", or "" when it may not, since
	// nothing would confine them.
	Sandbox string
}

// Entry is one thing an extension entry sets up, named and described.
type Entry struct{ Name, Detail string }

type EffectiveScope struct {
	ID, ParentID                domain.ID
	Name                        string
	Enabled                     bool
	Concurrency, AdmissionLimit int
	Authorization               Authorization
	// IsolationScope is the scope that set Authorization.Isolation: this
	// one or the ancestor it is inherited from ("" when none requires it).
	IsolationScope domain.ID
	Bindings       []Binding
	Routes         []Route
	Extensions     []Extension
	SecretRefs     map[string]string
	// Source is the included folder the scope came from, or "".
	Source string
	// EntryOperations are the entry operations in effect in the scope:
	// those its effective extension entries declare, by name.
	EntryOperations []string
}

// Snapshot is one immutable validated generation candidate. Normalized
// contains secret reference text: never log or return it.
type Snapshot struct {
	// PolicyClamps says which limits the daemon's policy holds below what
	// this generation states (doctor reports them).
	PolicyClamps []string
	Hash         string
	// FileHash is the hash of the generation without its submitted
	// includes' scopes: the part a config file sets, which a config file
	// apply checks its candidate against, so a team's fragment apply never
	// stales it. It is Hash when there are none.
	FileHash   string
	Normalized json.RawMessage
	Settings   Settings
	Order      []domain.ID
	Scopes     map[domain.ID]*EffectiveScope
	// Connections contain secret reference text: never log or return them.
	Connections []Connection
	// EntryOperations are the operations the enabled scopes' extension
	// entries declare, by the registration name of the extension that owns
	// each entry's type, each with the enabled scopes it is in effect in,
	// sorted by name. The host registers them for the generation
	// (extension.Registration.EntryOperations).
	EntryOperations map[string][]extension.EntryOperation
	// Includes are the submitted includes, each a fragment an operator
	// granted configure on its parent may apply (ApplyFragment).
	Includes []Include
	redacted json.RawMessage
}

// Include returns the submitted include name.
func (s *Snapshot) Include(name string) (Include, bool) {
	for _, in := range s.Includes {
		if in.Name == name {
			return in, true
		}
	}
	return Include{}, false
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
	// isolationScope is the scope that set auth.Isolation, or "".
	isolationScope string
	refs           map[string]string
	bindings       slots[Binding]
	routes         slots[Route]
	extensions     slots[Extension]
	// resolved are the scope's bindings with their operations lists
	// resolved against its authorization, by name.
	resolved map[string]Binding
	// entryOps are the entry operations the scope's effective entries
	// declare, sorted.
	entryOps []string
}

type validator struct {
	doc         *Document
	reg         Registry
	mode        policyMode
	diags       []Diagnostic
	connections map[string]bool
	index       map[string]int
	state       map[string]*effective
	visiting    map[string]bool
	broken      map[string]bool
	// playbooks caches BindingPlaybook by executor and config, so an
	// executor in a package is asked once per binding config.
	playbooks map[string]playbookAnswer
	// entryChecked are the owners' answers to the extension entries
	// validated early, for their operations (declareEntryOperations), by
	// path, so check doesn't ask again.
	entryChecked map[string]error
	// entryOps are the entry operations config declares, by name, and
	// slotOps the names each entry declares, by its declaring slot.
	entryOps map[string]entryOperation
	slotOps  map[[2]int][]string
}

// entryOperation is an operation an extension entry declares, with the
// owner of its type, the entry's name and where it was first declared.
type entryOperation struct {
	owner, entry, path string
	decl               extension.EntryOperationDecl
}

type playbookAnswer struct {
	name string
	ops  []string
}

// playbook is BindingPlaybook for b, asked once per executor and config.
func (v *validator) playbook(b Binding) (string, []string) {
	key := b.Executor + "\x00" + string(b.Config)
	if a, ok := v.playbooks[key]; ok {
		return a.name, a.ops
	}
	name, ops := BindingPlaybook(v.reg, b)
	if v.playbooks == nil {
		v.playbooks = map[string]playbookAnswer{}
	}
	v.playbooks[key] = playbookAnswer{name, ops}
	return name, ops
}

func (v *validator) fail(path, code, msg string) {
	v.diags = append(v.diags, Diagnostic{path, code, msg})
}

// owned records an owner's refusal (err, nil for none) of the config
// object at path (owned).
func (v *validator) owned(path string, err error, fallback string) {
	if err != nil {
		v.diags = append(v.diags, Owned(path, err, fallback)...)
	}
}

// Owned are the diagnostics of an owner's refusal of the config object at
// path: each diagnostic the owner authored (extension.ConfigError), at its
// field's full path and with its own code and message, or invalid_config
// at the object with fallback when it authored none. The owner's error
// text is never echoed: it could quote configured values.
func Owned(path string, err error, fallback string) []Diagnostic {
	ds := extension.ConfigDiagnostics(err)
	if len(ds) == 0 {
		return []Diagnostic{{path, "invalid_config", fallback}}
	}
	out := make([]Diagnostic, len(ds))
	for i, d := range ds {
		out[i] = Diagnostic{path + d.Path, d.Code, d.Message}
	}
	return out
}

// withGrant are Owned's diagnostics for the config of binding p refused
// with its grant k applied. The grant replaces only the top-level fields
// it sets, so a diagnostic about one of those is at the grant's config,
// and any other at the binding's own config, where the field is. Each
// message names the grant.
func withGrant(p string, k int, g Grant, err error, fallback string) []Diagnostic {
	grantPath := fmt.Sprintf("%s/grants/%d/config", p, k)
	ds := extension.ConfigDiagnostics(err)
	if len(ds) == 0 {
		return []Diagnostic{{grantPath, "invalid_config", fallback}}
	}
	var set map[string]json.RawMessage
	_ = json.Unmarshal(g.Config, &set)
	out := make([]Diagnostic, len(ds))
	for i, d := range ds {
		path := grantPath + d.Path
		if first, _, _ := strings.Cut(strings.TrimPrefix(d.Path, "/"), "/"); d.Path != "" {
			if _, ok := set[strings.NewReplacer("~1", "/", "~0", "~").Replace(first)]; !ok {
				path = p + "/config" + d.Path
			}
		}
		out[i] = Diagnostic{path, d.Code, fmt.Sprintf("with grant %s applied: %s", g.Name, d.Message)}
	}
	return out
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
	return validateDocument(doc, reg, policyMode{})
}

func validateDocument(doc *Document, reg Registry, mode policyMode) (*Snapshot, error) {
	v := &validator{doc: doc, reg: reg, mode: mode, connections: map[string]bool{}, index: map[string]int{},
		state: map[string]*effective{}, visiting: map[string]bool{}, broken: map[string]bool{}}
	for i, c := range doc.Connections {
		p := ptr("connections", i)
		if v.connections[c.ID] {
			v.fail(p+"/id", "duplicate_id", "connection id is already defined; use a unique id")
		}
		v.connections[c.ID] = true
		check, ok := reg.Connections[c.Type]
		if !ok {
			v.fail(p+"/type", "unknown_registration", "connection type is not registered; use a registered type"+registeredNames(reg.Connections, c.Type))
		} else if check != nil {
			v.owned(p+"/config", check(c.Config), "connection config rejected by its registered owner; fix it against the owner schema")
		}
	}
	for i, s := range doc.Scopes {
		if _, ok := v.index[s.ID]; ok {
			v.fail(ptr("scopes", i)+"/id", "duplicate_id", "scope id is already defined; use a unique id")
			continue
		}
		v.index[s.ID] = i
	}
	v.declareEntryOperations()
	for _, s := range doc.Scopes {
		v.resolve(s.ID)
	}
	for i, b := range doc.Settings.Budgets {
		if _, ok := v.index[b.Scope]; b.Scope != "" && !ok {
			v.fail(ptr("settings/budgets", i)+"/scope", "unknown_scope", "budget names a scope that is not defined; use a scope id")
		}
	}
	v.submittedIncludes()
	v.tunnel(doc.Settings.Webhooks.Tunnel)
	v.backupDestinations(doc.Settings.Backup.Destinations)
	v.secretSchemes()
	for _, name := range slices.Sorted(maps.Keys(doc.Settings.Extensions)) {
		v.checkPassEnv(ptr("settings/extensions", name)+"/pass_env", doc.Settings.Extensions[name].PassEnv)
	}
	v.policy()
	if len(v.diags) > 0 {
		return nil, invalid(v.diags)
	}
	s := v.snapshot()
	reg.Policy.clamp(s)
	return s, nil
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
		auth: Authorization{Connections: []string{}, Operations: []string{}, ReadScopes: []string{}, FileScopes: []string{}}, refs: map[string]string{}}
	if v.reg.Policy.RequiresIsolation() {
		e.auth.Isolation = IsolationRequired
	}
	if parent != nil {
		e = &effective{parent.enabled, parent.concurrency, parent.admission, parent.auth, parent.isolationScope, maps.Clone(parent.refs),
			parent.bindings.clone(), parent.routes.clone(), parent.extensions.clone(), nil, nil}
	}
	e.enabled = e.enabled && sc.Enabled
	if a := sc.Authorization; a != nil {
		categories := []struct {
			key                string
			requested, current []string
			known              func(string) bool
		}{
			{"connections", a.Connections, e.auth.Connections, func(s string) bool { return v.connections[s] }},
			{"read_scopes", a.ReadScopes, e.auth.ReadScopes, func(s string) bool { _, ok := v.index[s]; return ok }},
			{"file_scopes", a.FileScopes, e.auth.FileScopes, func(s string) bool { _, ok := v.index[s]; return ok }},
			{"work_kinds", a.WorkKinds, e.auth.WorkKinds, func(string) bool { return true }},
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
		var ceiling []string
		if parent != nil {
			ceiling = e.auth.Operations
		}
		ops := v.resolveOperations(a.Operations, ceiling, path+"/authorization/operations", "authority_widening",
			"child authorization must be a subset of the parent's effective operations")
		inherited := e.auth.Isolation
		e.auth = *a
		e.auth.Operations = ops
		// The requirement is a restriction, not a grant: an authorization
		// that doesn't restate it still has it, so no descendant drops it.
		if inherited != "" {
			e.auth.Isolation = inherited
		}
	}
	if e.isolationScope == "" && e.auth.Isolation != "" {
		e.isolationScope = id
	}
	if l := sc.Limits.Concurrency; l > 0 {
		if l > e.concurrency {
			v.fail(path+"/limits/concurrency", "limit_widening", "scope limits may only narrow ancestors")
		}
		e.concurrency = min(l, e.concurrency)
	}
	// admission_limit has no default: unset (zero) is no cap, and a scope
	// may set one where no ancestor does.
	if l := sc.Limits.AdmissionLimit; l > 0 {
		if e.admission > 0 && l > e.admission {
			v.fail(path+"/limits/admission_limit", "limit_widening", "scope limits may only narrow ancestors")
		}
		if e.admission == 0 || l < e.admission {
			e.admission = l
		}
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

// routedKind reports whether an enabled route of e binds child Work of kind
// (Registry.HandlerWorkKinds).
func (v *validator) routedKind(e *effective, kind string) bool {
	for _, name := range e.routes.names {
		if s := e.routes.byName[name]; !s.disabled {
			if _, ok := RoutedWorkKind(v.reg, s.value, kind); ok {
				return true
			}
		}
	}
	return false
}

// RoutedWorkKind is kind as route binds it, with its description, when the
// route's handler binds child Work of that kind (Registry.HandlerWorkKinds).
func RoutedWorkKind(reg Registry, route Route, kind string) (extension.WorkKind, bool) {
	kinds := reg.HandlerWorkKinds[route.Handler]
	if kinds == nil {
		return extension.WorkKind{}, false
	}
	for _, k := range kinds(route.Config) {
		if k.Kind == kind {
			return k, true
		}
	}
	return extension.WorkKind{}, false
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
	e.entryOps = nil
	for _, name := range e.extensions.names {
		if s := e.extensions.byName[name]; !s.disabled {
			e.entryOps = append(e.entryOps, v.slotOps[[2]int{s.scope, s.index}]...)
		}
	}
	slices.Sort(e.entryOps)
	e.entryOps = slices.Compact(e.entryOps)
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
		v.isolated(p, scopeID, e, b)
		resolved := b
		resolved.Playbook, resolved.PlaybookOperations = v.playbook(b)
		resolved.Operations = v.resolveOperations(b.Operations, e.auth.Operations, p+"/operations", "operation_denied",
			"binding operations must be registered and within the scope authorization")
		resolved.Operations = v.inEffect(e, b.Operations, resolved.Operations, p+"/operations")
		resolved.Grants = slices.Clone(b.Grants)
		for k, g := range b.Grants {
			gp := fmt.Sprintf("%s/grants/%d/operations", p, k)
			resolved.Grants[k].Operations = v.resolveOperations(g.Operations, e.auth.Operations, gp, "operation_denied",
				"grant operations must be registered and within the scope authorization")
			resolved.Grants[k].Operations = v.inEffect(e, g.Operations, resolved.Grants[k].Operations, gp)
		}
		if e.resolved == nil {
			e.resolved = map[string]Binding{}
		}
		e.resolved[name] = resolved
		if !secrets(b.SecretNames) {
			v.fail(p+"/secret_names", "unknown_secret", "selected secret name has no effective reference in this scope")
		}
		for k, g := range b.Grants {
			if !secrets(g.SecretNames) {
				v.fail(fmt.Sprintf("%s/grants/%d/secret_names", p, k), "unknown_secret", "selected secret name has no effective reference in this scope")
			}
		}
		if b.Connection != "" && !slices.Contains(e.auth.Connections, b.Connection) {
			v.fail(p+"/connection", "connection_denied", "connection is not granted by the scope authorization")
		}
		if id := b.Container.ConnectionID(); id != "" && !slices.Contains(e.auth.Connections, id) {
			v.fail(p+"/container/config/connection", "connection_denied", "connection is not granted by the scope authorization")
		}
		for k, kind := range b.WorkKinds {
			if !slices.Contains(e.auth.WorkKinds, kind) {
				v.fail(fmt.Sprintf("%s/work_kinds/%d", p, k), "work_kind_denied", "kind is not in the scope authorization's work_kinds; add it there or remove it here")
			}
		}
		if len(b.WorkKinds) > 0 && !resolved.Operation("create_work") {
			v.fail(p+"/work_kinds", "work_kinds_unused", "the binding names work_kinds but isn't granted create_work; add create_work to its operations")
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
			v.fail(p+"/handler", "unknown_registration", "route handler is not registered; use a registered handler"+registeredNames(v.reg.Handlers, r.Handler))
		} else if check := v.reg.HandlerConfig[r.Handler]; own && check != nil {
			v.owned(p+"/config", check(r.Config), "route config rejected by its registered handler; fix it against the handler's documented config")
		}
		b, ok := e.bindings.byName[r.Binding]
		if !ok || b.disabled {
			v.fail(p+"/binding", "missing_binding", "route binding is missing or disabled in this scope; bind an enabled binding")
			continue
		}
		if check := v.reg.HandlerBinding[r.Handler]; check != nil {
			// The handler judges what the binding asks for, toolsets
			// expanded but not narrowed to the scope's grant, so a binding
			// is refused for what it names even where the ceiling would
			// take it away.
			rb := b.value
			rb.Operations = ResolveOperations(rb.Operations, v.reg, nil)
			rb.Grants = slices.Clone(rb.Grants)
			for k := range rb.Grants {
				rb.Grants[k].Operations = ResolveOperations(rb.Grants[k].Operations, v.reg, nil)
			}
			if err := check(extension.RouteBinding{Executor: rb.Executor, Operations: rb.Operations, Config: rb.Config, Grants: rb.domainGrants()}); err != nil {
				v.fail(p+"/binding", "binding_refused", fmt.Sprintf("route handler %s refuses this binding: %s. Change the binding (its executor, operations, grants or config) to meet that, or route to a handler that accepts it", r.Handler, err))
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
	// After the routes, whose validation tells the host what an installed
	// handler's route binds (Registry.HandlerWorkKinds).
	for _, name := range e.bindings.names {
		s := e.bindings.byName[name]
		if s.disabled {
			continue
		}
		p, _ := at("bindings", name, s.scope, s.index)
		for k, kind := range s.value.WorkKinds {
			if !v.routedKind(e, kind) {
				v.fail(fmt.Sprintf("%s/work_kinds/%d", p, k), "work_kind_unrouted", "no enabled route in this scope binds child Work of this kind, so it would never run; add a work route that lists it or remove it here")
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
			v.fail(p+"/type", "unknown_registration", "extension type is not registered; use a registered type"+registeredNames(v.reg.Extensions, x.Type))
		} else if err, early := v.entryChecked[p]; own && early {
			v.owned(p+"/config", err, "extension config rejected by its registered owner; fix it against the owner schema")
		} else if own && t.Validate != nil {
			v.owned(p+"/config", t.Validate(x.Config, x.SecretNames), "extension config rejected by its registered owner; fix it against the owner schema")
		}
		if x.Connection != "" && !slices.Contains(e.auth.Connections, x.Connection) {
			v.fail(p+"/connection", "connection_denied", "connection is not granted by the scope authorization")
		}
		if own && registered {
			v.entrySandbox(p, x, t)
		}
		if registered && t.Bindings != nil {
			var missing []string
			for _, name := range t.Bindings(x.Config) {
				if b, ok := e.bindings.byName[name]; (!ok || b.disabled) && !slices.Contains(missing, name) {
					missing = append(missing, name)
				}
			}
			if len(missing) > 0 {
				v.fail(p+"/config", "missing_binding", "config names a binding this scope has no enabled binding of: "+strings.Join(missing, ", ")+"; name a binding of the scope, or enable it")
			}
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

// isolated refuses a binding that would run unconfined in a scope whose
// authorization requires isolation: one with neither a sandbox nor a
// container, whose executor doesn't declare that it runs no process the
// agent controls. An unregistered executor declares nothing, so it is
// refused too. The binding's grants run under the binding's own sandbox or
// container, so checking it covers them.
func (v *validator) isolated(p, scopeID string, e *effective, b Binding) {
	if e.auth.Isolation == "" || b.Sandbox != nil || b.Container != nil {
		return
	}
	if exec, ok := v.reg.Executors[b.Executor]; ok && exec.Capabilities().NoProcess {
		return
	}
	if v.reg.Policy.RequiresIsolation() {
		v.fail(p, PolicyDenied, fmt.Sprintf("the daemon's policy requires every binding to run in a sandbox or container, and executor %s runs processes; add a sandbox or container to the binding, or disable it in scope %s", b.Executor, scopeID))
		return
	}
	inherited := ""
	if e.isolationScope != scopeID {
		inherited = ", which scope " + scopeID + " inherits,"
	}
	v.fail(p, "isolation_required", fmt.Sprintf("scope %s requires every binding to run in a sandbox or container (its authorization.isolation)%s and executor %s runs processes; add a sandbox or container to the binding, or disable it in scope %s", e.isolationScope, inherited, b.Executor, scopeID))
}

// registration performs context-free executor, capability and resource checks.
func (v *validator) registration(p string, b Binding) {
	v.passEnv(p, b)
	if r := b.Requires; r.Workspace && b.Resource == nil {
		v.fail(p+"/resource", "resource_required", "workspace requires a bound resource provider; add resource or remove requires.workspace")
	}
	if b.Resource != nil && !v.reg.Resources[b.Resource.Provider] {
		v.fail(p+"/resource/provider", "unknown_registration", "resource provider is not registered; use a registered provider"+registeredNames(v.reg.Resources, b.Resource.Provider))
	} else if b.Resource != nil {
		if check := v.reg.ResourceConfig[b.Resource.Provider]; check != nil {
			v.owned(p+"/resource/config", check(b.Resource.Config), fmt.Sprintf("resource provider %s rejected this config; correct it against the provider's documented config", b.Resource.Provider))
		}
	}
	exec, ok := v.reg.Executors[b.Executor]
	if !ok {
		v.fail(p+"/executor", "unknown_registration", "executor is not registered; use a registered executor"+registeredNames(v.reg.Executors, b.Executor))
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
	if b.Sandbox != nil {
		if !c.Sandbox {
			v.fail(p+"/sandbox", "unsupported_capability", fmt.Sprintf("executor %s can't run in a sandbox; select an executor that can (shell, claude-code) or remove sandbox", b.Executor))
		}
		v.sandboxed(p, b)
	}
	if b.Container != nil {
		if b.Sandbox != nil {
			v.fail(p+"/container", "sandbox_conflict", "a binding declares sandbox or container, not both; remove one")
		}
		if !c.Container {
			v.fail(p+"/container", "unsupported_capability", fmt.Sprintf("executor %s can't run in a container; select an executor that can or remove container", b.Executor))
		}
		v.contained(p, b)
	}
	v.placed(p, b)
	if r.Toolbelt != "" && r.Toolbelt != c.Toolbelt {
		v.fail(p+"/requires/toolbelt", "unsupported_capability", fmt.Sprintf("executor %s uses toolbelt %s; select a matching executor or change requires.toolbelt", b.Executor, c.Toolbelt))
	}
	if err := extension.ValidateBinding(domain.Binding{Name: b.Name, Executor: b.Executor, Requires: r}, exec); errors.Is(err, extension.ErrInvalid) {
		v.fail(p+"/executor", "invalid_registration", "executor registration does not implement its advertised capabilities")
	}
	// Executor config is opaque to core and strictly checked by its owner.
	// Only the owner's diagnostics are echoed (owned), never its error text.
	if owner, ok := exec.(ConfigValidator); ok {
		if err := owner.ValidateConfig(b.Config, b.SecretNames); err != nil {
			v.owned(p+"/config", err, fmt.Sprintf("executor %s rejected this binding config; correct it against the executor's documented config", b.Executor))
			return // a grant's config would fail the same way
		}
		// Each grant's config is checked as the binding a Work that takes
		// it alone runs.
		base := domain.Binding{Config: domain.Payload{Data: b.Config}, Grants: b.domainGrants()}
		for k, g := range b.Grants {
			granted, _ := base.Granted([]string{g.Name})
			if err := owner.ValidateConfig(granted.Config.Data, b.SecretNamesWith([]string{g.Name})); err != nil {
				v.diags = append(v.diags, withGrant(p, k, g, err, fmt.Sprintf("executor %s rejected this binding config with the grant applied; correct it against the executor's documented config", b.Executor))...)
			}
		}
	}
}

// sandboxed checks a binding that declares a sandbox: a registered provider
// that accepts its config, an executor config (alone and with each grant)
// the executor can run in a sandbox (not for a container, whose executor
// keeps its config in the Work's own home), and a resource that wouldn't give the sandboxed
// executor a workspace that writes through to the operator's own files (a
// git worktree, whose commits go through the main checkout's .git) or runs
// setup outside the sandbox.
// passEnv fails closed on a pass_env name the host must never pass: a
// reserved one (domain.ReservedEnv), or one an env: secret reference in the
// document names, which only a binding's selected secrets may carry.
// The binding's resource's own pass_env follows the same rules.
func (v *validator) passEnv(p string, b Binding) {
	v.checkPassEnv(p+"/pass_env", b.PassEnv)
	if b.Resource != nil {
		v.checkPassEnv(p+"/resource/pass_env", b.Resource.PassEnv)
	}
}

// checkPassEnv applies the pass_env rules to one list of names at path, a
// binding's, its resource's or a package's (settings.extensions).
func (v *validator) checkPassEnv(path string, names []string) {
	refs := envSecretNames(v.doc)
	for i, name := range names {
		switch {
		case domain.ReservedEnv(name):
			v.fail(ptr(path, i), "pass_env_reserved", "OS_* and OPEN_CONDUIT_* variables are Open Conduit's own; remove it")
		case refs[name]:
			v.fail(ptr(path, i), "pass_env_secret", "an env: secret reference names this variable; select the secret with secret_names instead")
		case !v.reg.Policy.AllowsPassEnv(name):
			v.fail(ptr(path, i), PolicyDenied, "the daemon's policy does not allow pass_env to name "+name+"; use one of: "+listed(v.reg.Policy.PassEnv))
		}
	}
}

// envSecretNames are the variables the document's env: secret references
// name, on connections and scopes.
func envSecretNames(doc *Document) map[string]bool {
	out := map[string]bool{}
	add := func(refs map[string]string) {
		for _, ref := range refs {
			if name, ok := strings.CutPrefix(ref, "env:"); ok {
				out[name] = true
			}
		}
	}
	for _, c := range doc.Connections {
		add(c.SecretRefs)
	}
	for _, s := range doc.Scopes {
		add(s.SecretRefs)
	}
	return out
}

// tunnel checks settings.webhooks.tunnel against the packages: its type is
// one a package declares, and that package accepts its config.
func (v *validator) tunnel(t *Tunnel) {
	if t == nil {
		return
	}
	const p = "settings/webhooks/tunnel"
	check, ok := v.reg.Tunnels[t.Provider]
	if !ok {
		v.fail(p+"/provider", "unknown_registration", "no installed or bundled package provides this tunnel type; install one, or use public_url for a static URL"+registeredNames(v.reg.Tunnels, t.Provider))
	} else if check != nil {
		if err := check(t.Config); err != nil {
			v.fail(p+"/config", "invalid_config", err.Error())
		}
	}
}

// backupDestinations checks settings.backup.destinations against the
// packages: each type is one a package declares, which checks its config.
func (v *validator) backupDestinations(list []BackupDestination) {
	for i, d := range list {
		p := ptr("settings/backup/destinations", i)
		check, ok := v.reg.BackupDestinations[d.Type]
		if !ok {
			v.fail(p+"/type", "unknown_registration", "no installed or bundled package provides this backup destination type"+registeredNames(v.reg.BackupDestinations, d.Type))
		} else if check != nil {
			if err := check(d.Config); err != nil {
				v.fail(p+"/config", "invalid_config", err.Error())
			}
		}
	}
}

func (v *validator) sandboxed(p string, b Binding) {
	if b.Sandbox != nil {
		check, ok := v.reg.Sandboxes[b.Sandbox.Provider]
		if !ok {
			v.fail(p+"/sandbox/provider", "unknown_registration", "sandbox provider is not registered; use a registered provider"+registeredNames(v.reg.Sandboxes, b.Sandbox.Provider))
		} else if check != nil {
			if err := check(b.Sandbox.Config); err != nil {
				v.fail(p+"/sandbox/config", "invalid_config", err.Error())
			}
		}
	}
	if owner, ok := v.reg.Executors[b.Executor].(SandboxChecker); ok && b.Container == nil {
		err := owner.SandboxConfig(b.Config)
		if err != nil {
			v.fail(p+"/config", "sandbox_config", err.Error())
		}
		base := domain.Binding{Config: domain.Payload{Data: b.Config}, Grants: b.domainGrants()}
		for k, g := range b.Grants {
			if err != nil {
				break
			}
			granted, _ := base.Granted([]string{g.Name})
			if err := owner.SandboxConfig(granted.Config.Data); err != nil {
				v.fail(fmt.Sprintf("%s/grants/%d/config", p, k), "sandbox_config", err.Error())
			}
		}
	}
	if b.Resource == nil {
		return
	}
	check := v.reg.ResourceSandbox[b.Resource.Provider]
	if check == nil {
		return
	}
	var mode string
	if m, ok := v.reg.Executors[b.Executor].(ResourceModer); ok {
		mode = m.ResourceMode(b.Config)
	}
	if err := check(b.Resource.Config, mode); err != nil {
		v.fail(p+"/resource/config", "sandbox_workspace", err.Error())
	}
}

// placed refuses a binding whose resource can only be made where its
// launch is placed when its container provider doesn't place launches.
func (v *validator) placed(p string, b Binding) {
	if b.Resource == nil {
		return
	}
	needs := v.reg.ResourceSite[b.Resource.Provider]
	if needs == nil || !needs(b.Resource.Config) {
		return
	}
	if b.Container != nil && v.reg.Placers[b.Container.Provider] {
		return
	}
	v.fail(p+"/resource/config", "site_required", "this workspace is made on the machine the run executes on, not on the host; give the binding a container whose provider places launches, or a resource config made on the host")
}

// contained checks a binding that declares a container: the shared config
// (an image pinned by digest, the grant), a registered provider that
// accepts its part, and the executor and workspace checks a sandbox gets.
func (v *validator) contained(p string, b Binding) {
	if _, err := extension.ParseContainerConfig(b.Container.Config); err != nil {
		v.fail(p+"/container/config", "invalid_config", err.Error())
	}
	check, ok := v.reg.Containers[b.Container.Provider]
	if !ok {
		v.fail(p+"/container/provider", "unknown_registration", "container provider is not registered; use a registered provider"+registeredNames(v.reg.Containers, b.Container.Provider))
	} else if check != nil {
		if err := check(b.Container.Config); err != nil {
			v.fail(p+"/container/config", "invalid_config", err.Error())
		}
	}
	v.containerConnection(p, b)
	if b.Sandbox == nil {
		v.sandboxed(p, b)
	}
}

// containerConnection refuses a container that names a connection its
// provider's package can't resolve: one of a type another package, or
// none, declares. Whether the scope grants it is checked with the scope.
func (v *validator) containerConnection(p string, b Binding) {
	id := b.Container.ConnectionID()
	if id == "" {
		return
	}
	for _, c := range v.doc.Connections {
		if c.ID != id {
			continue
		}
		owner := v.reg.ContainerOwners[b.Container.Provider]
		if owner == "" || v.reg.ConnectionOwners[c.Type] != owner {
			v.fail(p+"/container/config/connection", "connection_type", fmt.Sprintf("connection %s is of type %s, which container provider %s doesn't declare; name a connection of a type its package declares, or remove connection", id, c.Type, b.Container.Provider))
		}
		return
	}
	v.fail(p+"/container/config/connection", "unknown_connection", "no connection has this id; define it under connections or remove connection")
}

// entrySandbox checks an extension entry's sandbox: only a type that
// declares one may have it, one that requires it must, and its provider
// must be registered and accept the policy.
func (v *validator) entrySandbox(p string, x Extension, t ExtensionType) {
	switch {
	case x.Sandbox == nil && t.Sandbox == "required":
		v.fail(p, "sandbox_required", "extension type "+x.Type+" runs processes for its entries that must be confined; add a sandbox with a registered provider")
	case x.Sandbox != nil && t.Sandbox == "":
		v.fail(p+"/sandbox", "unsupported_sandbox", "extension type "+x.Type+" runs nothing a sandbox would confine; remove sandbox")
	case x.Sandbox != nil:
		check, ok := v.reg.Sandboxes[x.Sandbox.Provider]
		if !ok {
			v.fail(p+"/sandbox/provider", "unknown_registration", "sandbox provider is not registered; use a registered provider"+registeredNames(v.reg.Sandboxes, x.Sandbox.Provider))
		} else if check != nil {
			if err := check(x.Sandbox.Config); err != nil {
				v.fail(p+"/sandbox/config", "invalid_config", err.Error())
			}
		}
	}
}

// SandboxChecker is optionally implemented by an executor registration to
// refuse a binding config it can't run safely in a sandbox (B09). Its error
// is a fixed, secret-free reason, shown in the diagnostic.
type SandboxChecker interface {
	SandboxConfig(config json.RawMessage) error
}

// ResourceModer is optionally implemented by an executor registration to say
// which resource mode a binding config makes it request, "" for the
// resource config's default (extension.ResourceModer).
type ResourceModer = extension.ResourceModer

// ConfigValidator is optionally implemented by an executor registration to
// reject its binding config at load/apply time instead of at launch.
type ConfigValidator interface {
	ValidateConfig(config json.RawMessage, secretNames []string) error
}

func (v *validator) operation(op string) bool {
	return slices.Contains(Builtins, op) || slices.Contains(v.reg.Operations, op)
}

// resolveOperations resolves an operations list as written (ResolveOperations)
// and reports what in it is wrong at path: an entry naming no registered
// operation or toolset (unknown_reference, so a typo never grants or fails
// to deny), and an operation named outside ceiling (code, with message).
// A nil ceiling is the root's: every registered operation. A toolset is
// narrowed to the ceiling rather than refused, so a release that adds an
// operation to it never stops a config that grants the toolset from
// loading; doctor reports what that leaves out (GrantWarnings).
func (v *validator) resolveOperations(list, ceiling []string, path, code, message string) []string {
	var ops, toolsets []string
	for _, entry := range list {
		name, toolset, _ := grantEntry(entry)
		if _, ok := v.reg.Toolsets[name]; toolset && !ok {
			toolsets = append(toolsets, entry)
		} else if !toolset && !v.operation(name) {
			ops = append(ops, entry)
		}
	}
	if len(ops) > 0 {
		v.fail(path, "unknown_reference", "operations name an unregistered operation: "+strings.Join(ops, ", ")+"; use a registered operation")
	}
	if len(toolsets) > 0 {
		name, _, _ := grantEntry(toolsets[0])
		v.fail(path, "unknown_reference", "operations name an unregistered toolset: "+strings.Join(toolsets, ", ")+
			"; name a registered toolset as @<name>"+registeredNames(v.reg.Toolsets, name))
	}
	if ceiling != nil {
		for _, entry := range list {
			if name, toolset, denied := grantEntry(entry); !toolset && !denied && v.operation(name) && !slices.Contains(ceiling, name) {
				v.fail(path, code, message)
				break
			}
		}
	}
	return ResolveOperations(list, v.reg, ceiling)
}

// declareEntryOperations asks the owner of each extension entry whose type
// declares operations which ones its config declares, before any operations
// list is resolved, and registers each for this validation as
// <owner>.<entry>.<name>, with the toolsets they join (an entry's name
// already has the form of a name segment, extension.EntryOperationSegment).
// It refuses a declaration the toolbelt couldn't register, and a name another operation, or another entry's different
// declaration, already has. Every declared entry counts, in any scope, so
// an authorization can grant what a descendant's entry declares; a
// binding is held to what its own scope has in effect (inEffect).
func (v *validator) declareEntryOperations() {
	var names []string
	toolsets := maps.Clone(v.reg.Toolsets)
	for i, sc := range v.doc.Scopes {
		for j, x := range sc.Extensions {
			t, ok := v.reg.Extensions[x.Type]
			if x.Disabled || !ok || t.Operations == nil {
				continue
			}
			p := fmt.Sprintf("scopes/%d/extensions/%d", i, j)
			var err error
			if t.Validate != nil {
				err = t.Validate(x.Config, x.SecretNames)
			}
			if v.entryChecked == nil {
				v.entryChecked, v.entryOps, v.slotOps = map[string]error{}, map[string]entryOperation{}, map[[2]int][]string{}
			}
			v.entryChecked[p] = err
			decls := t.Operations(x.Config)
			if err != nil || len(decls) == 0 {
				continue
			}
			for _, d := range decls {
				if extension.CheckEntryOperationDecl(d) != nil {
					v.fail(p+"/config", "invalid_operation", "the entry declares an operation whose name, toolset, summary, guidance or input schema the toolbelt can't register; fix it against the owner's documented config")
					continue
				}
				name := extension.EntryOperationName(t.Owner, x.Name, d.Name)
				if !extension.OperationName(name) {
					v.fail(p+"/config", "invalid_operation", "the entry declares an operation whose full name, <extension>.<entry>.<operation>, is longer than the toolbelt's 128 bytes; shorten the entry's name or the operation's")
					continue
				}
				if prev, ok := v.entryOps[name]; ok && !sameEntryOperation(prev.decl, d) {
					v.fail(p+"/config", "operation_conflict", "the entry declares "+name+" differently from the entry at "+prev.path+"; declare it the same in both, or give the entries different names")
					continue
				} else if !ok && v.operation(name) {
					v.fail(p+"/config", "operation_conflict", "the entry declares "+name+", which another operation already has; give the entry a different name")
					continue
				} else if !ok {
					v.entryOps[name] = entryOperation{owner: t.Owner, entry: x.Name, path: p, decl: d}
					names = append(names, name)
				}
				v.slotOps[[2]int{i, j}] = append(v.slotOps[[2]int{i, j}], name)
				if d.Toolset == "" {
					continue
				}
				ts := extension.EntryOperationName(t.Owner, x.Name, d.Toolset)
				if _, fixed := v.reg.Toolsets[ts]; fixed {
					v.fail(p+"/config", "operation_conflict", "the entry's operations join toolset "+ts+", which the extension already declares; give the entry a different name")
					continue
				}
				if toolsets == nil {
					toolsets = map[string]extension.Toolset{}
				}
				set := toolsets[ts]
				set.Name = ts
				if !slices.ContainsFunc(set.Operations, func(o extension.ToolsetOperation) bool { return o.Name == name }) {
					set.Operations = append(set.Operations, extension.ToolsetOperation{Name: name, Since: 1})
				}
				toolsets[ts] = set
			}
		}
	}
	if len(names) > 0 {
		v.reg.Operations = append(slices.Clone(v.reg.Operations), names...)
		v.reg.Toolsets = toolsets
	}
}

// sameEntryOperation reports whether two entries declare an operation the
// same way, so one registration serves both.
func sameEntryOperation(a, b extension.EntryOperationDecl) bool {
	return a.Name == b.Name && a.Summary == b.Summary && a.Guidance == b.Guidance && a.Toolset == b.Toolset && jsonEqual(a.Input, b.Input)
}

func jsonEqual(a, b json.RawMessage) bool {
	if len(a) == 0 || len(b) == 0 {
		return len(a) == len(b)
	}
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return string(a) == string(b)
	}
	ja, _ := json.Marshal(x)
	jb, _ := json.Marshal(y)
	return string(ja) == string(jb)
}

// inEffect is a binding's or grant's resolved operations less the entry
// operations no entry in effect in scope e declares, and refuses (at path)
// the list as written when it names one of those itself: an entry
// operation is granted only where its entry is. One a toolset brings is
// dropped instead, as a toolset is narrowed to the ceiling.
func (v *validator) inEffect(e *effective, list, resolved []string, path string) []string {
	if len(v.entryOps) == 0 {
		return resolved
	}
	missing := func(op string) bool {
		_, entry := v.entryOps[op]
		return entry && !slices.Contains(e.entryOps, op)
	}
	var named []string
	for _, item := range list {
		if name, toolset, denied := grantEntry(item); !toolset && !denied && missing(name) {
			named = append(named, name)
		}
	}
	if len(named) > 0 {
		v.fail(path, "operation_not_in_effect", "operations name entry operations no extension entry in effect in this scope declares: "+strings.Join(named, ", ")+"; add the entry to this scope or an ancestor, or remove them")
	}
	return slices.DeleteFunc(slices.Clone(resolved), missing)
}

// grantEntry splits an operations list entry into the name it grants or
// denies, whether that names a toolset and whether it is denied.
func grantEntry(entry string) (name string, toolset, denied bool) {
	entry, denied = strings.CutPrefix(entry, "!")
	name, toolset = strings.CutPrefix(entry, "@")
	return name, toolset, denied
}

// ResolveOperations resolves an operations list as written into operation
// names: the operations it names and those of the toolsets it names, less
// the ones it denies ("!op", or "!@toolset" for all of a toolset's). Only
// registered operations and builtins are granted, and only those in
// ceiling when it isn't nil: a toolset is narrowed to it. An unregistered
// toolset grants nothing. The result keeps the list's order, toolsets
// expanded in place, each operation once.
func ResolveOperations(list []string, reg Registry, ceiling []string) []string {
	known := func(op string) bool {
		return (slices.Contains(Builtins, op) || slices.Contains(reg.Operations, op)) && (ceiling == nil || slices.Contains(ceiling, op))
	}
	denied := map[string]bool{}
	for _, entry := range list {
		name, toolset, deny := grantEntry(entry)
		switch {
		case deny && toolset:
			for _, op := range reg.Toolsets[name].Names() {
				denied[op] = true
			}
		case deny:
			denied[name] = true
		}
	}
	out := []string{}
	add := func(op string) {
		if known(op) && !denied[op] && !slices.Contains(out, op) {
			out = append(out, op)
		}
	}
	for _, entry := range list {
		name, toolset, deny := grantEntry(entry)
		switch {
		case deny:
		case toolset:
			for _, op := range reg.Toolsets[name].Names() {
				add(op)
			}
		default:
			add(name)
		}
	}
	return out
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
	s.Settings.Budgets = slices.Clone(s.Settings.Budgets)
	s.Settings.Backup.TrustKeys = slices.Clone(s.Settings.Backup.TrustKeys)
	s.Settings.Backup.Destinations = slices.Clone(s.Settings.Backup.Destinations)
	s.Settings.Extensions = maps.Clone(s.Settings.Extensions)
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
			Concurrency: e.concurrency, AdmissionLimit: e.admission, Authorization: e.auth, IsolationScope: domain.ID(e.isolationScope),
			Bindings: e.activeBindings(), Routes: e.routes.active(), Extensions: e.extensions.active(), SecretRefs: e.refs, Source: sc.Source,
			EntryOperations: slices.Clone(e.entryOps)}
	}
	s.EntryOperations = v.registeredEntryOperations(s)
	s.Includes = v.doc.Submitted()
	s.FileHash = hash
	if file := *v.doc; len(s.Includes) > 0 {
		file.Scopes = nil
		for _, sc := range v.doc.Scopes {
			if !strings.HasPrefix(sc.Source, "submitted:") {
				file.Scopes = append(file.Scopes, sc)
			}
		}
		_, s.FileHash = file.Normalize()
	}
	return s
}

// submittedIncludes checks a document's submitted includes: unique names,
// each under a scope of the config file itself, and every submitted
// scope's include still declared.
func (v *validator) submittedIncludes() {
	names := map[string]bool{}
	for i, in := range v.doc.Includes {
		p := ptr("includes", i)
		if in.Name == "" {
			continue
		}
		if names[in.Name] {
			v.fail(p+"/name", "duplicate_value", "this include is already declared; name each include once")
		}
		names[in.Name] = true
		if k, ok := v.index[in.Parent]; !ok || v.doc.Scopes[k].Source != "" {
			v.fail(p+"/parent", "unknown_scope", "parent must be a scope defined in the config file itself")
		}
	}
	allows := map[string]IncludeAllow{}
	for _, in := range v.doc.Includes {
		if in.Name != "" {
			allows[in.Name] = in.Allow
		}
	}
	for i, sc := range v.doc.Scopes {
		name, ok := strings.CutPrefix(sc.Source, "submitted:")
		if !ok {
			continue
		}
		if !names[name] {
			v.fail(ptr("scopes", i)+"/source", "unknown_include", "this scope's submitted include is not declared")
			continue
		}
		v.submittedScope(ptr("scopes", i), sc, allows[name])
	}
}

// submittedScope holds a submitted scope to what its include allows: a
// binding it defines runs an allowed executor, an extension entry it
// declares is of an allowed type, and either selects only allowed secret
// names. Disabling an inherited one only narrows, so is always allowed.
func (v *validator) submittedScope(path string, sc ScopeConfig, allow IncludeAllow) {
	secrets := func(p string, names []string) {
		for m, n := range names {
			if !slices.Contains(allow.SecretNames, n) {
				v.fail(ptr(p+"/secret_names", m), "not_allowed", "the include doesn't let its fragments select this secret; ask the config file's owner to add it to the include's secret_names")
			}
		}
	}
	for k, b := range sc.Bindings {
		p := ptr(path+"/bindings", k)
		if b.Disabled {
			continue
		}
		if !slices.Contains(allow.Executors, b.Executor) {
			v.fail(p+"/executor", "not_allowed", "the include doesn't let its fragments define bindings for this executor; route to a binding the parent scope provides, or ask the config file's owner to add the executor to the include's executors")
		}
		secrets(p, b.SecretNames)
		for j, g := range b.Grants {
			secrets(fmt.Sprintf("%s/grants/%d", p, j), g.SecretNames)
		}
	}
	for k, x := range sc.Extensions {
		p := ptr(path+"/extensions", k)
		if x.Disabled {
			continue
		}
		if !slices.Contains(allow.ExtensionTypes, x.Type) {
			v.fail(p+"/type", "not_allowed", "the include doesn't let its fragments declare extension entries of this type; ask the config file's owner to add it to the include's extension_types")
		}
		secrets(p, x.SecretNames)
	}
}

// registeredEntryOperations are the entry operations in effect in the
// snapshot's enabled scopes, by owner, each with those scopes, sorted by
// name.
func (v *validator) registeredEntryOperations(s *Snapshot) map[string][]extension.EntryOperation {
	scopes := map[string][]domain.ID{}
	for _, id := range s.Order {
		if sc := s.Scopes[id]; sc.Enabled {
			for _, op := range sc.EntryOperations {
				scopes[op] = append(scopes[op], id)
			}
		}
	}
	var out map[string][]extension.EntryOperation
	for _, name := range slices.Sorted(maps.Keys(scopes)) {
		d := v.entryOps[name]
		if out == nil {
			out = map[string][]extension.EntryOperation{}
		}
		out[d.owner] = append(out[d.owner], extension.EntryOperation{Entry: d.entry, EntryOperationDecl: d.decl, Scopes: scopes[name]})
	}
	return out
}

// activeBindings are the scope's enabled bindings, in slot order, with their
// operations resolved.
func (e *effective) activeBindings() []Binding {
	var out []Binding
	for _, b := range e.bindings.active() {
		if r, ok := e.resolved[b.Name]; ok {
			b = r
		}
		out = append(out, b)
	}
	return out
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

// BindingConnections are the connections of connTypes that the scope's
// bindings name and its authorization grants, in document order, each once.
// They are what a binding's connection makes available to the extension
// that owns its type.
func (s *Snapshot) BindingConnections(scope domain.ID, connTypes map[string]bool) []Connection {
	e, ok := s.Scopes[scope]
	if !ok {
		return nil
	}
	var out []Connection
	for _, c := range s.Connections {
		if !connTypes[c.Type] || !slices.Contains(e.Authorization.Connections, c.ID) {
			continue
		}
		if slices.ContainsFunc(e.Bindings, func(b Binding) bool { return b.Connection == c.ID }) {
			out = append(out, c)
		}
	}
	return out
}

// ExtensionConnection returns connection id when an extension whose entry
// types are entryTypes, connection types connTypes and container provider
// types containerTypes may act as it in scope (B05): the connection is of
// one of connTypes and granted by the scope authorization, and either the
// scope has an entry of one of entryTypes and one of those entries or one
// of the scope's bindings names it, or one of the scope's bindings runs in
// a container of one of containerTypes whose config names it
// (extension.ContainerConfig.Connection).
func (s *Snapshot) ExtensionConnection(scope domain.ID, id string, entryTypes, connTypes, containerTypes map[string]bool) (Connection, bool) {
	e, ok := s.Scopes[scope]
	if !ok || id == "" || !slices.Contains(e.Authorization.Connections, id) {
		return Connection{}, false
	}
	entry, named := false, false
	for _, x := range e.Extensions {
		if entryTypes[x.Type] {
			entry = true
			named = named || x.Connection == id
		}
	}
	named = named || slices.ContainsFunc(e.Bindings, func(b Binding) bool { return b.Connection == id })
	contained := slices.ContainsFunc(e.Bindings, func(b Binding) bool {
		return b.Container != nil && containerTypes[b.Container.Provider] && b.Container.ConnectionID() == id
	})
	if !(entry && named) && !contained {
		return Connection{}, false
	}
	for _, c := range s.Connections {
		if c.ID == id && connTypes[c.Type] {
			return c, true
		}
	}
	return Connection{}, false
}

// GrantedConnection returns connection id of connTypes when scope is
// enabled and its authorization grants it, whether or not an entry or a
// binding there uses it yet: the authority a setup probe reads through.
func (s *Snapshot) GrantedConnection(scope domain.ID, id string, connTypes map[string]bool) (Connection, bool) {
	e, ok := s.Scopes[scope]
	if !ok || !e.Enabled || id == "" || !slices.Contains(e.Authorization.Connections, id) {
		return Connection{}, false
	}
	for _, c := range s.Connections {
		if c.ID == id && connTypes[c.Type] {
			return c, true
		}
	}
	return Connection{}, false
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

// AuthorizeWorkKind allows a binding to ask for child Work of kind: the
// binding names it in work_kinds and the scope authorization's work_kinds
// still has it.
func (s *Snapshot) AuthorizeWorkKind(scope domain.ID, binding, kind string) error {
	e, ok := s.Scopes[scope]
	if !ok || !e.Enabled {
		return ErrDenied
	}
	b, ok := e.Binding(binding)
	if !ok || !slices.Contains(e.Authorization.WorkKinds, kind) || !slices.Contains(b.WorkKinds, kind) {
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

// AuthorizeFile allows the source scope itself or an enabled file_scopes
// grant: filing an issue in, or commenting on, another scope's tracker.
func (s *Snapshot) AuthorizeFile(source, target domain.ID) error {
	src, ok := s.Scopes[source]
	if !ok || !src.Enabled {
		return ErrDenied
	}
	if source == target {
		return nil
	}
	dst, ok := s.Scopes[target]
	if !ok || !dst.Enabled || !slices.Contains(src.Authorization.FileScopes, string(target)) {
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
	Connection  string     `json:"connection,omitempty"`
	Operations  []string   `json:"operations"`
	SecretNames []string   `json:"secret_names"`
	Resource    *Resource  `json:"resource,omitempty"`
	Sandbox     *Sandbox   `json:"sandbox,omitempty"`
	Container   *Container `json:"container,omitempty"`
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
				Config: domain.Payload{Schema: BindingSchema, Data: slices.Clone(b.Config)}, PassEnv: slices.Clone(b.PassEnv), Grants: b.domainGrants()})
			meta.Bindings[b.Name] = bindingMeta{b.Connection, b.Operations, b.SecretNames, b.Resource, b.Sandbox, b.Container}
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
