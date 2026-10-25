package extension

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/alsiola/open-conduit/domain"
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
	Resources                       ResourceBroker
	ToolbeltEndpoint, ToolbeltToken string
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

// Injection acknowledgement/completion races are refined in 04-02. Success here
// means accepted, never proof that an event was consumed by the running process.
type InjectExecutor interface {
	Inject(context.Context, domain.ID, domain.Event) error
}
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
	if r.Usage.Cost != nil && (math.IsNaN(*r.Usage.Cost) || math.IsInf(*r.Usage.Cost, 0) || *r.Usage.Cost < 0 || r.Usage.Currency == "") {
		return fmt.Errorf("%w: cost", ErrInvalid)
	}
	return nil
}
