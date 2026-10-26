package protocol

import (
	"encoding/json"
	"time"

	"github.com/alsiola/open-conduit/domain"
	"github.com/alsiola/open-conduit/extension"
	"github.com/alsiola/open-conduit/observability"
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
)

// ValidateParams asks the extension to check one opaque config object of
// one of its declared types. The answer is an empty object, or a refusal
// with CodeInvalid whose message the host never echoes into diagnostics.
type ValidateParams struct {
	Kind        string          `json:"kind"`
	Type        string          `json:"type"`
	Config      json.RawMessage `json:"config,omitempty"`
	SecretNames []string        `json:"secret_names,omitempty"`
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
