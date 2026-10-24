package reference

import (
	"context"

	"github.com/alsiola/open-conduit/domain"
	"github.com/alsiola/open-conduit/extension"
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
