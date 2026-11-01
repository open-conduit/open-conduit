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
}

func FromOwnerStatus(s extension.ResourceOwnerStatus) OwnerStatus {
	return OwnerStatus{Known: s.Known, ExecutionDone: s.ExecutionDone, ExecutionSucceeded: s.ExecutionSucceeded, WorkDone: s.WorkDone, WorkSucceeded: s.WorkSucceeded}
}
func (s OwnerStatus) Extension() extension.ResourceOwnerStatus {
	return extension.ResourceOwnerStatus{Known: s.Known, ExecutionDone: s.ExecutionDone, ExecutionSucceeded: s.ExecutionSucceeded, WorkDone: s.WorkDone, WorkSucceeded: s.WorkSucceeded}
}

// ProviderReconcileParams carries the reconcile ID the extension quotes in
// provider.owner_status while the pass runs.
type ProviderReconcileParams struct {
	Type      string `json:"type"`
	Reconcile string `json:"reconcile"`
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

// ConfigEditParams is one `config set`/`config unset` edit to the text
// behind Handle: op add, replace or remove at a JSON pointer.
type ConfigEditParams struct {
	Handle  string          `json:"handle"`
	Op      string          `json:"op"`
	Pointer string          `json:"pointer"`
	Value   json.RawMessage `json:"value,omitempty"`
}

// ConfigEditResult is the candidate the edit built: its handle, its text
// redacted and the value that was at the pointer, redacted.
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
