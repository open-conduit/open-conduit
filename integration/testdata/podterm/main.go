// Command podterm-extension is a third-party container provider and executor
// for tests. It uses only the public contract: its attach container's Argv
// is an ordinary command, which the host runs under a terminal for an
// operator. Nothing in core knows about it.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/protocol"
	"github.com/open-conduit/open-conduit/protocol/sdk"
)

const planSchema = "podterm.plan.v1"

// session is what an attach container "runs": it proves the terminal is a
// real one, then echoes one line of the operator's input.
const session = `test -t 0 && printf 'pod-tty\n'; stty -echo; read line; printf 'pod-got:%s\n' "$line"`

type pod struct {
	mu   sync.Mutex
	next int
	made map[string]extension.ContainerState
}

func (*pod) Plan(_ context.Context, _ extension.ResourceOwner, policy extension.ContainerPolicy, _ extension.SandboxNeeds, l extension.SandboxLaunch) (extension.ContainerPlan, error) {
	c, err := extension.ParseContainerConfig(policy.Config.Data)
	if err != nil {
		return extension.ContainerPlan{}, fmt.Errorf("%w: %v", extension.ErrSandboxUnavailable, err)
	}
	data, _ := json.Marshal(map[string]any{"tty": l.TTY})
	return extension.ContainerPlan{Image: c.Image, Summary: "podterm", Data: domain.Payload{Schema: planSchema, Data: data}}, nil
}

func (p *pod) Create(_ context.Context, plan extension.ContainerPlan, labels map[string]string) (extension.ContainerCreated, error) {
	var d struct {
		TTY bool `json:"tty"`
	}
	if err := json.Unmarshal(plan.Data.Data, &d); err != nil {
		return extension.ContainerCreated{}, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.next++
	ref := extension.ContainerRef{ID: "podterm-" + strconv.Itoa(p.next), Labels: labels}
	p.made[ref.ID] = extension.ContainerState{Ref: ref, Daemon: "podterm", Image: plan.Image}
	argv := []string{"/usr/bin/true"}
	if d.TTY {
		argv = []string{"/bin/sh", "-c", session}
	}
	return extension.ContainerCreated{Ref: ref, Daemon: "podterm", Argv: argv}, nil
}

func (*pod) Signal(context.Context, extension.ContainerRef, string) error { return nil }

func (*pod) Stop(context.Context, extension.ContainerRef, time.Duration) error { return nil }

func (p *pod) Inspect(_ context.Context, ref extension.ContainerRef) (extension.ContainerState, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if st, ok := p.made[ref.ID]; ok {
		return st, nil
	}
	return extension.ContainerState{Ref: ref, Daemon: "podterm", Gone: true}, nil
}

func (p *pod) List(_ context.Context, owner extension.ResourceOwner) ([]extension.ContainerState, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []extension.ContainerState
	for _, st := range p.made {
		if owner.ExecutionID == "" || st.Ref.Labels[extension.ContainerLabelExecution] == string(owner.ExecutionID) {
			out = append(out, st)
		}
	}
	return out, nil
}

func (p *pod) Remove(_ context.Context, ref extension.ContainerRef) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.made, ref.ID)
	return nil
}

type handle struct{}

func (handle) Wait(context.Context) (extension.Result, error) {
	return extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeHandoff}}, nil
}

type executor struct{}

func (executor) Capabilities() domain.Capabilities {
	return domain.Capabilities{Toolbelt: extension.ToolbeltNone, Container: true}
}

func (executor) Run(ctx context.Context, r extension.RunRequest) (extension.RunHandle, error) {
	if r.Context.Container == nil {
		return nil, fmt.Errorf("%w: no container", extension.ErrSandboxUnavailable)
	}
	launch, err := launchIn(ctx, "run-"+string(r.Execution.ID), false)
	if err != nil {
		return nil, err
	}
	if err = r.PersistRecovery(domain.Payload{Schema: "podterm.recovery.v1", Data: []byte(`{}`)}); err != nil {
		return nil, err
	}
	if _, err := r.Context.Container.Launch(ctx, extension.SandboxNeeds{}, launch); err != nil {
		return nil, err
	}
	return handle{}, nil
}

func (executor) Recover(context.Context, extension.RunRequest, domain.Payload) (extension.RunHandle, error) {
	return handle{}, nil
}

func (executor) Attach(_ context.Context, work domain.ID) (extension.AttachSpec, error) {
	return extension.AttachSpec{Argv: []string{"open-conduit", "container", "attach", string(work)}}, nil
}

func (executor) ContainerAttach(ctx context.Context, r extension.ContainerAttachRequest) (extension.ContainerAttachSpec, error) {
	launch, err := launchIn(ctx, "attach-"+string(r.Work.ID), true)
	if err != nil {
		return extension.ContainerAttachSpec{}, err
	}
	return extension.ContainerAttachSpec{Launch: launch}, nil
}

// launchIn is a launch in a run directory of its own under the executor's
// state directory, where the host's launch guard expects one.
func launchIn(ctx context.Context, name string, tty bool) (extension.SandboxLaunch, error) {
	h := sdk.HostFrom(ctx)
	if h == nil {
		return extension.SandboxLaunch{}, extension.ErrUnsupported
	}
	sum := sha256.Sum256([]byte(name))
	dir := filepath.Join(h.ExecutorDir("podterm"), "runs", hex.EncodeToString(sum[:8]))
	work := filepath.Join(dir, "workspace")
	if err := os.MkdirAll(work, 0o700); err != nil {
		return extension.SandboxLaunch{}, err
	}
	return extension.SandboxLaunch{Argv: []string{"agent"}, Dir: work, RunDir: dir, TTY: tty}, nil
}

func main() {
	ext := &sdk.Extension{Manifest: protocol.Manifest{Protocol: []int{protocol.Version}, Name: "podterm", Version: "0.1.0", Command: []string{"podterm-extension"},
		Provides: protocol.Provides{
			Containers: []protocol.ContainerDecl{{Type: "podterm"}},
			Executors:  []protocol.ExecutorDecl{{Name: "podterm", Capabilities: protocol.Capabilities{Toolbelt: extension.ToolbeltNone, Container: true}, Controls: protocol.Controls{Attach: true, ContainerAttach: true}}},
		}},
		Containers: map[string]extension.ContainerProvider{"podterm": &pod{made: map[string]extension.ContainerState{}}},
		Executors:  map[string]extension.Executor{"podterm": executor{}}}
	if sdk.PackageCommand(os.Args[1:], ext.Manifest, nil, os.Stdout) {
		return
	}
	out := os.Stdout
	os.Stdout = os.Stderr
	<-sdk.NewServer(ext, os.Stdin, out).Done()
}
