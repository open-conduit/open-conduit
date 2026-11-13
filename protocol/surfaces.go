package protocol

import (
	"encoding/json"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/observability"
)

// Params and results of the surfaces 07-03 adapted: validation, webhooks,
// resource providers, telemetry export, annotations and setup. Like the rest
// of version 1 they are snake_case and convert losslessly to the public Go
// contracts.

// ConnectionEntry is the connection a ConfigEntry names, of one of the
// extension's declared connection types. Secrets lists the connection's
// secret names (keys of its secret_refs); never reference text or values.
type ConnectionEntry struct {
	ID      string          `json:"id"`
	Type    string          `json:"type"`
	Config  json.RawMessage `json:"config,omitempty"`
	Secrets []string        `json:"secrets,omitempty"`
}

// Validation.

// Validation kinds name the config.Registry validator a validate call
// stands in for.
const (
	ValidateConnection = "connection"
	ValidateExtension  = "extension"
	ValidateRoute      = "route"
	ValidateExecutor   = "executor"
	ValidateResource   = "resource"
	// ValidateBinding checks the binding a route binds, for a route
	// handler declared with binding (extension.RouteBinding). Type is the
	// handler, Binding the binding; Config is empty. Its refusal's message
	// is the reason the operator reads in the binding_refused diagnostic,
	// so it names the rule and never echoes config values.
	ValidateBinding = "binding"
	// ValidateResourceMode asks a provider declared with mode_default which
	// mode a resource config names as its default. Type is the resource
	// type, Config the resource config; the answer is a ValidateResult whose
	// Mode is the mode the config itself writes, "" for none.
	ValidateResourceMode = "resource_mode"
	// ValidateResourceSandbox asks a provider declared with sandbox whether
	// a binding that runs its executor in a sandbox may use a resource
	// config. Type is the resource type, Config the resource config and
	// Mode the mode the executor requests ("" for the config's default).
	// Its refusal's message is the reason the operator reads in the
	// sandbox_workspace diagnostic, so it names the rule and never echoes
	// config values.
	ValidateResourceSandbox = "resource_sandbox"
	// ValidateExecutorMode asks an executor declared with resource_mode
	// which resource mode a binding config makes it request. Type is the
	// executor, Config the binding's executor config; the answer is a
	// ValidateResult whose Mode is the requested mode, "" for the resource
	// config's default.
	ValidateExecutorMode = "executor_mode"
	// ValidateExecutorPlaybook asks an executor declared with playbook
	// which playbook a binding config starts its turns on. Type is the
	// executor, Config the binding's executor config; the answer is a
	// ValidateResult whose Playbook is the playbook's name, "" for none.
	ValidateExecutorPlaybook = "executor_playbook"
	// ValidateExecutorFolder adapts a binding from an included folder's
	// config, for an executor declared with folder_config. Type is the
	// executor, Config the binding's executor config, Folder the included
	// folder and Resource whether the binding has a resource; the answer
	// is a ValidateResult whose Config is the adapted config the host
	// validates and stores. A refusal rejects the binding config.
	ValidateExecutorFolder = "executor_folder"
	// ValidateSandbox checks a binding's sandbox config for a provider
	// declared in sandboxes. Type is the provider, Config its config.
	ValidateSandbox = "sandbox"
	// ValidateContainer checks a binding's container config for a provider
	// declared in containers. Type is the provider, Config the whole
	// container config (extension.ContainerConfig, provider settings
	// included).
	ValidateContainer = "container"
	// ValidateTunnel checks the webhook tunnel's config
	// (settings.webhooks.tunnel.config) for a type declared in tunnels.
	// Type is the tunnel type, Config its config as the operator wrote it,
	// which the host passes to tunnel.run unchanged.
	ValidateTunnel = "tunnel"
)

// ValidateParams asks the extension to check one opaque config object of
// one of its declared types. The answer is an empty object, or a refusal
// with CodeInvalid whose message the host never echoes into diagnostics.
type ValidateParams struct {
	Kind        string          `json:"kind"`
	Type        string          `json:"type"`
	Config      json.RawMessage `json:"config,omitempty"`
	SecretNames []string        `json:"secret_names,omitempty"`
	// Binding is the binding a route binds, for kind binding only.
	Binding *RouteBinding `json:"binding,omitempty"`
	// Mode is the resource mode the binding's executor requests, for kind
	// resource_sandbox only ("" for the resource config's default).
	Mode string `json:"mode,omitempty"`
	// Folder and Resource are, for kind executor_folder only, the included
	// folder a binding comes from and whether it has a resource.
	Folder   string `json:"folder,omitempty"`
	Resource bool   `json:"resource,omitempty"`
}

// ValidateResult is the answer to validate: empty for a check, the mode
// for kinds resource_mode and executor_mode, the playbook for kind
// executor_playbook, the adapted config for kind executor_folder, and for
// kind executor the
// warnings config check shows about a valid binding config
// (extension.ConfigWarner): fixed sentences that never echo config values.
// For kinds extension and route, Describe optionally says what a valid
// object sets up, for the operator's view of the config
// (inproc.ConfigType.Describe): a schedule entry's jobs and when they run,
// or which jobs a route takes. The host keeps the lines of the config it
// validated, so its view needs no call; without them it reads the entry
// type's declared describe. They must not reveal secrets.
//
// For kind route, WorkKinds are the kinds of child Work a valid route
// config binds, when the handler binds child Work (as the bundled work
// handler does): a binding may be granted a kind (its work_kinds) only when
// an enabled route of its scope binds it. The host keeps them with the
// describe lines.
type ValidateResult struct {
	Mode      string          `json:"mode,omitempty"`
	Playbook  string          `json:"playbook,omitempty"`
	Config    json.RawMessage `json:"config,omitempty"`
	Warnings  []string        `json:"warnings,omitempty"`
	Describe  []DescribeLine  `json:"describe,omitempty"`
	WorkKinds []string        `json:"work_kinds,omitempty"`
}

// DescribeLine is one thing a config object sets up: a named element of
// an entry (Name and Detail), or a route's one line (Detail alone).
type DescribeLine struct {
	Name   string `json:"name,omitempty"`
	Detail string `json:"detail,omitempty"`
}

// RouteBinding is the binding a route binds, as its handler's binding
// check sees it (extension.RouteBinding): its executor, the toolbelt
// operations it grants, its executor config and its grants.
type RouteBinding struct {
	Executor   string          `json:"executor"`
	Operations []string        `json:"operations,omitempty"`
	Config     json.RawMessage `json:"config,omitempty"`
	Grants     []BindingGrant  `json:"grants,omitempty"`
}

// BindingGrant is one grant the binding offers (domain.BindingGrant).
type BindingGrant struct {
	Name       string          `json:"name"`
	Label      string          `json:"label,omitempty"`
	Operations []string        `json:"operations,omitempty"`
	Config     json.RawMessage `json:"config,omitempty"`
}

// Contract is the binding as extension code sees it.
func (b RouteBinding) Contract() extension.RouteBinding {
	out := extension.RouteBinding{Executor: b.Executor, Operations: b.Operations, Config: b.Config}
	for _, g := range b.Grants {
		out.Grants = append(out.Grants, domain.BindingGrant{Name: g.Name, Label: g.Label, Operations: g.Operations, Config: g.Config})
	}
	return out
}

// WireBinding is a binding as the validate call carries it.
func WireBinding(b extension.RouteBinding) *RouteBinding {
	out := &RouteBinding{Executor: b.Executor, Operations: b.Operations, Config: b.Config}
	for _, g := range b.Grants {
		out.Grants = append(out.Grants, BindingGrant{Name: g.Name, Label: g.Label, Operations: g.Operations, Config: g.Config})
	}
	return out
}

// Webhooks.

// WebhookHandleParams carries one HTTP request the host received on a path
// the extension registered at configure. The extension verifies it and
// pushes through ingress.deliver before answering, so a 2xx means durable
// acceptance.
type WebhookHandleParams struct {
	Path       string              `json:"path"`
	Method     string              `json:"method"`
	Query      string              `json:"query,omitempty"`
	Header     map[string][]string `json:"header,omitempty"`
	Body       []byte              `json:"body,omitempty"`
	ReceivedAt time.Time           `json:"received_at"`
}
type WebhookHandleResult struct {
	Status int                 `json:"status"`
	Header map[string][]string `json:"header,omitempty"`
	Body   []byte              `json:"body,omitempty"`
}

// Resource providers.

type ResourceOwner struct {
	ScopeID     string `json:"scope_id"`
	WorkID      string `json:"work_id"`
	ExecutionID string `json:"execution_id"`
}

func FromOwner(o extension.ResourceOwner) ResourceOwner {
	return ResourceOwner{ScopeID: string(o.ScopeID), WorkID: string(o.WorkID), ExecutionID: string(o.ExecutionID)}
}
func (o ResourceOwner) Extension() extension.ResourceOwner {
	return extension.ResourceOwner{ScopeID: domain.ID(o.ScopeID), WorkID: domain.ID(o.WorkID), ExecutionID: domain.ID(o.ExecutionID)}
}

type ProviderAcquireParams struct {
	Type   string        `json:"type"`
	Owner  ResourceOwner `json:"owner"`
	Config Payload       `json:"config"`
	Mode   string        `json:"mode,omitempty"`
	Params Payload       `json:"params"`
	// HostEnv is extension.ResourceAcquisition.HostEnv.
	HostEnv map[string]string `json:"host_env,omitempty"`
}
type ProviderLookupParams struct {
	Type  string        `json:"type"`
	Owner ResourceOwner `json:"owner"`
	Ref   string        `json:"ref"`
}
type ResourceLeaseReport struct {
	Ref         string    `json:"ref"`
	Mode        string    `json:"mode"`
	State       string    `json:"state"`
	Code        string    `json:"code,omitempty"`
	RetainUntil time.Time `json:"retain_until"`
}
type ResourceReport struct {
	Active    int                   `json:"active"`
	Released  int                   `json:"released"`
	Removed   int                   `json:"removed"`
	Retained  int                   `json:"retained"`
	Attention []ResourceLeaseReport `json:"attention,omitempty"`
}

func FromReport(r extension.ResourceReport) ResourceReport {
	out := ResourceReport{Active: r.Active, Released: r.Released, Removed: r.Removed, Retained: r.Retained}
	for _, a := range r.Attention {
		out.Attention = append(out.Attention, ResourceLeaseReport{Ref: a.Ref, Mode: a.Mode, State: a.State, Code: a.Code, RetainUntil: a.RetainUntil})
	}
	return out
}
func (r ResourceReport) Extension() extension.ResourceReport {
	out := extension.ResourceReport{Active: r.Active, Released: r.Released, Removed: r.Removed, Retained: r.Retained}
	for _, a := range r.Attention {
		out.Attention = append(out.Attention, extension.ResourceLeaseReport{Ref: a.Ref, Mode: a.Mode, State: a.State, Code: a.Code, RetainUntil: a.RetainUntil})
	}
	return out
}

// OwnerStatusParams asks the host for a lease owner's durable status while
// it serves provider.reconcile (the ResourceStatusFunc callback).
type OwnerStatusParams struct {
	Reconcile string        `json:"reconcile"`
	Owner     ResourceOwner `json:"owner"`
}
type OwnerStatus struct {
	Known              bool `json:"known"`
	ExecutionDone      bool `json:"execution_done"`
	ExecutionSucceeded bool `json:"execution_succeeded"`
	WorkDone           bool `json:"work_done"`
	WorkSucceeded      bool `json:"work_succeeded"`
	// Mounts are the host paths live containers hold, so a sweep keeps a
	// resource beneath or holding one (extension.ResourceOwnerStatus).
	Mounts []string `json:"mounts,omitempty"`
}

func FromOwnerStatus(s extension.ResourceOwnerStatus) OwnerStatus {
	return OwnerStatus{Known: s.Known, ExecutionDone: s.ExecutionDone, ExecutionSucceeded: s.ExecutionSucceeded, WorkDone: s.WorkDone, WorkSucceeded: s.WorkSucceeded, Mounts: s.Mounts}
}
func (s OwnerStatus) Extension() extension.ResourceOwnerStatus {
	return extension.ResourceOwnerStatus{Known: s.Known, ExecutionDone: s.ExecutionDone, ExecutionSucceeded: s.ExecutionSucceeded, WorkDone: s.WorkDone, WorkSucceeded: s.WorkSucceeded, Mounts: s.Mounts}
}

// ProviderReconcileParams carries the reconcile ID the extension quotes in
// provider.owner_status while the pass runs.
type ProviderReconcileParams struct {
	Type      string `json:"type"`
	Reconcile string `json:"reconcile"`
}

// ResourceSpare is one spare resource a sweep found
// (extension.ResourceSpare).
type ResourceSpare struct {
	Ref   string `json:"ref"`
	Mode  string `json:"mode,omitempty"`
	Spare string `json:"spare"`
	Kept  string `json:"kept,omitempty"`
}

// ResourceSweepReport is what provider.sweep found and did
// (extension.ResourceSweepReport). provider.sweep's params are a
// ProviderReconcileParams: the sweep quotes its ID in provider.owner_status.
type ResourceSweepReport struct {
	Removed []ResourceSpare `json:"removed,omitempty"`
	Kept    []ResourceSpare `json:"kept,omitempty"`
}

func FromSweepReport(r extension.ResourceSweepReport) ResourceSweepReport {
	conv := func(in []extension.ResourceSpare) []ResourceSpare {
		var out []ResourceSpare
		for _, s := range in {
			out = append(out, ResourceSpare{Ref: s.Ref, Mode: s.Mode, Spare: s.Spare, Kept: s.Kept})
		}
		return out
	}
	return ResourceSweepReport{Removed: conv(r.Removed), Kept: conv(r.Kept)}
}
func (r ResourceSweepReport) Extension() extension.ResourceSweepReport {
	conv := func(in []ResourceSpare) []extension.ResourceSpare {
		var out []extension.ResourceSpare
		for _, s := range in {
			out = append(out, extension.ResourceSpare{Ref: s.Ref, Mode: s.Mode, Spare: s.Spare, Kept: s.Kept})
		}
		return out
	}
	return extension.ResourceSweepReport{Removed: conv(r.Removed), Kept: conv(r.Kept)}
}

// Telemetry.

type Correlation struct {
	WorkID          string `json:"work_id,omitempty"`
	EventID         string `json:"event_id,omitempty"`
	ExecutionID     string `json:"execution_id,omitempty"`
	ScopeID         string `json:"scope_id,omitempty"`
	ExecutorBinding string `json:"executor_binding,omitempty"`
	TraceID         string `json:"trace_id,omitempty"`
}
type TelemetryRecord struct {
	Name        string            `json:"name"`
	At          time.Time         `json:"at"`
	Correlation Correlation       `json:"correlation"`
	From        string            `json:"from,omitempty"`
	To          string            `json:"to,omitempty"`
	Attributes  map[string]string `json:"attributes,omitempty"`
	Usage       *Usage            `json:"usage,omitempty"`
}

func FromRecords(rs []observability.Record) []TelemetryRecord {
	out := make([]TelemetryRecord, len(rs))
	for i, r := range rs {
		c := r.Correlation
		out[i] = TelemetryRecord{Name: r.Name, At: r.At, From: r.From, To: r.To, Attributes: r.Attributes, Correlation: Correlation{WorkID: string(c.WorkID), EventID: string(c.EventID), ExecutionID: string(c.ExecutionID), ScopeID: string(c.ScopeID), ExecutorBinding: c.ExecutorBinding, TraceID: c.TraceID}}
		if u := r.Usage; u != nil {
			out[i].Usage = &Usage{InputTokens: u.InputTokens, OutputTokens: u.OutputTokens, CachedTokens: u.CachedTokens, Cost: u.Cost, Currency: u.Currency}
		}
	}
	return out
}
func ToRecords(rs []TelemetryRecord) []observability.Record {
	out := make([]observability.Record, len(rs))
	for i, r := range rs {
		c := r.Correlation
		out[i] = observability.Record{Name: r.Name, At: r.At, From: r.From, To: r.To, Attributes: r.Attributes, Correlation: observability.Correlation{WorkID: domain.ID(c.WorkID), EventID: domain.ID(c.EventID), ExecutionID: domain.ID(c.ExecutionID), ScopeID: domain.ID(c.ScopeID), ExecutorBinding: c.ExecutorBinding, TraceID: c.TraceID}}
		if u := r.Usage; u != nil {
			out[i].Usage = &observability.Measurement{InputTokens: u.InputTokens, OutputTokens: u.OutputTokens, CachedTokens: u.CachedTokens, Cost: u.Cost, Currency: u.Currency}
		}
	}
	return out
}

type TelemetryExportParams struct {
	Records []TelemetryRecord `json:"records"`
}

// TelemetryEvent is one named telemetry event of the host's telemetry
// events log: telemetry.capture writes one (permission telemetry.capture),
// and telemetry.sink hands one to an extension (permission telemetry.sink).
// ScopeID is the scope it belongs to, "" for one about the machine;
// Properties never carry it.
type TelemetryEvent struct {
	ScopeID    string         `json:"scope_id,omitempty"`
	Name       string         `json:"name"`
	At         time.Time      `json:"at"`
	Properties map[string]any `json:"properties,omitempty"`
}

// Setup probes (permission probes): the read-only probes the host, its
// extensions and its executors offer setup sessions, by full name
// ("claude-code.status").

type ProbeArg struct {
	Name     string `json:"name"`
	Summary  string `json:"summary,omitempty"`
	Required bool   `json:"required,omitempty"`
}
type ProbeInfo struct {
	Name    string     `json:"name"`
	Summary string     `json:"summary"`
	Args    []ProbeArg `json:"args,omitempty"`
}
type ProbeListResult struct {
	Probes []ProbeInfo `json:"probes"`
}
type ProbeRunParams struct {
	Name string            `json:"name"`
	Args map[string]string `json:"args,omitempty"`
}

// ProbeRunResult is a probe's output and the command it stands for. A probe
// that ran and failed answers Error, its fixed, secret-free reason, instead
// of a protocol error.
type ProbeRunResult struct {
	Command string          `json:"command,omitempty"`
	Output  json.RawMessage `json:"output,omitempty"`
	Error   string          `json:"error,omitempty"`
}

// The host's config service (permission config.read). The host keeps
// config text behind handles and answers only copies with every
// secret_refs value "[redacted]"; an extension builds candidates by editing
// a handle. Nothing here applies a configuration.

// ConfigBaseResult is the configuration now: its generation and hash, a
// handle on its file text, the text redacted, and the scopes from included
// folders, whose text is in other files.
type ConfigBaseResult struct {
	Generation string   `json:"generation"`
	Hash       string   `json:"hash"`
	Handle     string   `json:"handle"`
	Text       string   `json:"text"`
	Folders    []string `json:"folders,omitempty"`
}

// ConfigEditParams is one edit to the text behind Handle: op add, replace
// or remove at a JSON pointer, as in RFC 6902. add adds or replaces an
// object member and inserts into an array before a numeric index
// (appending at the length or "-"), refusing an element named by id or
// name since one is there; replace sets a value that exists and remove
// removes one.
type ConfigEditParams struct {
	Handle  string          `json:"handle"`
	Op      string          `json:"op"`
	Pointer string          `json:"pointer"`
	Value   json.RawMessage `json:"value,omitempty"`
}

// ConfigEditResult is the candidate the edit built: its handle, its text
// redacted and the value the edit replaced (none for an insert), redacted.
type ConfigEditResult struct {
	Handle string          `json:"handle"`
	Text   string          `json:"text"`
	Before json.RawMessage `json:"before,omitempty"`
}

type ConfigHandleParams struct {
	Handle string `json:"handle"`
}

// ConfigDiagnostic is one validation problem, as `config validate` prints
// it.
type ConfigDiagnostic struct {
	Path    string `json:"path"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// ConfigValidateResult is the candidate's hash when it is valid, its
// diagnostics when not.
type ConfigValidateResult struct {
	Hash        string             `json:"hash,omitempty"`
	Diagnostics []ConfigDiagnostic `json:"diagnostics,omitempty"`
}

// ConfigTraceParams traces one of a scope's named samples against the
// candidate behind Handle; with no sample it lists the scope's samples.
type ConfigTraceParams struct {
	Handle string `json:"handle"`
	Scope  string `json:"scope"`
	Sample string `json:"sample,omitempty"`
}

// ConfigTraceResult is what routing would do with the sample, route by
// route. A trace that can't run answers Error, a fixed reason, with the
// scope's samples when it has them.
type ConfigTraceResult struct {
	Samples []ConfigTraceSample `json:"samples,omitempty"`
	Sample  string              `json:"sample,omitempty"`
	Routes  []ConfigTraceRoute  `json:"routes,omitempty"`
	Effect  ConfigTraceEffect   `json:"effect,omitzero"`
	Error   string              `json:"error,omitempty"`
}

type ConfigTraceSample struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

type ConfigTraceRoute struct {
	Route   string `json:"route"`
	Handler string `json:"handler"`
	Binding string `json:"binding,omitempty"`
	Verdict string `json:"verdict"`
	Reason  string `json:"reason,omitempty"`
}

type ConfigTraceEffect struct {
	Kind    string `json:"kind,omitempty"`
	WorkID  string `json:"work_id,omitempty"`
	Binding string `json:"binding,omitempty"`
	Detail  string `json:"detail,omitempty"`
}

// Run environment.

// RunEnvironmentParams asks for the extension's environment layer for one
// run of a Work in a scope that uses it.
type RunEnvironmentParams struct {
	Owner ResourceOwner `json:"owner"`
	// Connection is the connection the run's binding names, or empty.
	Connection string `json:"connection,omitempty"`
}
type RunEnvironmentResult struct {
	Env map[string]string `json:"env,omitempty"`
}

// Annotations.

type WorkAnnotateParams struct {
	Work Work `json:"work"`
}
type Annotation struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}
type WorkAnnotateResult struct {
	Annotations []Annotation `json:"annotations,omitempty"`
}

// Actions (work.actions, work.act): an extension lists the actions
// available on a target now and authorises an invocation, returning the one
// Event the host accepts under core.action (extension.ActionProvider).

type ActionTarget struct {
	Scope Scope `json:"scope"`
	Work  *Work `json:"work,omitempty"`
}

func FromActionTarget(t extension.ActionTarget) ActionTarget {
	out := ActionTarget{Scope: FromScope(t.Scope)}
	if t.Work != nil {
		w := FromWork(*t.Work)
		out.Work = &w
	}
	return out
}
func (t ActionTarget) Extension() extension.ActionTarget {
	out := extension.ActionTarget{Scope: t.Scope.Domain()}
	if t.Work != nil {
		w := t.Work.Domain()
		out.Work = &w
	}
	return out
}

type Action struct {
	ID      string   `json:"id"`
	Label   string   `json:"label"`
	Prompt  string   `json:"prompt,omitempty"`
	Choices []string `json:"choices,omitempty"`
	Detail  string   `json:"detail,omitempty"`
	// Awaited: the Work waits for a person to take this action
	// (extension.Action.Awaited).
	Awaited bool `json:"awaited,omitempty"`
}
type WorkActionsParams struct {
	Target ActionTarget `json:"target"`
}
type WorkActionsResult struct {
	Actions []Action `json:"actions,omitempty"`
}
type Caller struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}
type WorkActParams struct {
	Target ActionTarget `json:"target"`
	Action string       `json:"action"`
	Input  string       `json:"input,omitempty"`
	Caller Caller       `json:"caller"`
}

// WorkActResult is an authorised invocation: Event carries only type,
// correlation key and data; the host assigns everything else.
type WorkActResult struct {
	Event   Event  `json:"event"`
	Message string `json:"message,omitempty"`
}

// Panels (view.render): an extension that offers actions shows panels on
// the target, a constrained document core renders in every operator view
// (extension.PanelProvider).

type ViewRenderParams struct {
	Target ActionTarget `json:"target"`
}
type ViewRenderResult struct {
	Panels []Panel `json:"panels,omitempty"`
}
type Panel struct {
	ID       string         `json:"id"`
	Title    string         `json:"title"`
	Sections []PanelSection `json:"sections"`
}
type PanelSection struct {
	Title    string         `json:"title,omitempty"`
	Text     string         `json:"text,omitempty"`
	Fields   []PanelField   `json:"fields,omitempty"`
	Table    *PanelTable    `json:"table,omitempty"`
	Messages []PanelMessage `json:"messages,omitempty"`
	Links    []PanelLink    `json:"links,omitempty"`
	Actions  []string       `json:"actions,omitempty"`
}
type PanelMessage struct {
	From    string    `json:"from"`
	Label   string    `json:"label,omitempty"`
	Text    string    `json:"text"`
	At      time.Time `json:"at,omitzero"`
	Choices []string  `json:"choices,omitempty"`
	Chosen  string    `json:"chosen,omitempty"`
}
type PanelField struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}
type PanelTable struct {
	Columns []string   `json:"columns"`
	Rows    [][]string `json:"rows,omitempty"`
}
type PanelLink struct {
	Label string `json:"label"`
	URL   string `json:"url"`
}

func FromPanels(ps []extension.Panel) []Panel {
	out := make([]Panel, 0, len(ps))
	for _, p := range ps {
		w := Panel{ID: p.ID, Title: p.Title, Sections: make([]PanelSection, 0, len(p.Sections))}
		for _, s := range p.Sections {
			ws := PanelSection{Title: s.Title, Text: s.Text, Actions: s.Actions}
			for _, f := range s.Fields {
				ws.Fields = append(ws.Fields, PanelField{Name: f.Name, Value: f.Value})
			}
			if s.Table != nil {
				ws.Table = &PanelTable{Columns: s.Table.Columns, Rows: s.Table.Rows}
			}
			for _, m := range s.Messages {
				ws.Messages = append(ws.Messages, PanelMessage(m))
			}
			for _, l := range s.Links {
				ws.Links = append(ws.Links, PanelLink{Label: l.Label, URL: l.URL})
			}
			w.Sections = append(w.Sections, ws)
		}
		out = append(out, w)
	}
	return out
}

func ToPanels(ps []Panel) []extension.Panel {
	out := make([]extension.Panel, 0, len(ps))
	for _, p := range ps {
		x := extension.Panel{ID: p.ID, Title: p.Title, Sections: make([]extension.PanelSection, 0, len(p.Sections))}
		for _, s := range p.Sections {
			xs := extension.PanelSection{Title: s.Title, Text: s.Text, Actions: s.Actions}
			for _, f := range s.Fields {
				xs.Fields = append(xs.Fields, extension.PanelField{Name: f.Name, Value: f.Value})
			}
			if s.Table != nil {
				xs.Table = &extension.PanelTable{Columns: s.Table.Columns, Rows: s.Table.Rows}
			}
			for _, m := range s.Messages {
				xs.Messages = append(xs.Messages, extension.PanelMessage(m))
			}
			for _, l := range s.Links {
				xs.Links = append(xs.Links, extension.PanelLink{Label: l.Label, URL: l.URL})
			}
			x.Sections = append(x.Sections, xs)
		}
		out = append(out, x)
	}
	return out
}

// Config candidates (config.candidate): an extension whose Work proposes
// configuration says what it proposes, for core's review and the
// operator's apply (extension.ConfigCandidate). Handle is one the host
// gave it (config.base, config.edit); the host serves the text behind it
// to the operator alone.

type ConfigCandidateParams struct {
	Work Work `json:"work"`
}
type ConfigCandidateResult struct {
	// Found is false for a Work with nothing proposed.
	Found          bool              `json:"found"`
	Handle         string            `json:"handle,omitempty"`
	BaseGeneration string            `json:"base_generation,omitempty"`
	Hash           string            `json:"hash,omitempty"`
	Changes        []CandidateChange `json:"changes,omitempty"`
	ApproveAll     string            `json:"approve_all,omitempty"`
	Validate       string            `json:"validate,omitempty"`
}
type CandidateChange struct {
	ID       string          `json:"id"`
	Op       string          `json:"op"`
	Pointer  string          `json:"pointer"`
	State    string          `json:"state"`
	Why      string          `json:"why,omitempty"`
	Before   json.RawMessage `json:"before,omitempty"`
	After    json.RawMessage `json:"after,omitempty"`
	Problems []string        `json:"problems,omitempty"`
	Approve  string          `json:"approve,omitempty"`
	Reject   string          `json:"reject,omitempty"`
}

// FromCandidate is c on the wire.
func FromCandidate(c extension.Candidate) ConfigCandidateResult {
	out := ConfigCandidateResult{Found: true, Handle: c.Handle, BaseGeneration: c.BaseGeneration, Hash: c.Hash, ApproveAll: c.ApproveAll, Validate: c.Validate}
	for _, x := range c.Changes {
		out.Changes = append(out.Changes, CandidateChange(x))
	}
	return out
}

// Candidate is the candidate the result carries.
func (r ConfigCandidateResult) Candidate() extension.Candidate {
	out := extension.Candidate{Handle: r.Handle, BaseGeneration: r.BaseGeneration, Hash: r.Hash, ApproveAll: r.ApproveAll, Validate: r.Validate}
	for _, x := range r.Changes {
		out.Changes = append(out.Changes, extension.CandidateChange(x))
	}
	return out
}

// Setup: setup.run is called at CLI time; the extension drives the operator
// through the setup.* callbacks while it runs, all quoting the run ID.

type SetupRunParams struct {
	Run      string   `json:"run"`
	Type     string   `json:"type"`
	Args     []string `json:"args,omitempty"`
	Config   []byte   `json:"config,omitempty"`
	Cwd      string   `json:"cwd"`
	StateDir string   `json:"state_dir"`
	DryRun   bool     `json:"dry_run,omitempty"`
}
type SetupRunResult struct {
	Connections []json.RawMessage `json:"connections,omitempty"`
	Scope       json.RawMessage   `json:"scope,omitempty"`
	Summary     []string          `json:"summary,omitempty"`
	Next        []string          `json:"next,omitempty"`
}
type SetupPrintParams struct {
	Run  string `json:"run"`
	Text string `json:"text"`
}
type SetupPromptParams struct {
	Run      string `json:"run"`
	Question string `json:"question"`
	Default  string `json:"default,omitempty"`
	Secret   bool   `json:"secret,omitempty"`
}
type SetupAnswer struct {
	Value string `json:"value,omitempty"`
	Index int    `json:"index,omitempty"`
	Found bool   `json:"found,omitempty"`
}
type SetupChooseParams struct {
	Run      string   `json:"run"`
	Question string   `json:"question"`
	Options  []string `json:"options"`
}
type SetupURLParams struct {
	Run string `json:"run"`
	URL string `json:"url"`
}
type SetupSecretParams struct {
	Run   string `json:"run"`
	Name  string `json:"name"`
	Value string `json:"value"`
}
type SetupLookPathParams struct {
	Run  string `json:"run"`
	Name string `json:"name"`
}
type SetupCommandParams struct {
	Run  string   `json:"run"`
	Dir  string   `json:"dir"`
	Name string   `json:"name"`
	Args []string `json:"args,omitempty"`
}

// Sandbox and container brokers (#428): an executor package launches a
// run in its binding's sandbox or container through the host, which holds
// the provider bound to the attempt. Each call names the run.

type SandboxNeeds struct {
	Read     []string `json:"read,omitempty"`
	Write    []string `json:"write,omitempty"`
	ReadOnly []string `json:"read_only,omitempty"`
	Deny     []string `json:"deny,omitempty"`
	Hosts    []string `json:"hosts,omitempty"`
	Sockets  []string `json:"sockets,omitempty"`
}

func FromSandboxNeeds(n extension.SandboxNeeds) SandboxNeeds {
	return SandboxNeeds{Read: n.Read, Write: n.Write, ReadOnly: n.ReadOnly, Deny: n.Deny, Hosts: n.Hosts, Sockets: n.Sockets}
}
func (n SandboxNeeds) Extension() extension.SandboxNeeds {
	return extension.SandboxNeeds{Read: n.Read, Write: n.Write, ReadOnly: n.ReadOnly, Deny: n.Deny, Hosts: n.Hosts, Sockets: n.Sockets}
}

type SandboxLaunch struct {
	Argv       []string `json:"argv"`
	Dir        string   `json:"dir,omitempty"`
	Env        []string `json:"env,omitempty"`
	WrapperEnv []string `json:"wrapper_env,omitempty"`
	RunDir     string   `json:"run_dir,omitempty"`
	Secrets    []string `json:"secrets,omitempty"`
	TTY        bool     `json:"tty,omitempty"`
}

func FromSandboxLaunch(l extension.SandboxLaunch) SandboxLaunch {
	return SandboxLaunch{Argv: l.Argv, Dir: l.Dir, Env: l.Env, WrapperEnv: l.WrapperEnv, RunDir: l.RunDir, Secrets: l.Secrets, TTY: l.TTY}
}
func (l SandboxLaunch) Extension() extension.SandboxLaunch {
	return extension.SandboxLaunch{Argv: l.Argv, Dir: l.Dir, Env: l.Env, WrapperEnv: l.WrapperEnv, RunDir: l.RunDir, Secrets: l.Secrets, TTY: l.TTY}
}

type SandboxPlan struct {
	Argv    []string `json:"argv"`
	Env     []string `json:"env,omitempty"`
	Digest  string   `json:"digest,omitempty"`
	Signals string   `json:"signals,omitempty"`
	Summary string   `json:"summary,omitempty"`
	Data    Payload  `json:"data,omitempty"`
}

func FromSandboxPlan(p extension.SandboxPlan) SandboxPlan {
	return SandboxPlan{Argv: p.Argv, Env: p.Env, Digest: p.Digest, Signals: p.Signals, Summary: p.Summary, Data: FromPayload(p.Data)}
}
func (p SandboxPlan) Extension() extension.SandboxPlan {
	return extension.SandboxPlan{Argv: p.Argv, Env: p.Env, Digest: p.Digest, Signals: p.Signals, Summary: p.Summary, Data: p.Data.Domain()}
}

// SandboxPaths are one attempt's host-set paths (extension.SandboxPaths).
type SandboxPaths struct {
	Workspace string `json:"workspace,omitempty"`
	Run       string `json:"run,omitempty"`
	Toolbelt  string `json:"toolbelt,omitempty"`
	Records   string `json:"records,omitempty"`
	Home      string `json:"home,omitempty"`
}

func FromSandboxPaths(p extension.SandboxPaths) SandboxPaths {
	return SandboxPaths{Workspace: p.Workspace, Run: p.Run, Toolbelt: p.Toolbelt, Records: p.Records, Home: p.Home}
}
func (p SandboxPaths) Extension() extension.SandboxPaths {
	return extension.SandboxPaths{Workspace: p.Workspace, Run: p.Run, Toolbelt: p.Toolbelt, Records: p.Records, Home: p.Home}
}

// SandboxProviderPlanParams asks a sandbox provider to wrap one launch
// (sandbox_provider.plan; extension.SandboxProvider.Plan). The host has
// already merged the resource's needs and its protected set into Needs, and
// computes the plan's digest itself from what comes back.
type SandboxProviderPlanParams struct {
	Type   string        `json:"type"`
	Owner  ResourceOwner `json:"owner"`
	Config Payload       `json:"config"`
	Paths  SandboxPaths  `json:"paths"`
	Needs  SandboxNeeds  `json:"needs"`
	Launch SandboxLaunch `json:"launch"`
}

// SandboxProviderAdoptParams asks a sandbox provider to prove a recovered
// segment still runs inside its plan (sandbox_provider.adopt;
// extension.SandboxProvider.Adopt). The answer is an empty object, or a
// refusal.
type SandboxProviderAdoptParams struct {
	Type   string      `json:"type"`
	Plan   SandboxPlan `json:"plan"`
	Parent int         `json:"parent"`
	Root   int         `json:"root"`
}

// TunnelRunParams starts one run of a tunnel (tunnel.run;
// extension.TunnelProvider.Run). The host holds the call open for as long
// as it wants the tunnel and cancels it ($/cancel) to stop it; the answer,
// an empty object, comes only once the tunnel has stopped. A refusal coded
// invalid is extension.ErrTunnelConfig: the host doesn't start that config
// again. Run is the host's ID for this call, which tunnel.url quotes.
type TunnelRunParams struct {
	Type   string  `json:"type"`
	Run    string  `json:"run"`
	Addr   string  `json:"addr"`
	Config Payload `json:"config"`
}

// TunnelURLParams reports a running tunnel's public URL (tunnel.url), ""
// while it is down. It is a request, not a notification, so the host takes
// the URLs in the order the package sent them; a run the host no longer
// holds open is refused with conflict.
type TunnelURLParams struct {
	Run string `json:"run"`
	URL string `json:"url"`
}

// LaunchParams plans one launch (sandbox.plan) or creates its container
// (container.launch).
type LaunchParams struct {
	RunID  string        `json:"run_id"`
	Needs  SandboxNeeds  `json:"needs"`
	Launch SandboxLaunch `json:"launch"`
}

// SandboxAdoptParams proves a recovered segment's wrapper, pid Parent,
// still runs in its sandbox.
type SandboxAdoptParams struct {
	RunID  string `json:"run_id"`
	Parent int    `json:"parent"`
}

// ContainerAdoptParams proves a recovered segment's container is the one
// pinned; Alive says its wrapper still runs.
type ContainerAdoptParams struct {
	RunID string `json:"run_id"`
	Alive bool   `json:"alive"`
}
type ContainerExit struct {
	Exited      bool `json:"exited,omitempty"`
	ExitCode    int  `json:"exit_code,omitempty"`
	Interrupted bool `json:"interrupted,omitempty"`
}

// ContainerRef identifies a container (extension.ContainerRef).
type ContainerRef struct {
	ID     string            `json:"id"`
	Labels map[string]string `json:"labels,omitempty"`
}

func FromContainerRef(r extension.ContainerRef) ContainerRef {
	return ContainerRef{ID: r.ID, Labels: r.Labels}
}
func (r ContainerRef) Extension() extension.ContainerRef {
	return extension.ContainerRef{ID: r.ID, Labels: r.Labels}
}

// ContainerState is a provider's view of one container
// (extension.ContainerState).
type ContainerState struct {
	Ref       ContainerRef `json:"ref"`
	Daemon    string       `json:"daemon,omitempty"`
	Image     string       `json:"image,omitempty"`
	Running   bool         `json:"running,omitempty"`
	Gone      bool         `json:"gone,omitempty"`
	ExitCode  int          `json:"exit_code,omitempty"`
	StartedAt time.Time    `json:"started_at,omitzero"`
	Mounts    []string     `json:"mounts,omitempty"`
}

func FromContainerState(s extension.ContainerState) ContainerState {
	return ContainerState{Ref: FromContainerRef(s.Ref), Daemon: s.Daemon, Image: s.Image, Running: s.Running, Gone: s.Gone, ExitCode: s.ExitCode, StartedAt: s.StartedAt, Mounts: s.Mounts}
}
func (s ContainerState) Extension() extension.ContainerState {
	return extension.ContainerState{Ref: s.Ref.Extension(), Daemon: s.Daemon, Image: s.Image, Running: s.Running, Gone: s.Gone, ExitCode: s.ExitCode, StartedAt: s.StartedAt, Mounts: s.Mounts}
}

// ContainerPlan is what a provider will create (extension.ContainerPlan).
// The host computes Digest itself from Image and Data.
type ContainerPlan struct {
	Image   string  `json:"image"`
	Digest  string  `json:"digest,omitempty"`
	Summary string  `json:"summary,omitempty"`
	Data    Payload `json:"data"`
}

func FromContainerPlan(p extension.ContainerPlan) ContainerPlan {
	return ContainerPlan{Image: p.Image, Digest: p.Digest, Summary: p.Summary, Data: FromPayload(p.Data)}
}
func (p ContainerPlan) Extension() extension.ContainerPlan {
	return extension.ContainerPlan{Image: p.Image, Digest: p.Digest, Summary: p.Summary, Data: p.Data.Domain()}
}

// ContainerCreated is a container created and not started
// (extension.ContainerCreated).
type ContainerCreated struct {
	Ref    ContainerRef `json:"ref"`
	Daemon string       `json:"daemon,omitempty"`
	Argv   []string     `json:"argv"`
	Env    []string     `json:"env,omitempty"`
}

func FromContainerCreated(c extension.ContainerCreated) ContainerCreated {
	return ContainerCreated{Ref: FromContainerRef(c.Ref), Daemon: c.Daemon, Argv: c.Argv, Env: c.Env}
}
func (c ContainerCreated) Extension() extension.ContainerCreated {
	return extension.ContainerCreated{Ref: c.Ref.Extension(), Daemon: c.Daemon, Argv: c.Argv, Env: c.Env}
}

// ContainerProviderPlanParams asks a container provider to plan one launch
// (container_provider.plan; extension.ContainerProvider.Plan). The host has
// already merged the resource's needs and its protected set into Needs,
// and computes the plan's digest itself from what comes back.
type ContainerProviderPlanParams struct {
	Type   string        `json:"type"`
	Owner  ResourceOwner `json:"owner"`
	Config Payload       `json:"config"`
	Paths  SandboxPaths  `json:"paths"`
	Needs  SandboxNeeds  `json:"needs"`
	Launch SandboxLaunch `json:"launch"`
}

// ContainerProviderCreateParams asks a provider to create a plan's
// container, not started (container_provider.create).
type ContainerProviderCreateParams struct {
	Type   string            `json:"type"`
	Plan   ContainerPlan     `json:"plan"`
	Labels map[string]string `json:"labels"`
}

// ContainerProviderRefParams names one container: signal (Signal), stop
// (GraceMillis), inspect and remove.
type ContainerProviderRefParams struct {
	Type        string       `json:"type"`
	Ref         ContainerRef `json:"ref"`
	Signal      string       `json:"signal,omitempty"`
	GraceMillis int64        `json:"grace_millis,omitempty"`
}

// ContainerProviderListParams lists the containers carrying an owner's
// labels; an owner with no execution_id lists every container of the host
// (container_provider.list). The answer is a ContainerList; a listing that
// fails is an error, never an empty list.
type ContainerProviderListParams struct {
	Type  string        `json:"type"`
	Owner ResourceOwner `json:"owner"`
}
type ContainerList struct {
	Containers []ContainerState `json:"containers"`
}

// ContainerProviderForwardParams asks a provider declared with forward for
// the address the host serves an execution's toolbelt on for a container
// config, before each launch and recovery (container_provider.forward). An
// empty Address means its containers reach the toolbelt's socket itself.
type ContainerProviderForwardParams struct {
	Type   string  `json:"type"`
	Config Payload `json:"config"`
}
type ContainerForward struct {
	Address string `json:"address,omitempty"`
}
