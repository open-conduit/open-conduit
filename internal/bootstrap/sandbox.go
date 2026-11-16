package bootstrap

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/open-conduit/open-conduit/internal/config"
	"github.com/open-conduit/open-conduit/protocol"

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
	// The processes packages ran for their entries ran under the last
	// daemon's packages, which have gone with it.
	_ = os.RemoveAll(d.entriesDir())
}

// entriesDir holds, per package, the directories of the processes it runs
// for its entries in a sandbox (entrySandbox) and their plans' records:
// <package>/runs/<dir> and <package>/records.
func (d *daemon) entriesDir() string { return filepath.Join(d.o.StateDir, "entries") }

// entrySandbox is sandbox.entry_plan for package m: a launch of one of
// its own processes for its entry in a scope, planned with the provider
// and policy that entry's sandbox names, with the host's protected set
// denied, in a fresh directory beneath the package's runs. Only an entry
// of a type m declares a sandbox for, in a scope where it is in effect,
// has one; nothing else may run.
func (d *daemon) entrySandbox(m protocol.Manifest) extension.EntrySandbox {
	types := map[string]bool{}
	for _, x := range m.Provides.Extensions {
		if x.Sandbox != "" {
			types[x.Type] = true
		}
	}
	return func(ctx context.Context, scope domain.ID, entry string, needs extension.SandboxNeeds, launch extension.SandboxLaunch) (extension.SandboxPlan, string, error) {
		var desc *config.Sandbox
		if s, _ := d.extensionSnapshot(); s != nil {
			if sc := s.Scopes[scope]; sc != nil && sc.Enabled {
				for _, e := range sc.Extensions {
					if e.Name == entry && types[e.Type] {
						desc = e.Sandbox
					}
				}
			}
		}
		if desc == nil {
			return extension.SandboxPlan{}, "", fmt.Errorf("%w: entry %s names no sandbox in scope %s", extension.ErrSandboxUnavailable, entry, scope)
		}
		p := d.sandboxes.providers[desc.Provider]
		if p == nil {
			return extension.SandboxPlan{}, "", fmt.Errorf("%w: sandbox provider %s", extension.ErrSandboxUnavailable, desc.Provider)
		}
		root := filepath.Join(d.entriesDir(), m.Name)
		runs := filepath.Join(root, "runs")
		if err := os.MkdirAll(runs, 0o700); err != nil {
			return extension.SandboxPlan{}, "", fmt.Errorf("%w: %v", extension.ErrSandboxUnavailable, err)
		}
		dir, err := os.MkdirTemp(runs, entry+"-")
		if err != nil {
			return extension.SandboxPlan{}, "", fmt.Errorf("%w: %v", extension.ErrSandboxUnavailable, err)
		}
		b := extension.BindSandbox(extension.SandboxBinding{
			Provider:  p,
			Owner:     extension.ResourceOwner{ScopeID: scope},
			Config:    domain.Payload{Schema: SandboxConfigSchema, Data: append([]byte(nil), desc.Config...)},
			Protected: d.protectedPaths(),
			RunRoot:   runs,
			Records:   filepath.Join(root, "records"),
			// An entry's process is no attempt's segment: its plan is
			// kept in the records and logged, and never adopted, since
			// it ends with the package that launched it.
			Pin: func(plan extension.SandboxPlan) error {
				d.log.Info("entry process sandboxed", "extension", m.Name, "scope", scope, "entry", entry, "provider", desc.Provider, "digest", plan.Digest, "summary", plan.Summary)
				return nil
			},
		})
		launch.Dir, launch.RunDir = dir, dir
		plan, err := b.Plan(ctx, needs, launch)
		if err != nil {
			_ = os.RemoveAll(dir)
			return extension.SandboxPlan{}, "", err
		}
		return plan, dir, nil
	}
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
