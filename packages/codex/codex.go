// Package codexext packages the Codex CLI executor (executors/codex) as an
// installable subprocess extension served over protocol version 1 through
// protocol/sdk, the way packages/shell serves the shell executor. Nothing
// in the daemon knows about Codex: a binding names executor "codex" once
// this package is installed (or bundled beside the binary), and the host
// binds it from the capabilities its manifest declares.
//
// Across the boundary the transcript and log are written through
// artifact.* (sdk.Artifacts), recovery metadata through
// execution.persist_recovery, the workspace through resource.*, and the
// segments and the Works' threads live in the extension's state directory.
// Segments are detached process groups, so they outlive the extension
// process, and a restarted extension adopts them through executor.recover.
package codexext

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"sync"
	"sync/atomic"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/executors/codex"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/protocol"
	"github.com/open-conduit/open-conduit/protocol/sdk"
	"github.com/open-conduit/open-conduit/storage"
)

// Name is the package's and the executor's registration name.
const Name = codex.Name

// Version is this package build's own version.
const Version = "0.1.0"

// Manifest declares the executor's capabilities (interrupt, inject,
// resume, workspace, the MCP toolbelt), its stop and forget controls, and
// that it says which workspace mode a binding requests.
func Manifest() protocol.Manifest {
	c := codex.Capabilities()
	return protocol.Manifest{Protocol: []int{protocol.Version}, Name: Name, Version: Version,
		Summary: "Runs agent turns on the Codex CLI, with the toolbelt over MCP.",
		Command: []string{"codex-extension"},
		Provides: protocol.Provides{Executors: []protocol.ExecutorDecl{{Name: Name,
			Capabilities: protocol.Capabilities{Interrupt: c.Interrupt, Inject: c.Inject, Resume: c.Resume, Workspace: c.Workspace, Toolbelt: c.Toolbelt},
			Controls:     protocol.Controls{Stop: true, Forget: true},
			ResourceMode: true}}}}
}

// executor creates the codex executor on first use, once the state
// directory (initialize) and the host (configure) are known.
type executor struct {
	stateDir func() string
	host     *atomic.Pointer[sdk.Host]
	once     sync.Once
	e        *codex.Executor
	err      error
}

func (x *executor) get() (*codex.Executor, error) {
	x.once.Do(func() {
		dir := x.stateDir()
		if dir == "" || x.host.Load() == nil {
			x.err = errors.New("codex extension: not initialized")
			return
		}
		x.e, x.err = codex.New(codex.Options{StateDir: filepath.Join(dir, "codex"), Artifacts: hostArtifacts{x.host}})
	})
	return x.e, x.err
}

// hostArtifacts is sdk.Artifacts over the connection's host.
type hostArtifacts struct{ host *atomic.Pointer[sdk.Host] }

func (a hostArtifacts) Create(ctx context.Context, media string) (storage.ArtifactWriter, error) {
	return sdk.NewArtifacts(a.host.Load()).Create(ctx, media)
}
func (a hostArtifacts) Open(ctx context.Context, ref storage.ArtifactRef) (io.ReadCloser, error) {
	return sdk.NewArtifacts(a.host.Load()).Open(ctx, ref)
}
func (a hostArtifacts) Delete(ctx context.Context, ref storage.ArtifactRef) error {
	return sdk.NewArtifacts(a.host.Load()).Delete(ctx, ref)
}

func (x *executor) Capabilities() domain.Capabilities { return codex.Capabilities() }
func (x *executor) ResourceMode(raw json.RawMessage) string {
	return codex.ResourceMode(raw)
}
func (x *executor) Run(ctx context.Context, r extension.RunRequest) (extension.RunHandle, error) {
	e, err := x.get()
	if err != nil {
		return nil, err
	}
	return e.Run(ctx, r)
}
func (x *executor) Resume(ctx context.Context, r extension.RunRequest, state domain.Payload) (extension.RunHandle, error) {
	e, err := x.get()
	if err != nil {
		return nil, err
	}
	return e.Resume(ctx, r, state)
}
func (x *executor) Recover(ctx context.Context, r extension.RunRequest, state domain.Payload) (extension.RunHandle, error) {
	e, err := x.get()
	if err != nil {
		return nil, err
	}
	return e.Recover(ctx, r, state)
}
func (x *executor) Interrupt(ctx context.Context, id domain.ID) error {
	e, err := x.get()
	if err != nil {
		return err
	}
	return e.Interrupt(ctx, id)
}
func (x *executor) Inject(ctx context.Context, id domain.ID, ev domain.Event) error {
	e, err := x.get()
	if err != nil {
		return err
	}
	return e.Inject(ctx, id, ev)
}
func (x *executor) Stop(ctx context.Context, id domain.ID, epoch uint64) error {
	e, err := x.get()
	if err != nil {
		return err
	}
	return e.Stop(ctx, id, epoch)
}
func (x *executor) Forget(ctx context.Context, id domain.ID) error {
	e, err := x.get()
	if err != nil {
		return err
	}
	return e.Forget(ctx, id)
}

// Build returns the served extension. stateDir returns the private state
// directory the host gave at initialize.
func Build(stateDir func() string) *sdk.Extension {
	host := &atomic.Pointer[sdk.Host]{}
	x := &executor{stateDir: stateDir, host: host}
	return &sdk.Extension{Manifest: Manifest(),
		Configure: func(_ context.Context, h *sdk.Host, _ protocol.ConfigureParams) (protocol.ConfigureResult, error) {
			host.Store(h)
			return protocol.ConfigureResult{}, nil
		},
		Validate: func(_ context.Context, p protocol.ValidateParams) error {
			if p.Kind != protocol.ValidateExecutor {
				return nil
			}
			return codex.ValidateConfig(p.Config, p.SecretNames)
		},
		Executors: map[string]extension.Executor{Name: x}}
}

var _ interface {
	extension.Executor
	extension.InterruptExecutor
	extension.InjectExecutor
	extension.ResumeExecutor
	extension.StopExecutor
	extension.ForgetExecutor
	extension.ResourceModer
} = (*executor)(nil)
