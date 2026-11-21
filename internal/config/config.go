// Package config parses, validates and resolves version 1 configuration (B05).
// Validation never touches the network, resolves secrets or runs commands.
package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
)

// MaxSize bounds configuration, secrets files and command secret output reads.
const MaxSize = 1 << 20

var (
	idPattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,62}$`)
	// grantPattern is an entry of an operations list: an operation, a
	// toolset ("@github.author") or either denied ("!github.pr.edit").
	grantPattern = regexp.MustCompile(`^!?@?[a-z][a-z0-9_.-]*$`)
	envPattern   = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	// kindPattern is a Work kind an operator names in config: two or more
	// lower-case words joined by dots (agent.task.engineer).
	kindPattern = regexp.MustCompile(`^[a-z][a-z0-9_-]*(\.[a-z][a-z0-9_-]*)+$`)
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
// classifies a literal as a reference. Besides the built-in forms, a
// reference may name a provider scheme ("vault:secret/app#token"): its
// syntax is checked here, and whether an installed package declares the
// scheme is checked against the Registry (Registry.SecretSchemes).
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
	case idPattern.MatchString(scheme):
		return validProviderReference(rest)
	}
	return false
}

// maxProviderReference bounds a provider reference's text after the scheme.
const maxProviderReference = 1024

// validProviderReference is a provider reference's text after the scheme:
// nonempty, at most maxProviderReference bytes, without spaces or control
// characters, so it is one token the provider parses as its own.
func validProviderReference(rest string) bool {
	if rest == "" || len(rest) > maxProviderReference || !utf8.ValidString(rest) {
		return false
	}
	for _, r := range rest {
		if r <= ' ' || r == 0x7f || unicode.IsControl(r) || unicode.IsSpace(r) {
			return false
		}
	}
	return true
}

// ProviderScheme returns the scheme of a valid reference that a package
// resolves (not keyring:, file:, env: or cmd:), or "".
func ProviderScheme(ref string) string {
	if !ValidSecretRef(ref) {
		return ""
	}
	scheme, _, _ := strings.Cut(ref, ":")
	switch scheme {
	case "keyring", "file", "env", "cmd":
		return ""
	}
	return scheme
}

// DefaultKeptFinishedDays is settings.kept_finished_days when unset.
const DefaultKeptFinishedDays = 14

type Settings struct {
	Concurrency, AdmissionLimit, ReconcileSeconds, TimeoutSeconds, KeptExecutions int
	RetryDelaysSeconds                                                            []int
	CausalMaxDepth, CausalPerMinute                                               int
	// JobConcurrency bounds running job attempts (shell and other non-agent
	// executors) apart from Concurrency, which then bounds agent turns (#13).
	JobConcurrency int
	// KeptFinishedDays is how long a finished Work's runs keep their
	// content (logs, artifacts, executor state) after it last changed.
	KeptFinishedDays int
	// Webhooks is the stored webhook listener and tunnel (settings.webhooks);
	// serve flags override it for one run. Zero means reconcile-only.
	Webhooks Webhooks
	// Budgets are the usage limits the host enforces at admission
	// (settings.budgets); none means no limit.
	Budgets []Budget
	// Extensions are the operator's grants to installed or bundled
	// extension packages by package name (settings.extensions).
	Extensions map[string]PackageSettings
	// Backup is how bundles are signed and checked (settings.backup).
	Backup BackupSettings
	// ExtensionTrust is the operator's package provenance policy
	// (settings.extension_trust): nil when unset, and every installed
	// package is admitted as before.
	ExtensionTrust *ExtensionTrust
}

// ExtensionTrust says which installed extension packages may run: those a
// listed publisher signed, or whose digest is pinned. Bundled packages are
// the release's and are not subject to it.
type ExtensionTrust struct {
	Publishers []TrustedPublisher
	// Pinned are admitted digests by package name, as lower-case hex: a
	// package tree's (extension list and inspect show it) or its release
	// archive's SHA-256.
	Pinned map[string][]string
}

// TrustedPublisher is a Sigstore identity: an exact certificate Subject or
// a SubjectRegexp matching the whole subject, and its OIDC Issuer.
// Packages, when set, are the only packages it is trusted for.
type TrustedPublisher struct {
	Subject, SubjectRegexp, Issuer string
	Packages                       []string
}

// BackupSettings are settings.backup. SigningKey is a secret reference to
// the key scheduled and offline bundles are signed with; TrustKeys are the
// public keys `backup verify` and `backup restore` accept, as the config
// file gives them. A restore never takes them from the bundle it checks.
type BackupSettings struct {
	SigningKey string
	TrustKeys  []string
}

// IsZero says whether no backup signing is configured.
func (b BackupSettings) IsZero() bool { return b.SigningKey == "" && len(b.TrustKeys) == 0 }

// backupTrustKey matches a trust key's text form: ed25519: and the base64 of
// 32 bytes.
var backupTrustKey = regexp.MustCompile(`^ed25519:[A-Za-z0-9+/]{43}=$`)

// PackageSettings are one package's grants (settings.extensions.<name>).
type PackageSettings struct {
	// PassEnv names daemon variables the host adds to the package's own
	// process environment, on top of the sanitized baseline, when it starts
	// the package: an ssh agent a container provider reaches remotes with.
	// They reach that package alone, and a running package sees a change
	// only at its next start.
	PassEnv []string
}

// PackagePassEnv is the pass_env the settings grant the named package.
func (s Settings) PackagePassEnv(name string) []string {
	return slices.Clone(s.Extensions[name].PassEnv)
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

// Webhooks records how serve exposes the webhook listener: through a
// tunnel a package provides (Tunnel), or at a public URL the operator
// already routes to it (PublicURL, the static tunnel). They can't be
// combined, and neither means the listener is reachable only locally.
type Webhooks struct {
	Addr, PublicURL string
	Tunnel          *Tunnel
	// NoProbe turns off the host's probe of a static public URL ("probe":
	// false): the operator's own monitoring watches the route, and the
	// daemon makes no request through it.
	NoProbe bool
	// Forwarder, when set, is the one sender the listener accepts: a queue
	// or gateway that receives deliveries, holds them, and forwards each
	// with its MAC over the request, when it first received it and its own
	// delivery ID.
	Forwarder *Forwarder
}

// Forwarder is settings.webhooks.forwarder: who it is (Name, in logs and
// doctor), the reference of the secret it signs with, and how old a receipt
// time it attests may be.
type Forwarder struct {
	Name, Secret  string
	MaxAgeSeconds int
}

// Forwarder bounds: the receipt age a forwarder's attestation may carry
// when settings.webhooks.forwarder sets none, and the most it may set.
const (
	DefaultForwarderMaxAgeSeconds = 24 * 60 * 60
	MaxForwarderMaxAgeSeconds     = 30 * 24 * 60 * 60
)

// MaxAge is how old a receipt time the forwarder attests may be.
func (f Forwarder) MaxAge() time.Duration {
	if f.MaxAgeSeconds <= 0 {
		return DefaultForwarderMaxAgeSeconds * time.Second
	}
	return time.Duration(f.MaxAgeSeconds) * time.Second
}

// Tunnel is a webhook tunnel: a type some package declares (manifest
// tunnels) and its config, opaque to the host. The package checks the
// config (validate kind tunnel) and runs it as written (tunnel.run).
type Tunnel struct {
	Provider string          `json:"provider"`
	Config   json.RawMessage `json:"config,omitempty"`
}

// Equal says whether w and o expose the listener the same way.
func (w Webhooks) Equal(o Webhooks) bool {
	if w.Addr != o.Addr || w.PublicURL != o.PublicURL || w.NoProbe != o.NoProbe || (w.Tunnel == nil) != (o.Tunnel == nil) || (w.Forwarder == nil) != (o.Forwarder == nil) {
		return false
	}
	if w.Forwarder != nil && *w.Forwarder != *o.Forwarder {
		return false
	}
	return w.Tunnel == nil || (w.Tunnel.Provider == o.Tunnel.Provider && bytes.Equal(w.Tunnel.Config, o.Tunnel.Config))
}

// IsZero says whether w stores no listener.
func (w Webhooks) IsZero() bool { return w.Equal(Webhooks{}) }

type Authorization struct {
	Connections []string `json:"connections"`
	// Operations are the operations the scope's bindings may be granted.
	// As written, an entry is an operation, a toolset an extension
	// publishes ("@github.author", every operation it has in the running
	// release) or either denied ("!github.pr.edit"); in an EffectiveScope
	// they are resolved to operation names (ResolveOperations).
	Operations []string `json:"operations"`
	ReadScopes []string `json:"read_scopes"`
	// FileScopes are the other scopes whose trackers this scope's agents
	// may file issues in and comment on (optional; empty grants none).
	FileScopes []string `json:"file_scopes,omitempty"`
	// WorkKinds are the kinds of child Work this scope's bindings may be
	// granted to ask for (Binding.WorkKinds; optional, empty grants none).
	WorkKinds []string `json:"work_kinds,omitempty"`
	// Isolation is IsolationRequired when every binding in the scope must
	// run confined: in a sandbox or a container, unless its executor runs
	// no process the agent controls (domain.Capabilities.NoProcess). It is
	// the operator's own rule. A descendant scope inherits it and can't
	// drop it, whether it writes its own authorization or not. Empty
	// requires nothing.
	Isolation string `json:"isolation,omitempty"`
}

// IsolationRequired is Authorization.Isolation for a scope whose bindings
// must all run in a sandbox or container.
const IsolationRequired = "required"

// WorkKind reports whether s has the shape of a Work kind an operator names
// in config: lower-case words joined by dots (agent.task.engineer).
func WorkKind(s string) bool { return kindPattern.MatchString(s) }

type Resource struct {
	Provider string          `json:"provider"`
	Config   json.RawMessage `json:"config"`
	// PassEnv names daemon variables the provider's own commands and setup
	// see for the binding's acquisitions (extension.ResourceAcquisition.
	// HostEnv), apart from the binding's pass_env, which reaches the run:
	// an ssh agent git fetches with need not reach the agent.
	PassEnv []string `json:"pass_env,omitempty"`
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

// ConnectionID is the connection the container's config names for its
// provider to reach its API with (extension.ContainerConfig.Connection),
// "" for none or a config that isn't an object.
func (c *Container) ConnectionID() string {
	if c == nil {
		return ""
	}
	var cfg struct {
		Connection string `json:"connection"`
	}
	if json.Unmarshal(c.Config, &cfg) != nil {
		return ""
	}
	return cfg.Connection
}

type Binding struct {
	Name, Executor, Connection string
	Requires                   domain.Capabilities
	Operations, SecretNames    []string
	// PassEnv names host variables the binding's runs see
	// (domain.Binding.PassEnv).
	PassEnv []string
	// WorkKinds are the kinds of child Work the binding's runs may ask for
	// with create_work, within the scope's authorization.work_kinds; the
	// host refuses any other kind.
	WorkKinds []string
	Resource  *Resource
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
	// Playbook and PlaybookOperations are, in an EffectiveScope, the
	// playbook the binding's executor starts its turns on with its config
	// and the operations that playbook declares it relies on
	// (BindingPlaybook), worked out once when the generation is
	// validated. They are never parsed from config.
	Playbook           string
	PlaybookOperations []string
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
	// Sandbox confines the processes the owning package runs for this
	// entry (a stdio MCP server), as a binding's confines its executor:
	// the provider and its opaque policy, checked by the provider. Only an
	// entry type that declares it may have one (ExtensionType.Sandbox).
	Sandbox *Sandbox `json:"sandbox,omitempty"`
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
	// Includes are what a config file composes in. ParseFile reads folders
	// (Path) and submitted includes (Name); Compose replaces a folder with
	// its scopes and keeps the submitted includes, which a stored
	// generation records so a fragment can be submitted to one later.
	Includes []Include
}

// Include adds scopes under Parent, a scope of the config file itself:
// either a folder (Path) whose .open-conduit/config.json holds them, or a
// submitted include (Name) whose fragment an operator granted configure
// on Parent applies through the management API (ApplyFragment). One
// include is one or the other, never both.
type Include struct {
	Path, Name, Parent string
	// Allow is what a submitted include's fragments may use beyond
	// inheriting: nothing unless the config file says.
	Allow IncludeAllow
}

// IncludeAllow is what the config file's owner lets a submitted include's
// fragments use. A binding a fragment defines runs its executor with the
// config the fragment's author wrote, and an extension entry may start
// processes, so each executor and entry type is the owner's to allow, as
// is each inherited secret name a binding or entry selects. Empty allows
// none: the fragment routes to the bindings its parent provides.
type IncludeAllow struct {
	Executors, ExtensionTypes, SecretNames []string
}

func (a IncludeAllow) empty() bool {
	return len(a.Executors) == 0 && len(a.ExtensionTypes) == 0 && len(a.SecretNames) == 0
}

// normalized renders a submitted include; only what it allows joins it.
func (in Include) normalized() map[string]any {
	m := map[string]any{"name": in.Name, "parent": in.Parent}
	for k, v := range map[string][]string{"executors": in.Allow.Executors, "extension_types": in.Allow.ExtensionTypes, "secret_names": in.Allow.SecretNames} {
		if len(v) > 0 {
			m[k] = v
		}
	}
	return m
}

// Submitted is the document's submitted includes, in order.
func (d *Document) Submitted() []Include {
	var out []Include
	for _, in := range d.Includes {
		if in.Name != "" {
			out = append(out, in)
		}
	}
	return out
}

// SubmittedSource is the source a stored generation records for the scopes
// of the submitted include name. A folder's source is its absolute path, so
// the two never collide.
func SubmittedSource(name string) string { return "submitted:" + name }

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
	if is := n.field("includes"); is != nil && c.typed(is, "includes", kArray, "array") {
		for i, item := range is.items {
			p := ptr("includes", i)
			// A stored generation keeps only submitted includes: a folder's
			// scopes are already in it.
			keys := []string{"name", "executors", "extension_types", "secret_names"}
			if c.file {
				keys = append(keys, "path")
			}
			if !c.object(item, p, []string{"parent"}, keys...) {
				continue
			}
			var inc Include
			switch pn, nn := item.field("path"), item.field("name"); {
			case pn != nil && nn != nil:
				c.fail(p, "invalid_value", "an include is a folder (path) or submitted (name), not both")
			case pn != nil:
				if path, ok := c.str(pn, p+"/path", nil); ok && path == "" {
					c.fail(p+"/path", "invalid_value", "include path must be a folder")
				} else {
					inc.Path = path
				}
			case nn != nil:
				inc.Name, _ = c.str(nn, p+"/name", idPattern)
			case c.file:
				c.fail(p+"/path", "missing_field", "give the folder's path, or a name for an include whose fragment is submitted through the API")
			default:
				c.fail(p+"/name", "missing_field", "required field is missing")
			}
			inc.Parent, _ = c.str(item.field("parent"), p+"/parent", idPattern)
			inc.Allow = IncludeAllow{
				Executors:      c.list(item.field("executors"), p+"/executors", nil),
				ExtensionTypes: c.list(item.field("extension_types"), p+"/extension_types", nil),
				SecretNames:    c.list(item.field("secret_names"), p+"/secret_names", idPattern),
			}
			if inc.Path != "" && !inc.Allow.empty() {
				c.fail(p, "invalid_value", "executors, extension_types and secret_names bound a submitted include's fragments; a folder's are the config file owner's own")
			}
			doc.Includes = append(doc.Includes, inc)
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
	s := Settings{4, 0, 900, 3600, 20, []int{300, 600}, 32, 128, 4, DefaultKeptFinishedDays, Webhooks{}, nil, nil, BackupSettings{}, nil}
	fields := []struct {
		key string
		dst *int
	}{{"concurrency", &s.Concurrency}, {"admission_limit", &s.AdmissionLimit}, {"reconcile_seconds", &s.ReconcileSeconds},
		{"timeout_seconds", &s.TimeoutSeconds}, {"kept_executions", &s.KeptExecutions},
		{"causal_max_depth", &s.CausalMaxDepth}, {"causal_per_minute", &s.CausalPerMinute},
		{"job_concurrency", &s.JobConcurrency}, {"kept_finished_days", &s.KeptFinishedDays}}
	names := []string{"retry_delays_seconds", "webhooks", "budgets", "extensions", "backup", "extension_trust"}
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
	s.Extensions = c.packages(n.field("extensions"))
	s.Backup = c.backupSettings(n.field("backup"))
	s.ExtensionTrust = c.extensionTrust(n.field("extension_trust"))
	return s
}

// backupSettings checks settings.backup offline.
func (c *checker) backupSettings(n *node) BackupSettings {
	const path = "settings/backup"
	var b BackupSettings
	if n == nil || !c.object(n, path, nil, "signing_key", "trust_keys") {
		return b
	}
	if k := n.field("signing_key"); k != nil {
		if v, ok := c.str(k, path+"/signing_key", nil); ok {
			// The key's own text has the shape of a provider reference, and
			// a config generation goes into every bundle.
			if !ValidSecretRef(v) || strings.HasPrefix(v, "ed25519-private:") {
				c.fail(path+"/signing_key", "invalid_value", "use keyring:name, file:name, env:NAME, cmd:command or <scheme>:<reference> for a scheme an installed package provides (no literals, spaces or line breaks)")
				return b
			}
			b.SigningKey = v
		}
	}
	if r := n.field("trust_keys"); r != nil && c.typed(r, path+"/trust_keys", kArray, "array") {
		for i, item := range r.items {
			if v, ok := c.str(item, ptr(path+"/trust_keys", i), nil); ok {
				if !backupTrustKey.MatchString(v) {
					c.fail(ptr(path+"/trust_keys", i), "invalid_value", "a trust key is the public key `backup keygen` prints: ed25519:<base64>")
				}
				b.TrustKeys = append(b.TrustKeys, v)
			}
		}
	}
	return b
}

var (
	pinnedDigest = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	nonEmpty     = regexp.MustCompile(`\S`)
)

// extensionTrust checks settings.extension_trust offline. Present, even
// empty, it is a policy: an empty one admits no installed package.
func (c *checker) extensionTrust(n *node) *ExtensionTrust {
	const path = "settings/extension_trust"
	if n == nil || !c.object(n, path, nil, "publishers", "pinned") {
		return nil
	}
	out := &ExtensionTrust{Publishers: []TrustedPublisher{}, Pinned: map[string][]string{}}
	if pubs := n.field("publishers"); pubs != nil && c.typed(pubs, path+"/publishers", kArray, "array") {
		for i, item := range pubs.items {
			p := ptr(path+"/publishers", i)
			if !c.object(item, p, []string{"issuer"}, "subject", "subject_regexp", "packages") {
				continue
			}
			var tp TrustedPublisher
			tp.Issuer, _ = c.str(item.field("issuer"), p+"/issuer", nonEmpty)
			subject, regexpField := item.field("subject"), item.field("subject_regexp")
			switch {
			case (subject == nil) == (regexpField == nil):
				c.fail(p, "invalid_value", "a publisher names exactly one of subject (the certificate identity) or subject_regexp")
			case subject != nil:
				tp.Subject, _ = c.str(subject, p+"/subject", nonEmpty)
			default:
				if v, ok := c.str(regexpField, p+"/subject_regexp", nonEmpty); ok {
					if _, err := regexp.Compile("^(?:" + v + ")$"); err != nil {
						c.fail(p+"/subject_regexp", "invalid_value", "not a valid regular expression: "+err.Error())
					}
					tp.SubjectRegexp = v
				}
			}
			if pk := item.field("packages"); pk != nil {
				tp.Packages = c.list(pk, p+"/packages", idPattern)
			}
			out.Publishers = append(out.Publishers, tp)
		}
	}
	if pins := n.field("pinned"); pins != nil && c.typed(pins, path+"/pinned", kObject, "object") {
		for _, name := range pins.keys {
			p := ptr(path+"/pinned", name)
			if !idPattern.MatchString(name) {
				c.fail(p, "invalid_value", "a package name is lower case letters, digits, - and _, starting with a letter")
			}
			var digests []string
			for _, d := range c.list(pins.obj[name], p, pinnedDigest) {
				digests = append(digests, strings.TrimPrefix(d, "sha256:"))
			}
			out.Pinned[name] = digests
		}
	}
	return out
}

// packages checks settings.extensions offline: an object keyed by package
// name, each with an optional pass_env. Whether the package is installed is
// a warning (PackageWarnings), so a config survives removing one; which
// names pass_env may hold is checked with the document (validator.passEnv).
func (c *checker) packages(n *node) map[string]PackageSettings {
	const path = "settings/extensions"
	if n == nil || !c.typed(n, path, kObject, "object") {
		return nil
	}
	out := map[string]PackageSettings{}
	for _, name := range n.keys {
		p := ptr(path, name)
		if !idPattern.MatchString(name) {
			c.fail(p, "invalid_value", "a package name is lower case letters, digits, - and _, starting with a letter")
		}
		item := n.obj[name]
		if !c.object(item, p, nil, "pass_env") {
			continue
		}
		var ps PackageSettings
		if pe := item.field("pass_env"); pe != nil {
			ps.PassEnv = c.list(pe, p+"/pass_env", envPattern)
		}
		out[name] = ps
	}
	return out
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
// Whether a package declares the tunnel's type, and whether it accepts the
// tunnel's config, is checked against the registry (validator.tunnel).
func (c *checker) webhooks(n *node) Webhooks {
	const path = "settings/webhooks"
	var w Webhooks
	if n == nil || !c.typed(n, path, kObject, "object") {
		return w
	}
	moved := false
	for _, k := range n.keys {
		if msg, ok := movedTunnelField[k]; ok {
			c.fail(ptr(path, k), "moved_field", msg)
			moved = true
		}
	}
	if moved || !c.object(n, path, []string{"addr"}, "tunnel", "public_url", "probe", "forwarder") {
		return w
	}
	w.Addr, _ = c.str(n.field("addr"), path+"/addr", nil)
	w.PublicURL, _ = c.str(n.field("public_url"), path+"/public_url", nil)
	if t := n.field("tunnel"); t != nil {
		if t.kind == kString && t.str == "static" {
			c.fail(path+"/tunnel", "moved_field", "the static tunnel is gone: drop tunnel and keep public_url, the URL that already reaches the listener")
		} else if t.kind == kString {
			c.fail(path+"/tunnel", "moved_field", `tunnel is an object naming the providing package's type: use "tunnel": {"provider": "`+t.str+`", "config": {…}}, or public_url alone for a static URL`)
		} else if c.object(t, path+"/tunnel", []string{"provider"}, "config") {
			w.Tunnel = &Tunnel{}
			w.Tunnel.Provider, _ = c.str(t.field("provider"), path+"/tunnel/provider", idPattern)
			w.Tunnel.Config = c.opaque(t.field("config"), path+"/tunnel/config")
		}
	}
	if p := n.field("probe"); p != nil {
		if on, ok := c.boolean(p, path+"/probe"); ok && !on {
			w.NoProbe = true
			if w.PublicURL == "" {
				c.fail(path+"/probe", "invalid_value", "probe: false is for a static public_url; a tunnel's probe is how the host notices its URL stopped answering and starts it again")
			}
		}
	}
	if f := n.field("forwarder"); f != nil && c.object(f, path+"/forwarder", []string{"name", "secret"}, "max_age_seconds") {
		w.Forwarder = &Forwarder{}
		w.Forwarder.Name, _ = c.str(f.field("name"), path+"/forwarder/name", idPattern)
		if v, ok := c.str(f.field("secret"), path+"/forwarder/secret", nil); ok {
			if !ValidSecretRef(v) {
				c.fail(path+"/forwarder/secret", "invalid_value", "use keyring:name, file:name, env:NAME, cmd:command or <scheme>:<reference> for a scheme an installed package provides (no literals, spaces or line breaks)")
			}
			w.Forwarder.Secret = v
		}
		if m := f.field("max_age_seconds"); m != nil {
			if v, ok := c.integer(m, path+"/forwarder/max_age_seconds"); ok {
				if v < 60 || v > MaxForwarderMaxAgeSeconds {
					c.fail(path+"/forwarder/max_age_seconds", "invalid_value", fmt.Sprintf("max_age_seconds is 60 to %d (30 days)", MaxForwarderMaxAgeSeconds))
				}
				w.Forwarder.MaxAgeSeconds = v
			}
		}
	}
	if a := n.field("addr"); a != nil && a.kind == kString {
		if _, port, err := net.SplitHostPort(w.Addr); err != nil || port == "" {
			c.fail(path+"/addr", "invalid_value", "use host:port, for example 127.0.0.1:8787")
		}
	}
	for _, p := range w.TunnelProblems() {
		c.fail(path+"/"+p.Field, "invalid_value", p.Message)
	}
	return w
}

// movedTunnelField are the per-type tunnel fields settings.webhooks took
// before a tunnel's config became its package's: each names its
// replacement.
var movedTunnelField = map[string]string{
	"tunnel_domain":   `tunnel_domain moved into the ngrok package's config: use "tunnel": {"provider": "ngrok", "config": {"domain": …}}`,
	"tunnel_name":     `tunnel_name moved into the cloudflared package's config: use "tunnel": {"provider": "cloudflared", "config": {"name": …, "hostname": …}}`,
	"tunnel_hostname": `tunnel_hostname moved into the cloudflared package's config: use "tunnel": {"provider": "cloudflared", "config": {"name": …, "hostname": …}}`,
}

// TunnelProblem is one field of the webhook exposure that doesn't fit.
type TunnelProblem struct {
	Field, Message string
}

// TunnelProblems checks the host's part of the exposure: a tunnel and a
// static public URL exclude each other, and a public URL is one webhooks
// can point at. settings.webhooks is checked this way, and so are serve's
// flags of the same names. The tunnel's config is its package's to check.
func (w Webhooks) TunnelProblems() []TunnelProblem {
	var out []TunnelProblem
	if w.Tunnel != nil && w.PublicURL != "" {
		out = append(out, TunnelProblem{"public_url", "public_url is a static URL the operator already routes to the listener; use it or a tunnel, not both"})
	} else if w.PublicURL != "" && extension.CheckPublicURL(w.PublicURL) != nil {
		out = append(out, TunnelProblem{"public_url", "public_url needs an http(s) URL without credentials"})
	}
	return out
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
			c.fail(p, "invalid_value", "use keyring:name, file:name, env:NAME, cmd:command or <scheme>:<reference> for a scheme an installed package provides (no literals, spaces or line breaks)")
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
		out.Authorization = &Authorization{Connections: []string{}, Operations: []string{}, ReadScopes: []string{}, FileScopes: []string{}}
		if c.object(a, p, []string{"connections", "operations", "read_scopes"}, "file_scopes", "work_kinds", "isolation") {
			out.Authorization = &Authorization{
				Connections: c.list(a.field("connections"), p+"/connections", idPattern),
				Operations:  c.list(a.field("operations"), p+"/operations", grantPattern),
				ReadScopes:  c.list(a.field("read_scopes"), p+"/read_scopes", idPattern),
				FileScopes:  c.list(a.field("file_scopes"), p+"/file_scopes", idPattern),
			}
			if k := a.field("work_kinds"); k != nil {
				out.Authorization.WorkKinds = c.list(k, p+"/work_kinds", kindPattern)
			}
			if i := a.field("isolation"); i != nil {
				out.Authorization.Isolation, _ = c.enum(i, p+"/isolation", IsolationRequired)
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
	optional := []string{"requires", "operations", "secret_names", "config", "connection", "resource", "sandbox", "container", "timeout_seconds", "grants", "work_kinds"}
	if !c.fragment {
		// A folder's bindings pass no daemon variables: like its scopes'
		// secret references, pass_env stays in the config file, so a
		// checkout can't read the daemon's environment.
		optional = append(optional, "pass_env")
	}
	if !c.object(n, path, []string{"name", "executor"}, optional...) {
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
	b.Operations = c.list(n.field("operations"), path+"/operations", grantPattern)
	b.SecretNames = c.list(n.field("secret_names"), path+"/secret_names", idPattern)
	if k := n.field("work_kinds"); k != nil {
		b.WorkKinds = c.list(k, path+"/work_kinds", kindPattern)
	}
	if pe := n.field("pass_env"); pe != nil {
		b.PassEnv = c.list(pe, path+"/pass_env", envPattern)
	}
	var resourceOptional []string
	if !c.fragment {
		// As for the binding's own pass_env, a folder's resource passes
		// no daemon variables.
		resourceOptional = append(resourceOptional, "pass_env")
	}
	if r := n.field("resource"); r != nil && c.object(r, path+"/resource", []string{"provider", "config"}, resourceOptional...) {
		b.Resource = &Resource{}
		b.Resource.Provider, _ = c.str(r.field("provider"), path+"/resource/provider", idPattern)
		b.Resource.Config = c.opaque(r.field("config"), path+"/resource/config")
		if pe := r.field("pass_env"); pe != nil {
			b.Resource.PassEnv = c.list(pe, path+"/resource/pass_env", envPattern)
		}
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
			grant.Operations = c.list(item.field("operations"), p+"/operations", grantPattern)
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
	if !c.object(n, path, []string{"name", "handler", "binding", "active_policy"}, "config", "unsupported_fallback", "error_fallback") {
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
	if !c.object(n, path, []string{"name", "type"}, "config", "secret_names", "connection", "sandbox") {
		return ExtensionEntry{Extension: e}
	}
	e.Name, _ = c.str(n.field("name"), path+"/name", idPattern)
	e.Type, _ = c.str(n.field("type"), path+"/type", idPattern)
	if conn := n.field("connection"); conn != nil {
		e.Connection, _ = c.str(conn, path+"/connection", idPattern)
	}
	e.Config = c.opaque(n.field("config"), path+"/config")
	e.SecretNames = c.list(n.field("secret_names"), path+"/secret_names", idPattern)
	if r := n.field("sandbox"); r != nil && c.object(r, path+"/sandbox", []string{"provider", "config"}) {
		e.Sandbox = &Sandbox{}
		e.Sandbox.Provider, _ = c.str(r.field("provider"), path+"/sandbox/provider", idPattern)
		e.Sandbox.Config = c.opaque(r.field("config"), path+"/sandbox/config")
	}
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
		"concurrency": s.Concurrency, "reconcile_seconds": s.ReconcileSeconds,
		"timeout_seconds": s.TimeoutSeconds, "kept_executions": s.KeptExecutions, "retry_delays_seconds": s.RetryDelaysSeconds,
		"causal_max_depth": s.CausalMaxDepth, "causal_per_minute": s.CausalPerMinute, "job_concurrency": s.JobConcurrency,
	}
	// admission_limit has no default (#706): only a set one is written.
	if s.AdmissionLimit > 0 {
		settings["admission_limit"] = s.AdmissionLimit
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
	// Only a changed kept_finished_days joins the hash, so configurations
	// written before it keep their generation hash.
	if s.KeptFinishedDays != DefaultKeptFinishedDays {
		settings["kept_finished_days"] = s.KeptFinishedDays
	}
	// Only a stored listener joins the hash, so configurations without one
	// keep their generation hash.
	if w := s.Webhooks; !w.IsZero() {
		hooks := map[string]any{"addr": w.Addr}
		if w.PublicURL != "" {
			hooks["public_url"] = w.PublicURL
		}
		if w.Tunnel != nil {
			hooks["tunnel"] = w.Tunnel
		}
		if w.NoProbe {
			hooks["probe"] = false
		}
		if f := w.Forwarder; f != nil {
			fw := map[string]any{"name": f.Name, "secret": f.Secret}
			if f.MaxAgeSeconds != 0 {
				fw["max_age_seconds"] = f.MaxAgeSeconds
			}
			hooks["forwarder"] = fw
		}
		settings["webhooks"] = hooks
	}
	// Only configured package grants join the hash, so configurations
	// without them keep their generation hash.
	if len(s.Extensions) > 0 {
		pkgs := map[string]any{}
		for name, ps := range s.Extensions {
			m := map[string]any{}
			if len(ps.PassEnv) > 0 {
				m["pass_env"] = ps.PassEnv
			}
			pkgs[name] = m
		}
		settings["extensions"] = pkgs
	}
	// Only configured signing joins the hash, so configurations without it
	// keep their generation hash.
	if b := s.Backup; !b.IsZero() {
		m := map[string]any{}
		if b.SigningKey != "" {
			m["signing_key"] = b.SigningKey
		}
		if len(b.TrustKeys) > 0 {
			m["trust_keys"] = b.TrustKeys
		}
		settings["backup"] = m
	}
	// Only a configured policy joins the hash, so configurations without
	// one keep their generation hash; it must survive normalization, or a
	// stored generation would start packages with no policy.
	if t := s.ExtensionTrust; t != nil {
		pubs := []any{}
		for _, p := range t.Publishers {
			m := map[string]any{"issuer": p.Issuer}
			if p.SubjectRegexp != "" {
				m["subject_regexp"] = p.SubjectRegexp
			} else {
				m["subject"] = p.Subject
			}
			if p.Packages != nil {
				m["packages"] = p.Packages
			}
			pubs = append(pubs, m)
		}
		pinned := map[string]any{}
		for name, ds := range t.Pinned {
			list := []string{}
			for _, d := range ds {
				list = append(list, "sha256:"+d)
			}
			pinned[name] = list
		}
		settings["extension_trust"] = map[string]any{"publishers": pubs, "pinned": pinned}
	}
	out := map[string]any{
		"version":     d.Version,
		"settings":    settings,
		"connections": connections,
		"scopes":      scopes,
	}
	// Only submitted includes are stored, and only a document with one
	// carries the list, so configurations without them keep their hash.
	if submitted := d.Submitted(); len(submitted) > 0 {
		list := []any{}
		for _, in := range submitted {
			list = append(list, in.normalized())
		}
		out["includes"] = list
	}
	return out
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
	if len(b.PassEnv) > 0 {
		m["pass_env"] = b.PassEnv
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
	if len(b.WorkKinds) > 0 {
		m["work_kinds"] = b.WorkKinds
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
