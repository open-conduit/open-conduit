package extension

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/open-conduit/open-conduit/domain"
)

const (
	ToolbeltNone = "none"
	ToolbeltCLI  = "cli"
	ToolbeltMCP  = "mcp"
	ToolbeltSDK  = "sdk"
)

type ExecutionContext struct {
	Config  domain.Payload
	Secrets map[string]string
	// Resources is nil unless the binding declares a resource provider. An
	// executor acquires what it needs through it (see resource.go); absence is
	// valid and every executor must operate without it.
	Resources ResourceBroker
	// Sandbox is nil unless the binding declares a process sandbox (B09).
	// An executor that advertises Capabilities.Sandbox wraps its launch
	// through it (WrapLaunch) and proves a recovered segment still runs in
	// it (AdoptSandboxed); see sandbox.go.
	Sandbox SandboxBroker
	// Container is nil unless the binding declares a container. An
	// executor that advertises Capabilities.Container launches through it
	// and calls its Adopt in Recover; see container.go.
	Container ContainerBroker
	// ContainerHome is set with Container: the Work's own home directory,
	// host-owned and kept across its segments (so a session resumes), and
	// mounted read-write at the same path. The executor points the
	// process's HOME and its own config there.
	ContainerHome                   string
	ToolbeltEndpoint, ToolbeltToken string
	// ToolbeltTokenFile, when set, is the host-kept file holding the current
	// ToolbeltToken, in a token directory the host rotates it in on recovery.
	// An executor points its process at it (OS_TOKEN_FILE) instead of
	// writing a token file of its own.
	ToolbeltTokenFile string
}
type RunRequest struct {
	Work      domain.Work
	Execution domain.Execution
	Trigger   domain.Event
	// Inputs retains the acceptance-ordered batch, including context preceding a
	// wake. Trigger remains the selected event; recovery restores the same batch.
	Inputs  []domain.Event
	Context ExecutionContext
	// PersistRecovery durably saves opaque executor-owned metadata before launch
	// is acknowledged. It is fenced by this execution's current lease.
	PersistRecovery func(domain.Payload) error
}
type Usage struct {
	InputTokens, OutputTokens, CachedTokens *uint64
	Cost                                    *float64
	Currency                                string
}
type Result struct {
	Outcome   domain.Outcome
	Usage     Usage
	Artifacts []domain.ArtifactRef
	// RetryHint optionally classifies a structured failed outcome as a
	// retryable executor limit (for example a usage limit with its reset).
	// Core treats it like a RetryHintProvider error, except that the observed
	// result also establishes that the attempt finished. Only failed outcomes
	// may carry it.
	RetryHint *RetryHint
	// ConsumedInputs is the generic inject acknowledgement (B04, 04-02). It
	// names Events core delivered to this attempt through Inject that the
	// executor has evidence the running process took in (an echo of the
	// injected message, a read of its inbox). It may accompany any outcome
	// (completed, handoff, waiting or failed) and is applied atomically with
	// that result. Core consumes such an input only when it holds an inject
	// intent for this execution whose dispatch was attempted, and the input is
	// still pending; any other ID is ignored, so the field grants no
	// authority. A consumed input is never presented again (not to attempt
	// N+1, not as the wake of a parked wait). Without proof an injected input
	// stays pending and is presented again, explicitly, to the next attempt
	// or wake.
	ConsumedInputs []domain.ID
	// Workspace describes the resource the attempt ran in, when its binding
	// declared one (B07, 05-02). An executor that acquired a resource reports
	// the provider's description here, so history shows the workspace for
	// every executor without interpreting executor payloads.
	Workspace *WorkspaceRecord
}

// WorkspaceRecord is the history description of an acquired resource: the
// provider's kind, opaque lease reference, working location, one-line
// human summary and opaque description data.
type WorkspaceRecord struct {
	Kind, Ref, Dir, Summary string
	Data                    domain.Payload
}

// RetryHint classifies a retryable executor limit without provider-specific
// fields. A nil reset means the reset time is unknown. Core validates bounds and
// selects backoff; a hint never grants authority or an immediate retry.
type RetryHint struct{ ResetAt *time.Time }

// RetryHintProvider may be implemented by an executor error. Runtime calls it
// outside coordination transactions and persists only validated policy data.
type RetryHintProvider interface{ RetryHint() RetryHint }

// Wait returns one structured outcome or a transport/process error. Cancellation
// of observation does not imply termination of a detached execution.
type RunHandle interface {
	Wait(context.Context) (Result, error)
}
type Executor interface {
	Capabilities() domain.Capabilities
	Run(context.Context, RunRequest) (RunHandle, error)
	// Recover must never launch replacement work. It returns the existing handle,
	// a finished result via that handle, or ErrNotAdoptable.
	Recover(context.Context, RunRequest, domain.Payload) (RunHandle, error)
}
type InterruptExecutor interface {
	Interrupt(context.Context, domain.ID) error
}

// InjectExecutor delivers an accepted Event to the running attempt.
//
//   - nil means accepted for delivery, never proof that the process took it
//     in. Consumption is proved only by the finished result's ConsumedInputs.
//   - ErrNotAccepting means the Event was definitely not delivered: no live
//     process of this execution can take input here (it finished, it was
//     adopted without an input channel, or this executor instance does not
//     hold it). Core stops dispatching that intent and keeps the input
//     pending for the next attempt or wake; it is not retried.
//   - Any other error is ambiguous (the Event may have been delivered). Core
//     retries with the same intent and Event identity, or applies the
//     binding's configured error fallback.
//
// Delivering one Event identity twice (a retried ambiguous dispatch) must not
// make the executor report it consumed twice.
type InjectExecutor interface {
	Inject(context.Context, domain.ID, domain.Event) error
}

// ErrNotAccepting is the definitive "not delivered" answer of Inject.
var ErrNotAccepting = errors.New("execution is not accepting input")

type ResumeExecutor interface {
	Resume(context.Context, RunRequest, domain.Payload) (RunHandle, error)
}

// ForgetExecutor is optional. Forget releases executor-private state (for
// example a run directory) for a terminal execution whose resource exclusion is
// confirmed and whose history runtime retention has expired. Runtime never
// calls it for an attempt that could still be recovered or resumed. It must be
// idempotent and must never signal or adopt processes.
type ForgetExecutor interface {
	Forget(context.Context, domain.ID) error
}

// StopExecutor is optional trusted host cleanup for a fenced attempt (for
// example after operator cancel, a deadline, ErrNotAdoptable or a failed
// observation). Stop terminates any verified live part of the execution at
// that lease epoch and returns nil only once nothing of it can still run, so
// the host may confirm physical exclusion. A newer adopting epoch must make
// it fail. Without it the host cannot prove a stop and replacement stays
// blocked. Promoted from the daemon's private interface in 07-01; shell,
// claude-code and legacy-job implement it.
type StopExecutor interface {
	Stop(ctx context.Context, execution domain.ID, epoch uint64) error
}

// AttachExecutor is optional. Attach returns an interactive command an
// operator runs locally to take over a Work's executor-private continuity
// (for example an agent conversation), for Work the operator holds with no
// attempt running. It carries no environment: the operator's own shell
// supplies it. ErrNothingToAttach means the Work has nothing to attach to
// yet; an executor without this interface does not support attach. Derived
// from the operator attach control (03-05) in 07-01.
type AttachExecutor interface {
	Attach(context.Context, domain.ID) (AttachSpec, error)
}

// AttachSpec is an attach command: argv and an optional working directory.
type AttachSpec struct {
	Argv []string
	Dir  string
}

// ContainerAttachExecutor is optional, for an executor whose Work ran in a
// container: its Attach returns `open-conduit container attach <work>`, and
// ContainerAttach is what that command runs inside a new container of the
// binding, with the Work's home and workspace, so nothing the agent wrote is
// ever read or run on the host. ErrNothingToAttach applies as in Attach.
type ContainerAttachExecutor interface {
	ContainerAttach(ctx context.Context, r ContainerAttachRequest) (ContainerAttachSpec, error)
}

// ContainerAttachRequest is the Work to attach to, its binding's config and
// secrets as a launch would get them, and the Work's container home.
type ContainerAttachRequest struct {
	Work    domain.Work
	Config  domain.Payload
	Secrets map[string]string
	Home    string
}

// ContainerAttachSpec is an interactive launch for the attach container:
// the needs and launch (Dir is the workspace) a ContainerBroker plans.
type ContainerAttachSpec struct {
	Needs  SandboxNeeds
	Launch SandboxLaunch
}

// ErrNothingToAttach is the definitive "nothing to attach to yet" answer of
// Attach.
var ErrNothingToAttach = errors.New("nothing to attach to")

func ValidateBinding(b domain.Binding, e Executor) error {
	c, r := e.Capabilities(), b.Requires
	valid := func(s string) bool {
		return s == ToolbeltNone || s == ToolbeltCLI || s == ToolbeltMCP || s == ToolbeltSDK
	}
	if !valid(c.Toolbelt) || (r.Toolbelt != "" && !valid(r.Toolbelt)) {
		return fmt.Errorf("%w: toolbelt transport", ErrInvalid)
	}
	if c.Interrupt {
		if _, ok := e.(InterruptExecutor); !ok {
			return fmt.Errorf("%w: interrupt implementation", ErrInvalid)
		}
	}
	if c.Inject {
		if _, ok := e.(InjectExecutor); !ok {
			return fmt.Errorf("%w: inject implementation", ErrInvalid)
		}
	}
	if c.Resume {
		if _, ok := e.(ResumeExecutor); !ok {
			return fmt.Errorf("%w: resume implementation", ErrInvalid)
		}
	}
	if r.Interrupt && !c.Interrupt || r.Inject && !c.Inject || r.Resume && !c.Resume || r.Workspace && !c.Workspace || r.Toolbelt != "" && r.Toolbelt != c.Toolbelt {
		return fmt.Errorf("%w: binding %s", ErrUnsupported, b.Name)
	}
	return nil
}
func ValidateResult(r Result) error {
	switch r.Outcome.Kind {
	case domain.OutcomeCompleted, domain.OutcomeFailed, domain.OutcomeWaiting, domain.OutcomeHandoff:
	default:
		return fmt.Errorf("%w: outcome", ErrInvalid)
	}
	if r.RetryHint != nil && r.Outcome.Kind != domain.OutcomeFailed {
		return fmt.Errorf("%w: retry hint on a non-failed outcome", ErrInvalid)
	}
	seen := map[domain.ID]bool{}
	for _, id := range r.ConsumedInputs {
		if id == "" || seen[id] {
			return fmt.Errorf("%w: consumed inputs", ErrInvalid)
		}
		seen[id] = true
	}
	if r.Usage.Cost != nil && (math.IsNaN(*r.Usage.Cost) || math.IsInf(*r.Usage.Cost, 0) || *r.Usage.Cost < 0 || r.Usage.Currency == "") {
		return fmt.Errorf("%w: cost", ErrInvalid)
	}
	return nil
}

// LiveExecutor is optional. Live returns what a running execution has
// written so far, in the same order and media types as the artifacts its
// result will publish (for example rendered activity, then the raw
// transcript), so an operator can follow an attempt before it ends. Each
// output's Content holds at most its last tail bytes (all of it when tail
// is not positive); Size is the whole output's length. ErrNotLive means no
// segment of the execution is running here: its output is, or will be, in
// its artifacts.
type LiveExecutor interface {
	Live(ctx context.Context, execution domain.ID, tail int64) ([]LiveOutput, error)
}

// LiveOutput is one output of a running execution.
type LiveOutput struct {
	Name, MediaType string
	Size            int64
	Content         []byte
}

// ErrNotLive is the definitive "not running here" answer of Live.
var ErrNotLive = errors.New("execution is not running")
