// Package inproc is the in-process extension contract (issue #66): what an
// extension running inside the daemon offers the host, and the host
// services it is given. Bootstrap composes every in-process extension
// through it by iterating one list, never by naming an extension, so a
// first-party extension reaches nothing an installed package could not.
//
// Every surface but Name is optional: an extension implements the
// interfaces of the surfaces it has. Types, Operations, Routes, WorkKinds
// and Recurrences are read before Start and offline (config validation), so
// they must not depend on host services; every other surface is used only after Start, except
// Exporter, which joins the telemetry pipeline before it.
//
// The surfaces mirror the subprocess manifest (protocol.Manifest): Configure
// receives the same protocol.ConfigureParams an installed package does.
package inproc

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/observability"
	"github.com/open-conduit/open-conduit/protocol"
	"github.com/open-conduit/open-conduit/storage"
)

// Extension is an in-process extension. Name identifies it in diagnostics,
// task keys and doctor items.
type Extension interface {
	Name() string
}

// ConfigType is a config type the extension owns. Kind is
// protocol.ValidateConnection, protocol.ValidateExtension or
// protocol.ValidateRoute. Validate checks an entry's opaque config at load
// and apply (nil accepts any object); its error text is never echoed.
//
// Binding, for a route handler's type only, checks the binding each route
// with this handler binds, wherever the route is declared, so a handler can
// refuse an agent it must not run (one with a shell, or with operations it
// must not have). Its error is shown to the operator, so it must be a fixed,
// secret-free reason.
type ConfigType struct {
	Kind     string
	Type     string
	Ingress  bool
	Validate func(config json.RawMessage, secretNames []string) error
	Binding  func(extension.RouteBinding) error
	// Settings, for an entry type only, optionally lists the entry's config
	// fields that are safe to show the operator, with typed values.
	Settings func(config json.RawMessage) []extension.Setting
	// Describe, for an entry type only, optionally lists what an entry's
	// config sets up (a schedule's jobs and when they run), for the
	// operator's view of the config. It must not reveal secrets.
	Describe func(config json.RawMessage) []Entry
}

// Entry is one thing an extension entry sets up, named and described.
type Entry struct{ Name, Detail string }

// Typed extensions own config types.
type Typed interface {
	Types() []ConfigType
}

// Operator extensions name the toolbelt operations config may grant.
type Operator interface {
	Operations() []string
}

// Toolsetter extensions publish toolsets: named sets of their operations
// (and builtins) for the roles their agents play, which config grants as
// "@<name>" (extension.Toolset). Toolsets is read offline, like
// Declarations, and must not depend on host services.
type Toolsetter interface {
	Toolsets() []extension.Toolset
}

// Declarer extensions declare their toolbelt operations: each one's Name,
// and its Summary and Input (see extension.Operation), with no handler.
// Declarations is read before Start, offline (config validation and the
// operations reference), so it must not depend on host services. The host
// gives each operation of the extension's Registration the declared
// Summary and Input it leaves unset (Describe), so they are declared once.
type Declarer interface {
	Declarations() []extension.Operation
}

// Describe returns r with each operation's unset Summary and Input taken
// from the declaration of the same name in decls.
func Describe(r extension.Registration, decls []extension.Operation) extension.Registration {
	if len(decls) == 0 {
		return r
	}
	byName := make(map[string]extension.Operation, len(decls))
	for _, d := range decls {
		byName[d.Name] = d
	}
	ops := make([]extension.Operation, len(r.Operations))
	for i, op := range r.Operations {
		if d, ok := byName[op.Name]; ok {
			if op.Summary == "" {
				op.Summary = d.Summary
			}
			if len(op.Input) == 0 {
				op.Input = d.Input
			}
		}
		ops[i] = op
	}
	r.Operations = ops
	return r
}

// Router extensions provide route handlers by name.
type Router interface {
	Routes() map[string]extension.RouteHandler
}

// Host is what the host gives an extension at Start.
type Host struct {
	Store storage.Store
	Now   func() time.Time
	Log   *slog.Logger
	// Secrets resolves names bound to the extension's own entries in scope:
	// an entry's selected secret names, or with connection set the secret
	// names of a connection one of its entries or one of the scope's
	// bindings names, of one of its connection types (B05, as for an
	// installed package). An operation's Authority.Connection is the one
	// the calling Execution's binding names.
	Secrets func(ctx context.Context, scope domain.ID, connection string, names []string) (map[string]string, error)
	// Report records a safe diagnostic code for the extension (logged once
	// per change; "" clears it).
	Report func(code string)
	// Nudge asks the host loop for an early pass, after the extension made
	// something the loop acts on (an accepted Event, an armed timer).
	Nudge func()
	// StateDir is the daemon's state directory, and DryRun whether the
	// daemon makes no external writes and starts no timed work.
	StateDir string
	DryRun   bool
	// Artifacts is the host's artifact store, for an executor's logs and
	// output files.
	Artifacts storage.Artifacts
	// ToolbeltDir holds the `os` binary runs reach the toolbelt through, and
	// PlaybookDir the playbook plugin the host writes; each run's
	// ExecutionContext carries them too.
	ToolbeltDir, PlaybookDir string
	// ExecutorDir is the state directory of one of the extension's
	// executors, by name: the host's <state>/executors/<name>. An executor
	// keeps its runs under <dir>/runs, where the host roots a sandboxed or
	// contained run's directory, in process or out. Nil or empty: the
	// executor uses StateDir/executors/<name>.
	ExecutorDir func(name string) string
	// Render renders an input Event for an agent's prompt through the
	// extension that owns its family (Renderer); false leaves it to the
	// caller's own generic rendering.
	Render func(ctx context.Context, e domain.Event) (string, bool)
	// OpenLog opens the extension's own JSON-lines log, <state>/logs/
	// <name>.jsonl, rotated by size like the host's logs. Each Write must be
	// one whole line.
	OpenLog func(name string) (io.WriteCloser, error)
	// AcceptInTx accepts a root Event of one of the extension's own
	// families (Event types named <Name>.*) into scope, inside the
	// extension's own transaction, so its own records commit with it.
	// Served over the protocol (protocol/sdk/inprocsdk) the Event is
	// returned from the transaction and accepted when it commits, so a new
	// Event's answer has Sequence 0: key records by its ID, not its
	// sequence. An Event already accepted is answered as it stands, with
	// added false.
	AcceptInTx func(tx storage.Tx, scope domain.ID, e domain.Event) (domain.AcceptedEvent, bool, error)
	// AcceptDerivedInTx accepts an Event of one of the extension's own
	// families as derived from parent, an accepted Event in scope, inside
	// the extension's own transaction: it lands in parent's causal chain,
	// bounded by the generation's causal limits. A bound it would exceed is
	// ErrCausalLimit, recorded by the host for the operator. Served over the
	// protocol, the bound is known only at commit: the call succeeds, the
	// rest of the transaction commits, and Transact returns an error
	// wrapping ErrCausalLimit. Sequence is 0 for a new Event, as for
	// AcceptInTx.
	AcceptDerivedInTx func(tx storage.Tx, scope, parent domain.ID, e domain.Event) (domain.AcceptedEvent, bool, error)
	// EventScope is the scope an accepted Event was accepted into, read in
	// the extension's own transaction.
	EventScope func(tx storage.Tx, a domain.AcceptedEvent) (domain.ID, error)
	// Deliver pushes a delivery to one of the extension's own ingress
	// sources (Ingester) by its registration name, acknowledged once it is
	// durably accepted.
	Deliver func(ctx context.Context, source string, d extension.Delivery) error
	// Authorize answers whether scope from may use scope to's events or
	// tracker, by kind, under the operator's authorization config: "read"
	// (authorization.read_scopes, a cross-scope wait or read) or "file"
	// (authorization.file_scopes, filing issues in and commenting on its
	// tracker). nil is allowed; the error says how to allow it.
	Authorize func(kind string, from, to domain.ID) error
	// WaitTarget resolves a named wait target of kind in scope ("job" for
	// a `job:<name>` condition) through the extensions that resolve that
	// kind (Targeter): the correlation key and Event types that end it.
	WaitTarget func(ctx context.Context, scope domain.ID, kind, name string) (key domain.CorrelationKey, types []string, ok bool)
	// Timers arms and cancels the extension's recurring timers through the
	// current generation: only its own recurrence kinds and timers whose
	// Event is of its own families.
	Timers Timers
	// CancelWork cancels Work of the extension's own kinds (the operator's
	// cancellation, idempotent per key and guarded by the Work's revision:
	// a stale revision is storage.ErrConflict).
	CancelWork func(ctx context.Context, work domain.ID, expected domain.Revision, reason, key string) (domain.ID, error)
	// WaitProgress sets the progress line ("3 of 7 checks finished") of
	// work's active Wait, when one of the extension's own operations
	// declared it; "" clears it. Another's Wait is
	// extension.ErrUnauthorized, and Work with no active Wait
	// storage.ErrNotFound.
	WaitProgress func(ctx context.Context, work domain.ID, progress string) error
	// SettleHold settles a hold the extension's start gate placed on work
	// (extension.StartGate): admit lets its next start through once without
	// asking the start gates again; otherwise the Work is cancelled. reason
	// goes in the Work's history. It is idempotent per key. Work the
	// extension's gate does not hold is an error.
	SettleHold func(ctx context.Context, work domain.ID, admit bool, reason, key string) (domain.ID, error)
	// Act invokes an action for a person the extension's own surface
	// identified (extension.ActRequest): the caller is {Kind: the
	// extension's name, ID: r.Caller}, and the provider authorises it as it
	// would any caller. A refusal is the provider's *OperationError.
	Act func(ctx context.Context, r extension.ActRequest) (extension.ActResult, error)
	// Actions lists the actions every provider offers now on a scope, or
	// on work in it (work non-empty), so the extension can show them on
	// its own surface for Act to take.
	Actions func(ctx context.Context, scope, work domain.ID) ([]extension.ActionOffer, error)
	// Capture writes a named telemetry event to the host's telemetry events
	// log and hands it to every Sink. scope is "" for one about the machine.
	Capture func(ctx context.Context, scope, name string, at time.Time, props map[string]any) error
	// Config reads and checks configuration without changing it, for an
	// extension that proposes config for the operator to apply (setup).
	Config ConfigHost
	// Probes are the setup probes other extensions and executors offer
	// (extension.SetupProbes), by full name ("claude-code.status"), and the
	// host's own (open-conduit.init, open-conduit.doctor).
	Probes func() map[string]extension.SetupProbe
}

// ConfigBase is the configuration a proposal starts from: the committed
// generation and its hash, and a handle on the config file's text (the
// default document when the daemon runs on it with no file). The host keeps
// the text; Text is a copy with every secret_refs value replaced by
// "[redacted]", so an extension never holds reference text. Folders are the
// IDs of the scopes that come from included folders, whose text is not in
// the file.
type ConfigBase struct {
	Generation string
	Hash       string
	Handle     string
	Text       []byte
	Folders    []string
}

// ConfigEdited is a candidate the host built from an edit: a handle on its
// text, the text redacted as ConfigBase's is, and the value that was at the
// pointer (nil when none), redacted too.
type ConfigEdited struct {
	Handle string
	Text   []byte
	Before json.RawMessage
}

// ConfigDiagnostic is one validation problem: a JSON pointer, a code and a
// remedy, as `config validate` prints them.
type ConfigDiagnostic struct {
	Path    string `json:"path"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// ErrConfigHandle refuses a config handle the host doesn't hold (one from
// before a restart, or long unused): read the base again.
var ErrConfigHandle = errors.New("the host no longer holds that configuration; read the base again")

// ConfigHost is the host's configuration service for extensions. The host
// keeps the config text behind handles and hands out only redacted copies;
// an extension builds candidates by proposing edits against a handle.
// Nothing in it applies a configuration: only the operator does that,
// through POST /config/apply.
type ConfigHost interface {
	// Base is the configuration now.
	Base(ctx context.Context) (ConfigBase, error)
	// Edit makes one edit to the text behind handle, keeping its layout
	// and key order, and holds the result as a new candidate. op is RFC
	// 6902's: "add" adds or replaces an object member and inserts into an
	// array before a numeric index (appending at the length or "-"),
	// refusing an element named by id or name since one is there;
	// "replace" sets a value that exists and "remove" removes one. value may set a new
	// reference. Pointers address array elements by id or name. Its errors
	// are fixed, secret-free reasons.
	Edit(ctx context.Context, handle, op, pointer string, value json.RawMessage) (ConfigEdited, error)
	// Validate runs the side-effect-free validation POST /config/validate
	// runs on the text behind handle: the candidate's hash when it is
	// valid, its diagnostics when not.
	Validate(ctx context.Context, handle string) (hash string, diagnostics []ConfigDiagnostic, err error)
	// Trace runs the route trace for one of a scope's named samples against
	// the text behind handle instead of the active generation, writing
	// nothing. With no sample it lists the scope's samples.
	Trace(ctx context.Context, handle, scope, sample string) (CandidateTrace, error)
}

// CandidateTrace is a route trace against a candidate configuration: what
// routing would do with the sample, route by route.
type CandidateTrace struct {
	Samples []CandidateSample `json:"samples,omitempty"`
	Sample  string            `json:"sample,omitempty"`
	Routes  []CandidateRoute  `json:"routes,omitempty"`
	// Effect is what routing would do: start, queue, inject, interrupt,
	// wake, drop or unmatched, with the Work and binding when there is one.
	Effect CandidateEffect `json:"effect,omitempty"`
}

type CandidateSample struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

type CandidateRoute struct {
	Route   string `json:"route"`
	Handler string `json:"handler"`
	Binding string `json:"binding,omitempty"`
	Verdict string `json:"verdict"`
	Reason  string `json:"reason,omitempty"`
}

type CandidateEffect struct {
	Kind    string `json:"kind,omitempty"`
	WorkID  string `json:"work_id,omitempty"`
	Binding string `json:"binding,omitempty"`
	Detail  string `json:"detail,omitempty"`
}

// ErrNotOwn refuses a host service call on something that is not the
// extension's own: an Event family, a recurrence kind, a timer or a Work
// kind it did not declare.
var ErrNotOwn = errors.New("not the extension's own")

// ErrCausalLimit refuses a derived Event that would exceed the causal depth
// or rate bounds of its chain (Host.AcceptDerivedInTx). The host has
// recorded the refusal; the extension's transaction may still commit.
var ErrCausalLimit = errors.New("causal depth or rate bound")

// Timers is the host's timer control.
type Timers interface {
	ArmRecurrence(ctx context.Context, scope domain.ID, timer domain.Timer, expected domain.Revision, kind string, spec []byte) error
	Cancel(ctx context.Context, id domain.ID, expected domain.Revision) error
}

// Starter extensions start once the host services exist, before the first
// generation is configured.
type Starter interface {
	Start(ctx context.Context, h Host) error
}

// Stopper extensions release what Start acquired at shutdown.
type Stopper interface {
	Stop()
}

// WorkKind is the lifecycle of a Work kind the extension's routes create.
// Standing Work outlives its attempts (only cancellation ends it); RunOnce
// Work does all its work in one attempt, so an attempt that hands back
// counts as completed; Trigger,
// when set, picks the input that triggers an attempt from the pending batch
// and must be pure and deterministic. Timed Work runs on a clock rather
// than for something that happened (a scheduled job), so a dry run starts
// none of it and its triggers wait.
type WorkKind struct {
	Kind     string
	Standing bool
	RunOnce  bool
	Timed    bool
	Trigger  func(inputs []domain.Event) int
}

// WorkKinder extensions declare the lifecycle of their own Work kinds.
type WorkKinder interface {
	WorkKinds() []WorkKind
}

// RecurrenceFunc is a pure, deterministic recurrence policy run inside the
// timer's firing transaction: given the stored spec, the fired timer and the
// firing time, the occurrence Event to accept (nil skips this firing) and
// the replacement timer (nil ends the recurrence). It does no I/O.
type RecurrenceFunc func(spec []byte, fired domain.Timer, now time.Time) (occurrence *domain.Event, next *domain.Timer, err error)

// Recurring extensions own timer recurrence kinds, by kind.
type Recurring interface {
	Recurrences() map[string]RecurrenceFunc
}

// Configurer extensions receive every applied generation (and every public
// URL change): the enabled scopes' entries of their own config types, with
// the connection each names when that is one of their connection types.
type Configurer interface {
	Configure(ctx context.Context, p protocol.ConfigureParams)
}

// Configure also carries the entries in scopes that are configured but
// disabled, in ConfigureParams.Disabled: the extension starts nothing for
// them, but keeps what it holds for them (timers, cursors) until the scope
// is enabled again or the entry removed.

// Source is an ingress source the extension registers: Name is its
// registration name, the extension's name or one beginning with name + "/";
// Interval is how often the host reconciles it (zero: the configured
// reconcile interval).
type Source struct {
	Name     string
	Source   extension.Source
	Ingress  extension.Ingress
	Interval time.Duration
}

// Ingester extensions register ingress sources. Sources is read after
// every Configure; a registration is immutable for the daemon's lifetime,
// so a source already registered keeps its first registration until
// restart.
type Ingester interface {
	Sources() []Source
}

// Webhooker extensions receive the webhook requests under /<Name>/ on the
// host's webhook listener. Webhook is given the path below that prefix
// ("a/b" for /<Name>/a/b) and returns the handler for it, or nil for none
// (the host answers 404). The handler verifies the request itself and
// pushes what it accepts through Host.Deliver.
type Webhooker interface {
	Webhook(path string) http.Handler
}

// Targeter extensions resolve named wait targets in a scope, by kind
// ("job" for a `job:<name>` wait): the correlation key and Event types
// that end the thing named, so another extension's wait operation can wait
// on it through ordinary dependencies.
type Targeter interface {
	Targets() map[string]func(ctx context.Context, scope domain.ID, name string) (key domain.CorrelationKey, types []string, ok bool)
}

// SegmentEnder extensions are told when an execution segment run in a
// container ends, so they revoke what they issued for it (git tokens). The
// host calls it before the segment settles; an extension sweeps what it
// misses itself.
type SegmentEnder interface {
	SegmentEnded(ctx context.Context, execution domain.ID)
}

// PoolJobs is the capacity pool of job executors: settings.job_concurrency
// bounds them, and settings.concurrency every other executor (agent turns),
// so neither starves the other.
const PoolJobs = "jobs"

// Executor is an executor an extension registers, by name: the executor
// and the capacity pool it counts against ("" for agent turns, PoolJobs for
// jobs).
type Executor struct {
	Executor extension.Executor
	Pool     string
}

// Executing extensions register executors bindings name. Executors is read
// before Start (configuration validation checks bindings against them) and
// must not depend on host services.
type Executing interface {
	Executors() map[string]Executor
}

// Tooled extensions register toolbelt operations on every generation's
// toolbelt.
type Tooled interface {
	Registration() extension.Registration
}

// EntryGated extensions' toolbelt registration joins only a generation in
// which an enabled scope has one of their entries: removing the last entry
// removes their operations, and a binding still granting them keeps running
// (its calls are undeclared).
type EntryGated interface {
	EntryGated()
}

// Actor extensions offer action providers on a scope, by provider name.
type Actor interface {
	Actions(scope domain.ID) map[string]extension.ActionProvider
}

// Titled extensions give the display name operators read where their Work
// and sources are listed (an installed package's manifest title). Without
// one, the extension shows as its name. Title is read after Start and must
// not change.
type Titled interface {
	Title() string
}

// Paged extensions contribute pages to the operator's web view
// (extension.Page). Pages is read after Start and must not change.
type Paged interface {
	Pages() []extension.Page
}

// Annotation is one line an extension contributes to a Work's view.
type Annotation struct {
	Source, Name, Value string
}

// Annotator extensions annotate Work (GET /work/{id}).
type Annotator interface {
	Annotate(ctx context.Context, w domain.Work) []Annotation
}

// Renderer extensions render their own Event families for agent prompts;
// false leaves the Event to the next renderer.
type Renderer interface {
	Render(ctx context.Context, e domain.Event) (string, bool)
}

// Task is background work the host loop runs. Pace is the least time
// between starts (zero: every loop pass). A task never overlaps itself.
type Task struct {
	Name string
	Pace time.Duration
	Run  func(ctx context.Context) error
}

// Tasker extensions have background tasks.
type Tasker interface {
	Tasks() []Task
}

// Check statuses, as doctor reports them.
const (
	CheckOK   = "ok"
	CheckInfo = "info"
	CheckWarn = "warn"
	CheckFail = "fail"
)

// CheckResult is one doctor check.
type CheckResult struct {
	Name, Status, Code, Detail string
	Items                      []string
}

// Check is a doctor check. A deep check runs only when secrets are checked
// and may call services; an ordinary one must not. Run reads the host's
// read-only view of the configuration, cfg (protocol.CheckConfig, the view
// an installed package's check.run receives). An Offline check reads
// nothing but cfg and the machine, so doctor also runs it without a daemon.
type Check struct {
	Deep    bool
	Offline bool
	Run     func(ctx context.Context, cfg protocol.CheckConfig) CheckResult
}

// Checker extensions contribute doctor checks.
type Checker interface {
	Checks() []Check
}

// Playbooker extensions contribute agent playbooks: skill name to its
// SKILL.md text, written into the Claude Code plugin.
type Playbooker interface {
	Playbooks() map[string]string
}

// Exporting extensions receive lifecycle telemetry. The exporter joins the
// pipeline before Start, so it must send nothing until configured.
type Exporting interface {
	Exporter() observability.Exporter
}

// Sink extensions receive the named telemetry events the host writes to its
// telemetry events log, besides that log. A failure never fails the log.
// scope is the scope the event belongs to, "" for one about the machine
// (a host reading); props never carry it.
type Sink interface {
	Capture(ctx context.Context, scope, name string, at time.Time, props map[string]any) error
}
