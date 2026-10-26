package reference

import (
	"context"
	"sync"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
)

// Executor models detached completion without exposing a PID or session to core.
type Executor struct{ Result extension.Result }
type resultHandle struct{ result extension.Result }

func (h resultHandle) Wait(ctx context.Context) (extension.Result, error) { return h.result, ctx.Err() }
func (e *Executor) Capabilities() domain.Capabilities {
	return domain.Capabilities{Toolbelt: extension.ToolbeltCLI}
}
func (e *Executor) Run(ctx context.Context, r extension.RunRequest) (extension.RunHandle, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if r.PersistRecovery == nil {
		return nil, extension.ErrInvalid
	}
	if err := r.PersistRecovery(domain.Payload{Schema: "fixture.adoption.v1", Data: []byte(`{"handle":"opaque"}`)}); err != nil {
		return nil, err
	}
	return resultHandle{e.Result}, nil
}
func (e *Executor) Recover(ctx context.Context, _ extension.RunRequest, state domain.Payload) (extension.RunHandle, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if state.Schema != "fixture.adoption.v1" || string(state.Data) != `{"handle":"opaque"}` {
		return nil, extension.ErrNotAdoptable
	}
	return resultHandle{e.Result}, nil
}

// InjectExecutor models the generic inject acknowledgement: a running attempt
// keeps an inbox, Consume takes in what is there (the process's own read),
// Finish ends the attempt and its result proves what was taken in. Results are
// durable for Recover. It is a volatile double: one attempt at a time.
type InjectExecutor struct {
	mu       sync.Mutex
	running  domain.ID
	inbox    []domain.ID
	taken    []domain.ID
	finished chan struct{}
	results  map[domain.ID]extension.Result
}

func (e *InjectExecutor) Capabilities() domain.Capabilities {
	return domain.Capabilities{Inject: true, Toolbelt: extension.ToolbeltCLI}
}

type injectHandle struct {
	e  *InjectExecutor
	id domain.ID
}

func (h injectHandle) Wait(ctx context.Context) (extension.Result, error) {
	h.e.mu.Lock()
	done := h.e.finished
	h.e.mu.Unlock()
	select {
	case <-done:
	case <-ctx.Done():
		return extension.Result{}, ctx.Err()
	}
	h.e.mu.Lock()
	defer h.e.mu.Unlock()
	return h.e.results[h.id], nil
}

func (e *InjectExecutor) Run(ctx context.Context, r extension.RunRequest) (extension.RunHandle, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if r.PersistRecovery == nil {
		return nil, extension.ErrInvalid
	}
	if err := r.PersistRecovery(domain.Payload{Schema: "fixture.inject.v1", Data: []byte(`{"handle":"inbox"}`)}); err != nil {
		return nil, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.running, e.inbox, e.taken, e.finished = r.Execution.ID, nil, nil, make(chan struct{})
	if e.results == nil {
		e.results = map[domain.ID]extension.Result{}
	}
	return injectHandle{e, r.Execution.ID}, nil
}

func (e *InjectExecutor) Recover(ctx context.Context, r extension.RunRequest, state domain.Payload) (extension.RunHandle, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, ok := e.results[r.Execution.ID]; !ok || state.Schema != "fixture.inject.v1" {
		return nil, extension.ErrNotAdoptable
	}
	return injectHandle{e, r.Execution.ID}, nil
}

func (e *InjectExecutor) Inject(_ context.Context, id domain.ID, ev domain.Event) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if id != e.running {
		return extension.ErrNotAccepting
	}
	for _, have := range append(append([]domain.ID(nil), e.inbox...), e.taken...) {
		if have == ev.ID {
			return nil
		}
	}
	e.inbox = append(e.inbox, ev.ID)
	return nil
}

// Consume is the running process reading its inbox.
func (e *InjectExecutor) Consume() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.taken = append(e.taken, e.inbox...)
	e.inbox = nil
}

// Finish ends the running attempt with a completed result proving what it
// took in.
func (e *InjectExecutor) Finish() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.results[e.running] = extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeCompleted}, ConsumedInputs: append([]domain.ID(nil), e.taken...)}
	e.running = ""
	close(e.finished)
}
