package bootstrap

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/internal/runtime"
)

// SandboxConfigSchema labels the opaque binding sandbox config handed to a
// provider.
const SandboxConfigSchema = "core.config.sandbox.v1"

// sandboxState is the host side of the process sandbox providers (B09):
// the packages' declared ones (manifest sandboxes), by type.
type sandboxState struct {
	providers map[string]extension.SandboxProvider
}

func (d *daemon) openSandboxes() {
	d.sandboxes.providers = map[string]extension.SandboxProvider{}
}

// protectedPaths are what no sandboxed process may read or write, whatever
// its policy grants: the state directory (the operator token, the
// management socket, the database, every other execution's run directory
// and workspace), the fallback toolbelt socket root and the config file.
func (d *daemon) protectedPaths() []string {
	out := []string{d.o.StateDir}
	if d.endpoints != nil {
		out = append(out, d.endpoints.ShortRoot())
	}
	if d.o.ConfigPath != "" {
		out = append(out, d.o.ConfigPath)
	}
	return out
}

// bindSandbox gives an attempt its binding's sandbox, bound to the attempt's
// trusted owner, this execution's own toolbelt and run directories, and the
// host's protected set. A binding without one gets none: a segment launched
// in a sandbox before its binding dropped it is adopted as it runs, which
// is no wider than the binding now allows.
func (d *daemon) bindSandbox(ctx context.Context, state runtime.AttemptState, resources extension.ResourceBroker, pin func(extension.SandboxPlan) error) (extension.SandboxBroker, extension.ResourceBroker, error) {
	desc, err := d.configs.Sandbox(state)
	if err != nil {
		return nil, nil, err
	}
	if desc == nil {
		return nil, resources, nil
	}
	if !state.Capabilities.Sandbox {
		return nil, nil, fmt.Errorf("%w: executor %s can't run in a sandbox", extension.ErrSandboxUnavailable, state.Executor)
	}
	p := d.sandboxes.providers[desc.Provider]
	if p == nil {
		return nil, nil, fmt.Errorf("%w: sandbox provider %s", extension.ErrSandboxUnavailable, desc.Provider)
	}
	run := d.endpoints.Dir(state.ExecutionID)
	shared := []string{run, d.pluginDir()}
	// The stable link to `os` is first on a run's PATH, so the executor
	// asks to read it; it holds nothing but that link.
	if d.o.ToolbeltDir != "" {
		shared = append(shared, d.o.ToolbeltDir)
	}
	socket := d.endpoints.Socket(state.ExecutionID)
	if socket != "" {
		shared = append(shared, filepath.Dir(socket))
	}
	var pinned string
	if pin := state.SandboxPin(); pin != nil && state.LaunchState == "recovering" {
		pinned = pin.Digest
	}
	b := extension.BindSandbox(extension.SandboxBinding{
		Provider:  p,
		Owner:     extension.ResourceOwner{ScopeID: state.ScopeID, WorkID: state.WorkID, ExecutionID: state.ExecutionID},
		Config:    domain.Payload{Schema: SandboxConfigSchema, Data: append([]byte(nil), desc.Config...)},
		Protected: d.protectedPaths(),
		Shared:    shared,
		RunRoot:   filepath.Join(d.o.StateDir, "executors", state.Executor, "runs"),
		Toolbelt:  socket,
		Records:   filepath.Join(run, "sandbox"),
		Pin:       pin,
		Pinned:    pinned,
		Phase:     runtime.LaunchPhases(ctx),
	})
	return b, b.Resources(resources), nil
}
