package bootstrap

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/extensions/srt"
	"github.com/open-conduit/open-conduit/internal/config"
	"github.com/open-conduit/open-conduit/internal/runtime"
)

// SandboxConfigSchema labels the opaque binding sandbox config handed to a
// provider.
const SandboxConfigSchema = "core.config.sandbox.v1"

func sandboxRegistry(r *config.Registry) {
	r.Sandboxes = map[string]func(json.RawMessage) error{srt.Type: srt.ValidateConfig}
}

// sandboxState is the host side of the process sandbox providers (B09).
type sandboxState struct {
	providers map[string]extension.SandboxProvider
	srt       *srt.Provider
}

func (d *daemon) openSandboxes() {
	d.sandboxes.srt = srt.New(srt.Options{})
	d.sandboxes.providers = map[string]extension.SandboxProvider{srt.Type: d.sandboxes.srt}
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
func (d *daemon) bindSandbox(_ context.Context, state runtime.AttemptState, resources extension.ResourceBroker, pin func(extension.SandboxPlan) error) (extension.SandboxBroker, extension.ResourceBroker, error) {
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
	})
	return b, b.Resources(resources), nil
}

// sandboxCheck is doctor's view of the sandbox providers the config uses:
// for srt, that it is present at a known version and that the kernel lets
// it isolate (on Linux, user namespaces and seccomp), by running a command
// under an empty policy.
func (d *daemon) sandboxCheck(ctx context.Context) runtime.Check {
	snapshot, _ := d.configs.Current()
	check := runtime.Check{Name: "sandbox"}
	var bindings []string
	if snapshot != nil {
		for _, id := range snapshot.Order {
			scope := snapshot.Scopes[id]
			if scope == nil || !scope.Enabled {
				continue
			}
			for _, b := range scope.Bindings {
				if b.Sandbox != nil && b.Sandbox.Provider == srt.Type {
					bindings = append(bindings, string(id)+"/"+b.Name)
				}
			}
		}
	}
	if len(bindings) == 0 {
		check.Status, check.Code, check.Detail = runtime.CheckInfo, "not_configured", "no binding declares a sandbox"
		return check
	}
	sort.Strings(bindings)
	h := d.sandboxes.srt.Health(ctx)
	check.Code, check.Detail, check.Items = h.Code, h.Detail, h.Items
	switch h.Status {
	case "ok":
		check.Status = runtime.CheckOK
	case "warn":
		check.Status = runtime.CheckWarn
	default:
		check.Status = runtime.CheckFail
		check.Detail += "; sandboxed bindings run nothing until this is fixed"
	}
	check.Items = append(check.Items, "sandboxed: "+strings.Join(bindings, ", "))
	return check
}
