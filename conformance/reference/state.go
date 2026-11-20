package reference

import (
	"context"
	"slices"
	"sync"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
)

// StateExecutor keeps private state per execution, as an executor's run
// directory, and sweeps it (extension.StateSweeper): Recover adopts only
// an execution whose state it still holds, Forget releases one, and
// SweepState releases those the host doesn't know.
type StateExecutor struct {
	Result extension.Result
	mu     sync.Mutex
	held   map[domain.ID]bool
}

const stateSchema = "fixture.state.v1"

func (e *StateExecutor) Capabilities() domain.Capabilities {
	return domain.Capabilities{Toolbelt: extension.ToolbeltCLI}
}

func (e *StateExecutor) Run(ctx context.Context, r extension.RunRequest) (extension.RunHandle, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if r.PersistRecovery == nil || r.Execution.ID == "" {
		return nil, extension.ErrInvalid
	}
	e.mu.Lock()
	if e.held == nil {
		e.held = map[domain.ID]bool{}
	}
	e.held[r.Execution.ID] = true
	e.mu.Unlock()
	if err := r.PersistRecovery(domain.Payload{Schema: stateSchema, Data: []byte(`"` + string(r.Execution.ID) + `"`)}); err != nil {
		return nil, err
	}
	return resultHandle{e.Result}, nil
}

func (e *StateExecutor) Recover(ctx context.Context, r extension.RunRequest, state domain.Payload) (extension.RunHandle, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	e.mu.Lock()
	held := e.held[r.Execution.ID]
	e.mu.Unlock()
	if state.Schema != stateSchema || !held {
		return nil, extension.ErrNotAdoptable
	}
	if r.PersistRecovery == nil {
		return nil, extension.ErrInvalid
	}
	if err := r.PersistRecovery(state); err != nil {
		return nil, err
	}
	return resultHandle{e.Result}, nil
}

func (e *StateExecutor) Forget(ctx context.Context, id domain.ID) error {
	e.mu.Lock()
	delete(e.held, id)
	e.mu.Unlock()
	return ctx.Err()
}

// Held are the executions whose state the executor holds.
func (e *StateExecutor) Held() []domain.ID {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []domain.ID
	for id := range e.held {
		out = append(out, id)
	}
	slices.Sort(out)
	return out
}

func (e *StateExecutor) SweepState(ctx context.Context, known extension.ExecutionKnownFunc) (extension.StateSweepReport, error) {
	var report extension.StateSweepReport
	ids := e.Held()
	got, err := extension.KnownExecutions(ctx, known, ids)
	if err != nil {
		return report, err
	}
	for _, id := range ids {
		if !got[id] {
			if err := e.Forget(ctx, id); err != nil {
				return report, err
			}
			report.Removed = append(report.Removed, id)
		}
	}
	return report, nil
}

var _ interface {
	extension.Executor
	extension.ForgetExecutor
	extension.StateSweeper
} = (*StateExecutor)(nil)
