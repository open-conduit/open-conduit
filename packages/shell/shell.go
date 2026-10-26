// Package shellext packages the first-party shell executor (executors/shell,
// unchanged) as an installable subprocess extension (07-03): detached
// process groups with file-backed output, recovery metadata before launch,
// adoption across restarts, interrupt, inject through the inbox, verified
// stop, forget and artifacts, served over protocol version 1 through
// protocol/sdk. With the Slack package it is the second, materially
// different first-party workload round-tripped through the subprocess
// boundary. The daemon keeps registering the shell executor in-process, so
// this package cannot be installed beside it (the names conflict).
//
// What changes across the boundary: artifacts are written through
// artifact.* (sdk.Artifacts), recovery metadata through
// execution.persist_recovery, the workspace through resource.*, and the
// executor's state lives in the extension's state directory. Its detached
// processes survive the extension process exactly as they survive the
// daemon, and a restarted extension adopts them through executor.recover.
package shellext

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"sync"
	"sync/atomic"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/executors/shell"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/protocol"
	"github.com/open-conduit/open-conduit/protocol/sdk"
	"github.com/open-conduit/open-conduit/storage"
)

// Name is the package's registration name: the first-party executor name.
const Name = "shell"

// Version is this package build's own version.
const Version = "0.1.0"

// Manifest is the package manifest: the in-process executor's capabilities
// (resume off, as the daemon registers it) and its stop and forget controls.
func Manifest() protocol.Manifest {
	return protocol.Manifest{Protocol: []int{protocol.Version}, Name: Name, Version: Version,
		Summary: "The first-party shell executor as a subprocess package.",
		Command: []string{"shell-extension"},
		Provides: protocol.Provides{Executors: []protocol.ExecutorDecl{{Name: Name,
			Capabilities: protocol.Capabilities{Interrupt: true, Inject: true, Workspace: true, Toolbelt: extension.ToolbeltCLI},
			Controls:     protocol.Controls{Stop: true, Forget: true}}}}}
}

// executor creates the shell executor on first use, once the state
// directory (initialize) and the host (configure) are known.
type executor struct {
	stateDir func() string
	host     *atomic.Pointer[sdk.Host]
	once     sync.Once
	e        *shell.Executor
	err      error
}

func (x *executor) get() (*shell.Executor, error) {
	x.once.Do(func() {
		dir := x.stateDir()
		if dir == "" || x.host.Load() == nil {
			x.err = errors.New("shell extension: not initialized")
			return
		}
		x.e, x.err = shell.New(shell.Options{StateDir: filepath.Join(dir, "shell"), Artifacts: hostArtifacts{x.host}})
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

func (x *executor) Capabilities() domain.Capabilities {
	return domain.Capabilities{Interrupt: true, Inject: true, Toolbelt: extension.ToolbeltCLI, Workspace: true}
}
func (x *executor) Run(ctx context.Context, r extension.RunRequest) (extension.RunHandle, error) {
	e, err := x.get()
	if err != nil {
		return nil, err
	}
	return e.Run(ctx, r)
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
			return (&shell.Executor{}).ValidateConfig(p.Config, p.SecretNames)
		},
		Executors: map[string]extension.Executor{Name: x}}
}

// Certification is the package's certification.json: a static fixture.
// The executor case finishes at once; the inject case waits for the
// runner's finish file, then acknowledges everything in its inbox.
func Certification() protocol.CertificationFile {
	script := `while [ ! -e '{tmp}/inject-go' ]; do sleep 0.02; done; for f in "$OS_INBOX"/*.json; do [ -e "$f" ] || continue; d=${f##*-}; d=${d%.json}; : > "$OS_INBOX/consumed/$d"; done`
	run, _ := json.Marshal(map[string]string{"command": "printf certified"})
	inject, _ := json.Marshal(map[string]string{"command": script})
	return protocol.CertificationFile{Fixture: &protocol.Fixture{
		Executor: &protocol.ExecutorCase{Executor: Name, Config: protocol.Payload{Schema: "shell.config.v1", Data: run}},
		Inject:   &protocol.InjectCase{Executor: Name, Config: protocol.Payload{Schema: "shell.config.v1", Data: inject}, FinishFile: "{tmp}/inject-go"},
	}}
}

var _ interface {
	extension.Executor
	extension.InterruptExecutor
	extension.InjectExecutor
	extension.StopExecutor
	extension.ForgetExecutor
} = (*executor)(nil)
