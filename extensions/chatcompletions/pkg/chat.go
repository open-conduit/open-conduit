// Package chatcompletionsext packages the Chat Completions executor
// (extensions/chatcompletions) as an installable subprocess extension
// served over protocol version 1 through protocol/sdk. Nothing in the
// daemon knows about it: a binding names executor chat-completions once
// this package is installed (or bundled beside the binary), and the host
// binds it from the capabilities its manifest declares.
//
// Across the boundary the transcript is written through artifact.*
// (sdk.Artifacts), recovery metadata through execution.persist_recovery,
// and the Works' conversations and the attempts' results live in the
// extension's state directory. An attempt is a loop in this process, so
// it ends with the process: a restarted extension returns a recorded
// result through executor.recover, and the runtime's next attempt carries
// the Work's conversation on.
package chatcompletionsext

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/extensions/chatcompletions"
	"github.com/open-conduit/open-conduit/protocol"
	"github.com/open-conduit/open-conduit/protocol/sdk"
	"github.com/open-conduit/open-conduit/storage"
)

// Name is the package's and the executor's registration name.
const Name = chatcompletions.Name

// Version is this package build's own version.
const Version = "0.1.0"

// Manifest declares the executor's capabilities (interrupt, inject,
// resume and the toolbelt it calls itself), its stop and forget controls,
// and that it says which playbook a binding starts on.
func Manifest() protocol.Manifest {
	c := chatcompletions.Capabilities()
	return protocol.Manifest{Protocol: []int{protocol.Version}, Name: Name, Version: Version, Title: "Chat Completions",
		Summary: "Runs toolbelt-only turns as a Chat Completions tool loop against an OpenAI-compatible endpoint.",
		Command: protocol.HostRunCommand(Name),
		Provides: protocol.Provides{Executors: []protocol.ExecutorDecl{{Name: Name,
			Capabilities: protocol.Capabilities{Interrupt: c.Interrupt, Inject: c.Inject, Resume: c.Resume, Workspace: c.Workspace, Toolbelt: c.Toolbelt},
			Controls:     protocol.Controls{Stop: true, Forget: true},
			Playbook:     true, Fields: chatcompletions.BindingFields()}}}}
}

// executor creates the executor on first use, once the state directory
// (initialize) and the host (configure) are known.
type executor struct {
	stateDir func() string
	host     *atomic.Pointer[sdk.Host]
	once     sync.Once
	e        *chatcompletions.Executor
	err      error
}

func (x *executor) get() (*chatcompletions.Executor, error) {
	x.once.Do(func() {
		dir := x.stateDir()
		if dir == "" || x.host.Load() == nil {
			x.err = errors.New("chat-completions extension: not initialized")
			return
		}
		x.e, x.err = chatcompletions.New(chatcompletions.Options{StateDir: filepath.Join(dir, "chat-completions"), Artifacts: hostArtifacts{x.host}})
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

func (x *executor) Capabilities() domain.Capabilities { return chatcompletions.Capabilities() }
func (x *executor) Playbook(raw json.RawMessage) string {
	return chatcompletions.Playbook(raw)
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
			return chatcompletions.ValidateConfig(p.Config, p.SecretNames)
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
	extension.PlaybookNamer
} = (*executor)(nil)

// Main serves the package over protocol version 1 on stdin and stdout, as
// `open-conduit extension-run chat-completions` does. Its subcommands:
// package-manifest and package-certification print the package files, and
// certify-fixture serves the conformance runner's fixture and the fake
// endpoint it names. It returns the process's exit status.
func Main(args []string) int {
	if sdk.PackageCommand(args, Manifest(), Certification(), os.Stdout) {
		return 0
	}
	if len(args) > 0 && args[0] == "certify-fixture" {
		if err := ServeFixture(os.Stdin, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		return 0
	}
	out := os.Stdout
	os.Stdout = os.Stderr
	var server atomic.Pointer[sdk.Server]
	ext := Build(func() string {
		if s := server.Load(); s != nil {
			return s.StateDir()
		}
		return ""
	})
	s := sdk.NewServer(ext, os.Stdin, out)
	server.Store(s)
	<-s.Done()
	return 0
}
