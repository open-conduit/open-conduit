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
	"log/slog"
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
	// AcceptInTx accepts a root Event of one of the extension's own
	// families (Event types named <Name>.*) into scope, inside the
	// extension's own transaction, so its own records commit with it.
	AcceptInTx func(tx storage.Tx, scope domain.ID, e domain.Event) (domain.AcceptedEvent, bool, error)
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
	// Capture writes a named telemetry event to the host's telemetry events
	// log and hands it to every Sink. scope is "" for one about the machine.
	Capture func(ctx context.Context, scope, name string, at time.Time, props map[string]any) error
	// Config reads and checks configuration without changing it, for an
	// extension that proposes config for the operator to apply (setup).
	Config ConfigHost
	// Probes are the setup probes other extensions and executors offer
	// (extension.SetupProbes), by full name ("claude-code.status"), and the
	// host's own (setup.init, setup.doctor).
	Probes func() map[string]extension.SetupProbe
}

// ConfigBase is the configuration a proposal starts from: the committed
// generation and its hash, and the config file's text (the default
// document when the daemon runs on it with no file). Folders are the IDs of
// the scopes that come from included folders, whose text is not in the file.
type ConfigBase struct {
	Generation string
	Hash       string
	Text       []byte
	Folders    []string
}

// ConfigDiagnostic is one validation problem: a JSON pointer, a code and a
// remedy, as `config validate` prints them.
type ConfigDiagnostic struct {
	Path    string `json:"path"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// ConfigHost is the host's configuration service for extensions. Nothing
// in it applies a configuration: only the operator does that.
type ConfigHost interface {
	// Base is the configuration now.
	Base(ctx context.Context) (ConfigBase, error)
	// Validate runs the side-effect-free validation POST /config/validate
	// runs on config file text: the candidate's hash when it is valid, its
	// diagnostics when not.
	Validate(ctx context.Context, text []byte) (hash string, diagnostics []ConfigDiagnostic, err error)
	// Edit makes one `config set`/`config unset` edit to config file text,
	// keeping its layout and key order: op "add" sets (appending with "-"),
	// "replace" sets a value that exists and "remove" removes one. It
	// returns the new text and the value that was at pointer (nil when
	// none). Pointers address array elements by id or name. Its errors are
	// fixed, secret-free reasons.
	Edit(text []byte, op, pointer string, value json.RawMessage) (out []byte, before json.RawMessage, err error)
	// Trace runs the route trace for one of a scope's named samples against
	// config text instead of the active generation, writing nothing. With
	// no sample it lists the scope's samples.
	Trace(ctx context.Context, text []byte, scope, sample string) (CandidateTrace, error)
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
// Standing Work outlives its attempts (only cancellation ends it); Trigger,
// when set, picks the input that triggers an attempt from the pending batch
// and must be pure and deterministic.
type WorkKind struct {
	Kind     string
	Standing bool
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
