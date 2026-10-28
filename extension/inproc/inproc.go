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
type ConfigType struct {
	Kind     string
	Type     string
	Ingress  bool
	Validate func(config json.RawMessage, secretNames []string) error
}

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
	// names of a connection one of its entries names (B05, as for an
	// installed package).
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
// and may call services; an ordinary one must not.
type Check struct {
	Deep bool
	Run  func(ctx context.Context) CheckResult
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
