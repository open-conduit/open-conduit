package protocol

import (
	"encoding/json"
	"time"
)

// Params and results of the version 1 methods (07-02). Each type is named
// after its method; a method absent here takes an empty object and returns
// an empty object.

// Lifecycle.

type HostInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}
type InitializeParams struct {
	ProtocolVersions []int    `json:"protocol_versions"`
	Host             HostInfo `json:"host"`
	// StateDir is the extension's private state directory.
	StateDir string `json:"state_dir"`
}
type InitializeResult struct {
	ProtocolVersion int `json:"protocol_version"`
	// Manifest must equal the manifest file the host discovered.
	Manifest json.RawMessage `json:"manifest"`
}

// ConfigEntry is one configured scope extension entry of the extension's
// own types. Config is opaque; secret values never appear here.
type ConfigEntry struct {
	ScopeID string          `json:"scope_id"`
	Type    string          `json:"type"`
	Config  json.RawMessage `json:"config,omitempty"`
	// Secrets lists the secret names the entry may resolve.
	Secrets []string `json:"secrets,omitempty"`
	// Name is the entry's name in its scope (07-03).
	Name string `json:"name,omitempty"`
	// Connection is the connection the entry names, when it names one of
	// the extension's declared connection types (07-03).
	Connection *ConnectionEntry `json:"connection,omitempty"`
	// BindingConnections are the connections the bindings in the entry's
	// scope name, when they are of one of the extension's declared
	// connection types and the scope authorization grants them. An
	// operation acts as one when its authority's connection names it.
	BindingConnections []ConnectionEntry `json:"binding_connections,omitempty"`
}

// KVState tells the extension which KV layout the host has recorded for it
// (Stored) and which its manifest declares (Declared). Stored < Declared asks
// the extension to migrate its own namespaces before answering configure.
type KVState struct {
	Stored   int `json:"stored"`
	Declared int `json:"declared"`
}
type ConfigureParams struct {
	Generation string        `json:"generation"`
	Entries    []ConfigEntry `json:"entries,omitempty"`
	PublicURL  string        `json:"public_url,omitempty"`
	DryRun     bool          `json:"dry_run,omitempty"`
	KV         KVState       `json:"kv"`
}

// SourceDecl is an ingress source the extension registers for an entry.
// Name is the registration name the host uses in ingress.push and
// ingress.reconcile and the extension uses in ingress.deliver; it must be
// the extension name or begin with name + "/".
type SourceDecl struct {
	Name            string `json:"name"`
	Source          Source `json:"source"`
	IntervalSeconds int    `json:"interval_seconds"`
	// Families are the ingress families of this source (a subset of the
	// manifest's families).
	Families []EventFamily `json:"families"`
}
type ConfigureResult struct {
	Sources      []SourceDecl `json:"sources,omitempty"`
	WebhookPaths []string     `json:"webhook_paths,omitempty"`
	// KVVersion is the KV layout the extension now holds; the host records
	// it once configure succeeds.
	KVVersion int `json:"kv_version"`
}

// Ingress.

type IngressPushParams struct {
	Name     string   `json:"name"`
	Source   Source   `json:"source"`
	Delivery Delivery `json:"delivery"`
}
type IngressReconcileParams struct {
	Name string `json:"name"`
	Scan Scan   `json:"scan"`
}
type IngressDeliverParams struct {
	Name     string   `json:"name"`
	Delivery Delivery `json:"delivery"`
}
type IngressDeliverResult struct {
	Accepted []AcceptedEvent `json:"accepted"`
}

// Routing and in-transaction callbacks.

type RouteMatchParams struct {
	Handler string     `json:"handler"`
	Input   RouteInput `json:"input"`
}

// RouteSamplesParams asks a handler for the sample Events it offers for a
// route of a scope, written as of Now, the host's clock.
type RouteSamplesParams struct {
	Handler string    `json:"handler"`
	Scope   Scope     `json:"scope"`
	Route   Route     `json:"route"`
	Now     time.Time `json:"now"`
}
type RouteSamplesResult struct {
	Samples []RouteSample `json:"samples"`
}
type WorkKindTriggerParams struct {
	Kind   string  `json:"kind"`
	Inputs []Event `json:"inputs"`
}
type WorkKindTriggerResult struct {
	Index int `json:"index"`
}
type RecurrenceNextParams struct {
	Kind  string          `json:"kind"`
	Spec  json.RawMessage `json:"spec"`
	Fired Timer           `json:"fired"`
	Now   time.Time       `json:"now"`
}
type RecurrenceNextResult struct {
	Occurrence *Event `json:"occurrence,omitempty"`
	Next       *Timer `json:"next,omitempty"`
}
type DependencyFilterParams struct {
	Filter Payload `json:"filter"`
	Event  Event   `json:"event"`
}
type DependencyFilterResult struct {
	Match bool `json:"match"`
}

// Operations.

type OperationParams struct {
	Operation string    `json:"operation"`
	Authority Authority `json:"authority"`
	Input     Payload   `json:"input"`
	RequestID string    `json:"request_id,omitempty"`
}
type OperationResult struct {
	Output Payload `json:"output"`
}
type EmitCommitParams struct {
	CommitID string  `json:"commit_id"`
	Emitted  Emitted `json:"emitted"`
}
type EventRenderParams struct {
	Event Event `json:"event"`
}
type EventRenderResult struct {
	Text string `json:"text,omitempty"`
	OK   bool   `json:"ok"`
}

// Executors.

type ExecutorRunParams struct {
	Executor string     `json:"executor"`
	RunID    string     `json:"run_id"`
	Request  RunRequest `json:"request"`
	// State is the persisted recovery metadata for executor.recover and
	// executor.resume.
	State *Payload `json:"state,omitempty"`
}
type ExecutorRunResult struct {
	Handle string `json:"handle"`
}
type ExecutorWaitParams struct {
	Executor string `json:"executor"`
	Handle   string `json:"handle"`
}
type ExecutorTargetParams struct {
	Executor    string `json:"executor"`
	ExecutionID string `json:"execution_id,omitempty"`
	WorkID      string `json:"work_id,omitempty"`
	Epoch       uint64 `json:"epoch,omitempty"`
	Event       *Event `json:"event,omitempty"`
}
type PersistRecoveryParams struct {
	RunID string  `json:"run_id"`
	State Payload `json:"state"`
}
type ResourceAcquireParams struct {
	RunID  string  `json:"run_id"`
	Mode   string  `json:"mode,omitempty"`
	Params Payload `json:"params"`
}
type ResourceLookupParams struct {
	RunID string `json:"run_id"`
	Ref   string `json:"ref"`
}
type ResourceResult struct {
	Resource Resource `json:"resource"`
	// Log is the provider's human-readable progress for this acquisition.
	Log string `json:"log,omitempty"`
	// SetupFailed carries extension.ErrResourceSetup: the resource exists
	// and is returned for inspection, but its setup failed.
	SetupFailed bool `json:"setup_failed,omitempty"`
}

// Namespaced KV: precondition batches, never interactive transactions.

type KVGetParams struct {
	Namespace string `json:"namespace"`
	Key       string `json:"key"`
}

// KVEntry carries a value and its version: a digest of the value, so a
// later kv.commit can require that nothing changed in between.
type KVEntry struct {
	Key     string `json:"key"`
	Value   []byte `json:"value"`
	Version string `json:"version"`
}
type KVGetResult struct {
	Found bool    `json:"found"`
	Entry KVEntry `json:"entry"`
}
type KVListParams struct {
	Namespace string `json:"namespace"`
	Prefix    string `json:"prefix,omitempty"`
}
type KVListResult struct {
	Entries []KVEntry `json:"entries"`
}

// KVCheck is a precondition: the key holds Version, or is absent.
type KVCheck struct {
	Namespace string `json:"namespace"`
	Key       string `json:"key"`
	Version   string `json:"version,omitempty"`
	Absent    bool   `json:"absent,omitempty"`
}
type KVPut struct {
	Namespace string `json:"namespace"`
	Key       string `json:"key"`
	Value     []byte `json:"value"`
}
type KVDelete struct {
	Namespace string `json:"namespace"`
	Key       string `json:"key"`
}
type KVCommitParams struct {
	Checks  []KVCheck  `json:"checks,omitempty"`
	Puts    []KVPut    `json:"puts,omitempty"`
	Deletes []KVDelete `json:"deletes,omitempty"`
}

// State reads.

type StateParams struct {
	ID      string `json:"id,omitempty"`
	ScopeID string `json:"scope_id,omitempty"`
	After   uint64 `json:"after,omitempty"`
	Limit   int    `json:"limit,omitempty"`
}

// Derived events and control.

// EventAcceptParams accepts one Event derived from Parent, an accepted Event
// of a scope the extension is configured in. The host applies the declared
// family, own source namespace, payload schema, causal depth and rate
// checks, and commits Checks/Records atomically with the Event. A subprocess
// never mints a causal root outside ingress.
type EventAcceptParams struct {
	ScopeID string     `json:"scope_id"`
	Parent  string     `json:"parent"`
	Event   Event      `json:"event"`
	Checks  []KVCheck  `json:"checks,omitempty"`
	Records []KVPut    `json:"records,omitempty"`
	Deletes []KVDelete `json:"deletes,omitempty"`
}
type EventAcceptResult struct {
	Event AcceptedEvent `json:"event"`
	Added bool          `json:"added"`
}
type TimerArmParams struct {
	ScopeID  string          `json:"scope_id"`
	Timer    Timer           `json:"timer"`
	Expected uint64          `json:"expected"`
	Kind     string          `json:"kind"`
	Spec     json.RawMessage `json:"spec"`
}
type TimerCancelParams struct {
	ID       string `json:"id"`
	Expected uint64 `json:"expected"`
}
type WorkCancelParams struct {
	WorkID   string `json:"work_id"`
	Expected uint64 `json:"expected"`
	Reason   string `json:"reason"`
	Key      string `json:"key"`
}
type WorkCancelResult struct {
	EventID string `json:"event_id"`
}

// WaitProgressParams set the progress line of a Work's active Wait, which
// one of the extension's own operations declared; "" clears it.
type WaitProgressParams struct {
	WorkID   string `json:"work_id"`
	Progress string `json:"progress"`
}

// Artifacts and secrets.

type ArtifactCreateParams struct {
	MediaType string `json:"media_type"`
}
type ArtifactCreateResult struct {
	Writer string `json:"writer"`
}
type ArtifactWriteParams struct {
	Writer string `json:"writer"`
	Chunk  []byte `json:"chunk"`
}
type ArtifactTarget struct {
	Writer string `json:"writer"`
}

// SecretResolveParams resolves secret names bound to one of the
// extension's entries in a configured scope: the entry's selected secret
// names, or, with Connection set, the secret names of that connection when
// an entry of the extension in the scope names it (07-03), or a binding in
// the scope names it and it is of one of the extension's connection types.
type SecretResolveParams struct {
	ScopeID    string   `json:"scope_id"`
	Connection string   `json:"connection,omitempty"`
	Names      []string `json:"names"`
}
type SecretResolveResult struct {
	Values map[string]string `json:"values"`
}

// Background tasks and checks.

type TaskRunParams struct {
	Task string `json:"task"`
}
type CheckRunParams struct {
	Check string `json:"check"`
	Deep  bool   `json:"deep,omitempty"`
	// Config is the read-only view of the configuration the check reads.
	Config CheckConfig `json:"config"`
}

// CheckRunResult is one check's outcome. Status, when set, is ok, info,
// warn or fail, with a stable Code and Items listed under the check; unset,
// OK alone decides (ok, or warn with code extension_check_failed).
type CheckRunResult struct {
	OK     bool     `json:"ok"`
	Detail string   `json:"detail,omitempty"`
	Status string   `json:"status,omitempty"`
	Code   string   `json:"code,omitempty"`
	Items  []string `json:"items,omitempty"`
}

// CheckConfig is the read-only view of the current configuration a check
// reads: each enabled scope that uses the extension (an entry of its types,
// a route on its handlers, a binding granted its operations, naming its
// connections, or on its executors or resource providers), with that
// scope's routes and bindings, and the connections of the extension's own
// types they name. It carries names and non-secret config, never secret
// values or reference text. Dir is the config file's directory, against
// which relative paths in it resolve.
type CheckConfig struct {
	Dir         string            `json:"dir,omitempty"`
	Scopes      []CheckScope      `json:"scopes,omitempty"`
	Connections []CheckConnection `json:"connections,omitempty"`
}

// CheckScope is one scope in a CheckConfig. Entries are the scope's entries
// of the extension's own types.
type CheckScope struct {
	ID       string         `json:"id"`
	Entries  []CheckEntry   `json:"entries,omitempty"`
	Routes   []CheckRoute   `json:"routes,omitempty"`
	Bindings []CheckBinding `json:"bindings,omitempty"`
}
type CheckEntry struct {
	Name       string          `json:"name"`
	Type       string          `json:"type"`
	Connection string          `json:"connection,omitempty"`
	Config     json.RawMessage `json:"config,omitempty"`
	// SecretNames are the names the entry selects, never their values.
	SecretNames []string `json:"secret_names,omitempty"`
}
type CheckRoute struct {
	Name    string `json:"name"`
	Handler string `json:"handler"`
	Binding string `json:"binding"`
}

// CheckBinding is one binding. Toolbelt is its executor's toolbelt
// capability (none, cli, mcp or sdk; empty when the executor is not
// registered). Resource is its workspace provider and that provider's
// config.
type CheckBinding struct {
	Name       string         `json:"name"`
	Executor   string         `json:"executor"`
	Toolbelt   string         `json:"toolbelt,omitempty"`
	Operations []string       `json:"operations,omitempty"`
	Connection string         `json:"connection,omitempty"`
	Resource   *CheckResource `json:"resource,omitempty"`
}
type CheckResource struct {
	Provider string          `json:"provider"`
	Config   json.RawMessage `json:"config,omitempty"`
}

// CheckConnection is a connection of one of the extension's own types:
// its config, without secret references.
type CheckConnection struct {
	ID     string          `json:"id"`
	Type   string          `json:"type"`
	Config json.RawMessage `json:"config,omitempty"`
}
