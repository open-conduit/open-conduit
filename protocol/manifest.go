package protocol

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/open-conduit/open-conduit/extension"
)

// ManifestFile is the manifest's file name inside an extension's directory.
const ManifestFile = "open-conduit-extension.json"

// Manifest declares what an extension process provides, before it runs. The
// host reads it at discovery, validates it offline (config validation does
// not need the process), and checks at the handshake that the process
// returns the same manifest. Everything is mechanical: names, event
// families, operation kinds and capability labels. There is no storage
// capability: every extension gets the namespaced KV under its name, and a
// store larger than coordination state is an explicit connection.
type Manifest struct {
	// Protocol lists the protocol versions the extension speaks.
	Protocol []int `json:"protocol"`
	// Name is the registration name: the root of every name the extension
	// owns (KV namespaces, event families, operations, kinds, source
	// namespaces, executor and type names, webhook paths).
	Name string `json:"name"`
	// Version is the extension's own version, informational for
	// diagnostics and compatibility reports.
	Version string `json:"version"`
	Summary string `json:"summary,omitempty"`
	// Command is the argv that starts the process; a relative argv[0] is
	// resolved against the manifest's directory. It receives the protocol
	// on stdin/stdout; stderr is its diagnostic log.
	Command []string `json:"command"`
	// KVVersion is the layout version of the extension's own KV namespaces
	// (07-02). The host records the version an extension last migrated to
	// and refuses to start a manifest declaring an older one (kv_downgrade).
	KVVersion int `json:"kv_version,omitempty"`
	// Permissions are the host services beyond the extension's own surfaces
	// it asks to use: the operator sees them at install and in `extension
	// list`, and the host refuses a call one of them gates unless it is
	// declared here. Packages are not sandboxed, so a permission is consent
	// and a guard against accidental leaks, but it is enforced as though it
	// were the boundary.
	Permissions []string `json:"permissions,omitempty"`
	Provides    Provides `json:"provides"`
}

// Permissions an extension may declare.
const (
	// PermissionTelemetrySink receives every named telemetry event the host
	// writes to its telemetry events log (telemetry.sink), from every
	// scope: it sees the whole machine's activity.
	PermissionTelemetrySink = "telemetry.sink"
	// PermissionTelemetryCapture writes named telemetry events to the
	// host's telemetry events log, which every sink receives
	// (telemetry.capture).
	PermissionTelemetryCapture = "telemetry.capture"
	// PermissionProbes lists and runs the setup probes the host, its
	// extensions and its executors offer (probe.list, probe.run): read-only
	// checks of the machine, such as whether Claude Code is signed in.
	PermissionProbes = "probes"
)

// PermissionInfo says what a permission lets an extension do, in words the
// operator reads at install.
type PermissionInfo struct {
	Name, Summary string
}

// KnownPermissions are the permissions this host understands, in the order
// they are shown.
var KnownPermissions = []PermissionInfo{
	{PermissionTelemetrySink, "receives every telemetry event the host records, from every scope"},
	{PermissionTelemetryCapture, "records telemetry events, which every telemetry sink receives"},
	{PermissionProbes, "runs the read-only setup probes of the host, its extensions and executors"},
}

// PermissionSummary is the summary of a known permission ("" otherwise).
func PermissionSummary(name string) string {
	for _, p := range KnownPermissions {
		if p.Name == name {
			return p.Summary
		}
	}
	return ""
}

// Allows reports whether the manifest declares permission.
func (m Manifest) Allows(permission string) bool {
	for _, p := range m.Permissions {
		if p == permission {
			return true
		}
	}
	return false
}

// Provides lists the surfaces an extension implements. Each enables the
// catalogue methods whose Method.Provides names it; the host never calls,
// and refuses callbacks from, a surface the manifest does not declare.
type Provides struct {
	// Connections are config connection types the extension validates.
	Connections []TypeDecl `json:"connections,omitempty"`
	// Extensions are scope extension entry types; Ingress marks an entry
	// that registers ingress sources at configure time.
	Extensions []ExtensionDecl `json:"extensions,omitempty"`
	// Families are every event family the extension produces through
	// ingress, event.accept or toolbelt emission, with recoverability.
	Families []Family `json:"families,omitempty"`
	// Schemas are the payload schemas the extension writes and reads, each
	// with the versions it can read; the highest is the one it writes
	// (07-02, schema evolution).
	Schemas []SchemaDecl `json:"schemas,omitempty"`
	// Routes are route handler names; Config marks a handler that
	// validates its routes' opaque config.
	Routes []RouteDecl `json:"routes,omitempty"`
	// WorkKinds are the Work kinds the extension's routes create, with
	// their lifecycle policy.
	WorkKinds []WorkKindDecl `json:"work_kinds,omitempty"`
	// Recurrences are calendar recurrence kinds for timer.arm.
	Recurrences []string `json:"recurrences,omitempty"`
	// Operations are toolbelt operations the extension serves.
	Operations []OperationDecl `json:"operations,omitempty"`
	// DependencyFilter marks an extension whose wait dependencies carry an
	// opaque filter the host evaluates through dependency.filter.
	DependencyFilter bool `json:"dependency_filter,omitempty"`
	// Executors are executor registrations with their mechanical
	// capabilities and optional controls.
	Executors []ExecutorDecl `json:"executors,omitempty"`
	// Resources are resource provider types.
	Resources []ResourceDecl `json:"resources,omitempty"`
	// Tasks are host-paced background passes (projection, timer ensure,
	// executor housekeeping, webhook management).
	Tasks []TaskDecl `json:"tasks,omitempty"`
	// Webhooks marks an extension that receives webhook.handle for the
	// paths its configure result registers under /<name>/.
	Webhooks bool `json:"webhooks,omitempty"`
	// Exporters marks a telemetry exporter (telemetry.export).
	Exporters bool `json:"exporters,omitempty"`
	// Checks are doctor checks; Deep checks run only with secrets.
	Checks []CheckDecl `json:"checks,omitempty"`
	// Setup is the `open-conduit init <type>` flow.
	Setup *SetupDecl `json:"setup,omitempty"`
	// Annotate marks an extension that contributes Work annotations.
	Annotate bool `json:"annotate,omitempty"`
	// RunEnvironment marks an extension that contributes an environment
	// layer to the runs of Work in the scopes that use it (run.environment).
	RunEnvironment bool `json:"run_environment,omitempty"`
	// Actions marks an extension that offers actions on scopes and Work
	// (work.actions, work.act).
	Actions bool `json:"actions,omitempty"`
	// Panels marks an extension that shows panels on scope and Work pages
	// (view.render). Their buttons invoke its actions.
	Panels bool `json:"panels,omitempty"`
	// Render marks an extension that renders its own families for agent
	// prompts (event.render).
	Render bool `json:"render,omitempty"`
	// Playbooks is a directory, relative to the manifest, of prompt
	// material the host offers agent executors (a first-party convention).
	Playbooks string `json:"playbooks,omitempty"`
	// ExternalStores declare stores larger than coordination state (search
	// indexes, big tables). Each is reached through a connection of the
	// named type, so it is visible in config and never hidden in KV.
	ExternalStores []ExternalStore `json:"external_stores,omitempty"`
}

type TypeDecl struct {
	Type string `json:"type"`
}
type ExtensionDecl struct {
	Type    string `json:"type"`
	Ingress bool   `json:"ingress,omitempty"`
	// Settings are the entry config fields that are safe to show the
	// operator, with their value types. The host reads them from an entry's
	// config itself, so the operator's view of the config (and `config
	// check`) needs no process; every other field is shown by name alone.
	Settings []SettingDecl `json:"settings,omitempty"`
	// Describe lists what an entry sets up (a schedule's jobs), read by the
	// host from the entry's config.
	Describe *DescribeDecl `json:"describe,omitempty"`
}

// Setting value types.
const (
	SettingString  = "string"
	SettingBoolean = "boolean"
	SettingNumber  = "number"
	SettingStrings = "strings"
)

// SettingDecl is one top-level entry config field safe to show the operator
// (extension.Setting). A field missing from the config shows Default when
// one is declared, and is left out otherwise; a value of another type is
// left out. It must never name a field that can carry a credential.
type SettingDecl struct {
	Field   string          `json:"field"`
	Type    string          `json:"type"`
	Default json.RawMessage `json:"default,omitempty"`
}

// DescribeDecl says what an entry sets up: one line per element of the
// top-level array field List, named by the element's Name field and
// detailed by its Detail field (a string, number or boolean; "" when
// absent). Elements without a string Name are skipped.
type DescribeDecl struct {
	List   string `json:"list"`
	Name   string `json:"name"`
	Detail string `json:"detail,omitempty"`
}

var fieldPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_-]{0,62}$`)

// Value is the setting's value in an entry config object: the field's
// value when it has the declared type, else Default when the field is
// absent; false when there is none.
func (d SettingDecl) Value(config map[string]json.RawMessage) (any, bool) {
	raw, ok := config[d.Field]
	if !ok || string(raw) == "null" {
		if len(d.Default) == 0 {
			return nil, false
		}
		raw = d.Default
	}
	return settingValue(d.Type, raw)
}

func settingValue(typ string, raw json.RawMessage) (any, bool) {
	var ok bool
	switch typ {
	case SettingString:
		var v string
		ok = json.Unmarshal(raw, &v) == nil
		return v, ok
	case SettingBoolean:
		var v bool
		ok = json.Unmarshal(raw, &v) == nil
		return v, ok
	case SettingNumber:
		var v float64
		ok = json.Unmarshal(raw, &v) == nil
		if ok && v == float64(int64(v)) {
			return int64(v), true
		}
		return v, ok
	case SettingStrings:
		var v []string
		ok = json.Unmarshal(raw, &v) == nil
		if v == nil {
			v = []string{}
		}
		return v, ok
	}
	return nil, false
}

// DescribeEntry is one line DescribeDecl yields.
type DescribeEntry struct{ Name, Detail string }

// Entries are the lines an entry config yields.
func (d DescribeDecl) Entries(config map[string]json.RawMessage) []DescribeEntry {
	var items []map[string]json.RawMessage
	if json.Unmarshal(config[d.List], &items) != nil {
		return nil
	}
	var out []DescribeEntry
	for _, item := range items {
		var name string
		if json.Unmarshal(item[d.Name], &name) != nil || name == "" {
			continue
		}
		e := DescribeEntry{Name: name}
		if raw, ok := item[d.Detail]; ok && d.Detail != "" {
			var v any
			if json.Unmarshal(raw, &v) == nil {
				switch x := v.(type) {
				case string:
					e.Detail = x
				case float64, bool:
					e.Detail = strings.TrimSpace(string(raw))
				}
			}
		}
		out = append(out, e)
	}
	return out
}

// Family mirrors extension.EventFamily: a family is re-derivable through
// reconcile, or states what is lost without push.
type Family struct {
	Type        string `json:"type"`
	ReDerivable bool   `json:"re_derivable"`
	Loss        string `json:"loss,omitempty"`
}
type RouteDecl struct {
	Handler string `json:"handler"`
	Config  bool   `json:"config,omitempty"`
	// Samples marks a handler that offers sample Events for a route trace
	// through route.samples (extension.RouteSampler).
	Samples bool `json:"samples,omitempty"`
}

// WorkKindDecl mirrors the host's WorkKindPolicy; Trigger marks a kind whose
// trigger selection the host asks for through work_kind.trigger.
type WorkKindDecl struct {
	Kind     string `json:"kind"`
	Standing bool   `json:"standing,omitempty"`
	Trigger  bool   `json:"trigger,omitempty"`
}

// Operation kinds mirror extension.Operation's three mutually exclusive
// handler fields.
const (
	OperationHandler = "handler"
	OperationWait    = "wait"
	OperationEmit    = "emit"
)

// OperationDecl declares one toolbelt operation. Commit marks an emit whose
// plan writes records through emit.commit. Guidance is the operation's
// agent guidance (extension.Operation.Guidance), served to executions whose
// grant includes it.
type OperationDecl struct {
	Name     string `json:"name"`
	Kind     string `json:"kind"`
	Commit   bool   `json:"commit,omitempty"`
	Guidance string `json:"guidance,omitempty"`
}

// ExecutorDecl declares an executor. Capabilities are exactly the bind-time
// labels of domain.Capabilities; Controls are optional host-driven methods.
type ExecutorDecl struct {
	Name         string       `json:"name"`
	Capabilities Capabilities `json:"capabilities"`
	Controls     Controls     `json:"controls,omitempty"`
}

// Capabilities are the PRD's mechanical labels, checked at bind time. No
// semantic suitability claim is expressible.
type Capabilities struct {
	Interrupt bool   `json:"interrupt,omitempty"`
	Inject    bool   `json:"inject,omitempty"`
	Resume    bool   `json:"resume,omitempty"`
	Workspace bool   `json:"workspace,omitempty"`
	Toolbelt  string `json:"toolbelt"`
}

// Controls are optional executor methods that are not capabilities: stop
// (physical-stop evidence), forget (retention cleanup) and attach (operator
// takeover).
type Controls struct {
	Stop   bool `json:"stop,omitempty"`
	Forget bool `json:"forget,omitempty"`
	Attach bool `json:"attach,omitempty"`
}
type ResourceDecl struct {
	Type  string   `json:"type"`
	Modes []string `json:"modes"`
}
type TaskDecl struct {
	Name            string `json:"name"`
	IntervalSeconds int    `json:"interval_seconds"`
}
type CheckDecl struct {
	Name string `json:"name"`
	Deep bool   `json:"deep,omitempty"`
}
type SetupDecl struct {
	Type    string `json:"type"`
	Summary string `json:"summary"`
	Usage   string `json:"usage,omitempty"`
}
type ExternalStore struct {
	Connection  string `json:"connection"`
	Description string `json:"description"`
}

// ErrManifest wraps every manifest validation failure.
var ErrManifest = errors.New("protocol: invalid manifest")

// ErrNewerManifest refuses a manifest that uses a field this module does not
// know. Additive surfaces arrive in a release without a protocol version
// bump, so an unknown field means the package was built for a newer Open
// Conduit, not that it is broken.
var ErrNewerManifest = errors.New("protocol: manifest field from a newer Open Conduit")

var (
	namePattern  = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,62}$`)
	identPattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,62}$`)
	dottedPart   = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)
)

// reserved names belong to core: its event families and sources (core.*,
// execution/<id>), the toolbelt protocol identifiers and the product.
var reserved = map[string]bool{"core": true, "execution": true, "os": true, "open-conduit": true, "manual": true}

// ParseManifest decodes a manifest strictly (unknown fields, including any
// attempt to negotiate storage, are rejected with ErrNewerManifest) and
// validates it.
func ParseManifest(data []byte) (Manifest, error) {
	var m Manifest
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		if field, ok := strings.CutPrefix(err.Error(), "json: unknown field "); ok {
			return Manifest{}, fmt.Errorf("%w: %s", ErrNewerManifest, field)
		}
		return Manifest{}, fmt.Errorf("%w: %v", ErrManifest, err)
	}
	if dec.More() {
		return Manifest{}, fmt.Errorf("%w: trailing data", ErrManifest)
	}
	return m, m.Validate()
}

// Validate checks the manifest offline: a supported protocol version, the
// namespacing of every owned name, operation and family consistency,
// capability labels and duplicate declarations.
func (m Manifest) Validate() error {
	var errs []error
	fail := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	if _, err := Negotiate(m.Protocol, Versions); err != nil {
		fail("protocol %v: %v", m.Protocol, err)
	}
	if !namePattern.MatchString(m.Name) || reserved[m.Name] {
		fail("name %q must match %s and not be reserved", m.Name, namePattern)
	}
	if m.Version == "" {
		fail("version is required")
	}
	if len(m.Command) == 0 || m.Command[0] == "" {
		fail("command is required")
	} else if !filepath.IsAbs(m.Command[0]) && strings.HasPrefix(filepath.Clean(m.Command[0]), "..") {
		fail("command %q leaves the extension directory", m.Command[0])
	}
	granted := map[string]bool{}
	for _, perm := range m.Permissions {
		if PermissionSummary(perm) == "" {
			// A permission this host does not know is from a newer release,
			// as an unknown field is.
			return fmt.Errorf("%w: permission %q", ErrNewerManifest, perm)
		}
		if granted[perm] {
			fail("duplicate permission %q", perm)
		}
		granted[perm] = true
	}
	p := m.Provides
	seen := map[string]bool{}
	unique := func(kind, name string) {
		if seen[kind+"\x00"+name] {
			fail("duplicate %s %q", kind, name)
		}
		seen[kind+"\x00"+name] = true
	}
	// Identifier-shaped names (config types, handlers, executors,
	// providers, tasks, checks) are the name itself or name-<suffix>.
	ident := func(kind, v string) {
		unique(kind, v)
		if !identPattern.MatchString(v) || (v != m.Name && !strings.HasPrefix(v, m.Name+"-")) {
			fail("%s %q must be %q or begin with %q", kind, v, m.Name, m.Name+"-")
		}
	}
	// Dotted names (families, operations, kinds, recurrences) begin with
	// name + ".".
	dotted := func(kind, v string) {
		unique(kind, v)
		rest, ok := strings.CutPrefix(v, m.Name+".")
		if !ok || rest == "" {
			fail("%s %q must begin with %q", kind, v, m.Name+".")
			return
		}
		for _, part := range strings.Split(rest, ".") {
			if !dottedPart.MatchString(part) {
				fail("%s %q has an invalid segment %q", kind, v, part)
				return
			}
		}
	}
	for _, c := range p.Connections {
		ident("connection type", c.Type)
	}
	ingress := false
	for _, x := range p.Extensions {
		ident("extension type", x.Type)
		ingress = ingress || x.Ingress
		fields := map[string]bool{}
		for _, st := range x.Settings {
			if !fieldPattern.MatchString(st.Field) || fields[st.Field] {
				fail("extension type %q setting %q must be a distinct field name", x.Type, st.Field)
			}
			fields[st.Field] = true
			switch st.Type {
			case SettingString, SettingBoolean, SettingNumber, SettingStrings:
				if len(st.Default) > 0 {
					if _, ok := settingValue(st.Type, st.Default); !ok {
						fail("extension type %q setting %q default is not a %s", x.Type, st.Field, st.Type)
					}
				}
			default:
				fail("extension type %q setting %q type %q must be string, boolean, number or strings", x.Type, st.Field, st.Type)
			}
		}
		if d := x.Describe; d != nil {
			if !fieldPattern.MatchString(d.List) || !fieldPattern.MatchString(d.Name) || d.Detail != "" && !fieldPattern.MatchString(d.Detail) {
				fail("extension type %q describe must name its list, name and detail fields", x.Type)
			}
		}
	}
	families := map[string]bool{}
	for _, f := range p.Families {
		dotted("family", f.Type)
		families[f.Type] = true
		if !f.ReDerivable && strings.TrimSpace(f.Loss) == "" {
			fail("family %q is not re-derivable and must state its loss", f.Type)
		}
		if f.ReDerivable && f.Loss != "" {
			fail("family %q is re-derivable and cannot declare a loss", f.Type)
		}
	}
	for _, sc := range p.Schemas {
		dotted("schema", sc.Name)
		if len(sc.Versions) == 0 {
			fail("schema %q must list its readable versions", sc.Name)
		}
		for i, v := range sc.Versions {
			if v < 1 || i > 0 && v <= sc.Versions[i-1] {
				fail("schema %q versions must be positive and strictly increasing", sc.Name)
				break
			}
		}
		if versionSegment.MatchString(sc.Name[strings.LastIndex(sc.Name, ".")+1:]) {
			fail("schema %q must name a base without a version suffix", sc.Name)
		}
	}
	if m.KVVersion < 0 {
		fail("kv_version must not be negative")
	}
	if ingress && len(p.Families) == 0 {
		fail("an ingress extension type must declare its event families")
	}
	if p.Webhooks && !ingress {
		fail("webhooks require an ingress extension type")
	}
	for _, r := range p.Routes {
		ident("route handler", r.Handler)
	}
	for _, k := range p.WorkKinds {
		dotted("work kind", k.Kind)
	}
	if len(p.WorkKinds) > 0 && len(p.Routes) == 0 {
		fail("work kinds are created by routes; declare a route handler")
	}
	for _, r := range p.Recurrences {
		dotted("recurrence", r)
	}
	for _, o := range p.Operations {
		dotted("operation", o.Name)
		if len(o.Guidance) > extension.MaxGuidance {
			fail("operation %q: guidance is longer than %d bytes", o.Name, extension.MaxGuidance)
		}
		switch o.Kind {
		case OperationHandler, OperationWait:
			if o.Commit {
				fail("operation %q: only emit operations commit records", o.Name)
			}
		case OperationEmit:
			if len(p.Families) == 0 {
				fail("emit operation %q needs a declared family", o.Name)
			}
		default:
			fail("operation %q kind %q must be handler, wait or emit", o.Name, o.Kind)
		}
	}
	if p.DependencyFilter && !hasKind(p.Operations, OperationWait) {
		fail("dependency_filter needs a wait operation")
	}
	for _, x := range p.Executors {
		ident("executor", x.Name)
		switch x.Capabilities.Toolbelt {
		case extension.ToolbeltNone, extension.ToolbeltCLI, extension.ToolbeltMCP, extension.ToolbeltSDK:
		default:
			fail("executor %q toolbelt %q must be none, cli, mcp or sdk", x.Name, x.Capabilities.Toolbelt)
		}
	}
	for _, r := range p.Resources {
		ident("resource provider", r.Type)
		if len(r.Modes) == 0 {
			fail("resource provider %q must list its modes", r.Type)
		}
		modes := map[string]bool{}
		for _, mode := range r.Modes {
			if mode != extension.ResourceShared && mode != extension.ResourceWork && mode != extension.ResourceFresh || modes[mode] {
				fail("resource provider %q mode %q must be a distinct shared, work or fresh", r.Type, mode)
			}
			modes[mode] = true
		}
	}
	for _, t := range p.Tasks {
		ident("task", t.Name)
		if t.IntervalSeconds < 1 {
			fail("task %q interval must be at least one second", t.Name)
		}
	}
	for _, c := range p.Checks {
		ident("check", c.Name)
	}
	if p.Setup != nil {
		ident("setup type", p.Setup.Type)
		if p.Setup.Summary == "" {
			fail("setup %q needs a summary", p.Setup.Type)
		}
	}
	if p.Playbooks != "" && (filepath.IsAbs(p.Playbooks) || strings.HasPrefix(filepath.Clean(p.Playbooks), "..")) {
		fail("playbooks %q must stay inside the extension directory", p.Playbooks)
	}
	connections := map[string]bool{}
	for _, c := range p.Connections {
		connections[c.Type] = true
	}
	for _, s := range p.ExternalStores {
		if !connections[s.Connection] {
			fail("external store %q must name a declared connection type", s.Connection)
		}
		if s.Description == "" {
			fail("external store %q needs a description", s.Connection)
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("%w: %w", ErrManifest, errors.Join(errs...))
	}
	return nil
}

func hasKind(ops []OperationDecl, kind string) bool {
	for _, o := range ops {
		if o.Kind == kind {
			return true
		}
	}
	return false
}

// Namespace reports whether ns is a KV namespace the named extension owns:
// the name itself or name + "." + suffix (the extension.Record rule).
func Namespace(name, ns string) bool {
	return ns == name || strings.HasPrefix(ns, name+".") && len(ns) > len(name)+1
}

// SourceNamespace reports whether a source namespace belongs to the named
// extension: the name itself or name + "/" + suffix (the EmitPlan rule).
func SourceNamespace(name, ns string) bool {
	return ns == name || strings.HasPrefix(ns, name+"/") && len(ns) > len(name)+1
}

// Enabled reports the catalogue methods a manifest enables, sorted: the
// lifecycle and always-available methods plus those of each declared
// surface, less those whose permission it does not declare. The host serves
// and calls nothing else for this extension.
func (m Manifest) Enabled() []string {
	p := m.Provides
	on := map[string]bool{"": true}
	set := func(key string, v bool) {
		if v {
			on[key] = true
		}
	}
	ingress := false
	for _, x := range p.Extensions {
		ingress = ingress || x.Ingress
	}
	set("extensions.ingress", ingress)
	set("webhooks", p.Webhooks)
	set("families", len(p.Families) > 0)
	set("routes", len(p.Routes) > 0)
	for _, r := range p.Routes {
		set("routes.samples", r.Samples)
	}
	set("work_kinds", len(p.WorkKinds) > 0)
	set("recurrences", len(p.Recurrences) > 0)
	set("operations", len(p.Operations) > 0)
	for _, o := range p.Operations {
		set("operations.wait", o.Kind == OperationWait)
		set("operations.emit", o.Kind == OperationEmit)
		set("operations.commit", o.Commit)
	}
	set("dependency_filter", p.DependencyFilter)
	set("executors", len(p.Executors) > 0)
	for _, x := range p.Executors {
		set("executors.resume", x.Capabilities.Resume)
		set("executors.interrupt", x.Capabilities.Interrupt)
		set("executors.inject", x.Capabilities.Inject)
		set("executors.workspace", x.Capabilities.Workspace)
		set("executors.stop", x.Controls.Stop)
		set("executors.forget", x.Controls.Forget)
		set("executors.attach", x.Controls.Attach)
	}
	set("resources", len(p.Resources) > 0)
	set("tasks", len(p.Tasks) > 0)
	set("exporters", p.Exporters)
	set("checks", len(p.Checks) > 0)
	set("setup", p.Setup != nil)
	set("annotate", p.Annotate)
	set("run_environment", p.RunEnvironment)
	set("actions", p.Actions)
	set("panels", p.Panels)
	set("render", p.Render)
	var out []string
	for _, method := range Methods {
		if on[method.Provides] && (method.Permission == "" || m.Allows(method.Permission)) {
			out = append(out, method.Name)
		}
	}
	sort.Strings(out)
	return out
}
