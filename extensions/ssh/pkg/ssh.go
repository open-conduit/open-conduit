// Package sshext serves the ssh container provider (extensions/ssh) as a
// package over protocol version 1, through protocol/sdk (#477): the host
// plans, creates, signals, stops, inspects, lists and removes a binding's
// runs on a remote through container_provider.*, and checks a binding's
// container config through validate. Nothing in the daemon knows about
// ssh: a binding names container provider "ssh" once this package is
// installed (or bundled beside the binary). open-conduit serves it as
// `open-conduit extension-run ssh`, and an executor's wrapper starts a
// created run with its `run` subcommand.
//
// The provider pins each remote's host key in the directory the host names
// for its type (<state>/containers/ssh). Out of process it has only the
// sanitized environment and the variables the operator grants it
// (settings.extensions.ssh.pass_env), so an ssh agent's SSH_AUTH_SOCK must
// be named there; a binding's pass_env, which reaches the run, never
// reaches it.
package sshext

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/extensions/ssh"
	"github.com/open-conduit/open-conduit/protocol"
	"github.com/open-conduit/open-conduit/protocol/sdk"
)

// Name is the package's registration name: the provider's.
const Name = ssh.Type

// Version is this package build's own version.
const Version = "0.1.0"

// Manifest declares the ssh container provider and its doctor check.
func Manifest() protocol.Manifest {
	return protocol.Manifest{Protocol: []int{protocol.Version}, Name: Name, Version: Version, Title: "Remote runs over SSH",
		Summary: "Runs a container binding's launches on another machine over SSH, pinned by its host key, with the launch's granted paths copied there and back.",
		Command: protocol.HostRunCommand(Name),
		Provides: protocol.Provides{
			Containers: []protocol.ContainerDecl{{Type: ssh.Type}},
			Checks:     []protocol.CheckDecl{{Name: Name}},
		}}
}

// provider creates the ssh provider on first use, once the host has named
// its directory and label (initialize), and reaches the remotes the last
// configure's bindings use.
type provider struct {
	once sync.Once
	p    *ssh.Provider
	dir  string
	err  error

	mu      sync.Mutex
	remotes []ssh.Settings
}

func (x *provider) get(ctx context.Context) (*ssh.Provider, error) {
	x.once.Do(func() {
		h := sdk.HostFrom(ctx)
		if h == nil || h.ContainerDir(ssh.Type) == "" || h.ContainerHost() == "" {
			x.err = errors.New("ssh extension: the host named no container directory or label")
			return
		}
		self, err := sdk.Self()
		if err != nil {
			x.err = err
			return
		}
		x.dir = h.ContainerDir(ssh.Type)
		x.p = ssh.New(ssh.Options{Helper: append(self, "run"), Host: h.ContainerHost(), Dir: x.dir, Remotes: x.configured})
	})
	return x.p, x.err
}

// configured are the remotes the last configure's bindings use.
func (x *provider) configured() []ssh.Settings {
	x.mu.Lock()
	defer x.mu.Unlock()
	return slices.Clone(x.remotes)
}

func (x *provider) configure(uses []protocol.ContainerUse) {
	var remotes []ssh.Settings
	for _, u := range uses {
		if u.Provider != ssh.Type {
			continue
		}
		if _, s, err := ssh.ParseConfig(u.Config); err == nil {
			remotes = append(remotes, s)
		}
	}
	x.mu.Lock()
	x.remotes = remotes
	x.mu.Unlock()
}

func (x *provider) Plan(ctx context.Context, owner extension.ResourceOwner, policy extension.ContainerPolicy, needs extension.SandboxNeeds, l extension.SandboxLaunch) (extension.ContainerPlan, error) {
	p, err := x.get(ctx)
	if err != nil {
		return extension.ContainerPlan{}, fmt.Errorf("%w: %v", extension.ErrSandboxUnavailable, err)
	}
	return p.Plan(ctx, owner, policy, needs, l)
}

func (x *provider) Create(ctx context.Context, plan extension.ContainerPlan, labels map[string]string) (extension.ContainerCreated, error) {
	p, err := x.get(ctx)
	if err != nil {
		return extension.ContainerCreated{}, fmt.Errorf("%w: %v", extension.ErrSandboxUnavailable, err)
	}
	return p.Create(ctx, plan, labels)
}

func (x *provider) Signal(ctx context.Context, ref extension.ContainerRef, signal string) error {
	p, err := x.get(ctx)
	if err != nil {
		return err
	}
	return p.Signal(ctx, ref, signal)
}

func (x *provider) Stop(ctx context.Context, ref extension.ContainerRef, grace time.Duration) error {
	p, err := x.get(ctx)
	if err != nil {
		return err
	}
	return p.Stop(ctx, ref, grace)
}

func (x *provider) Inspect(ctx context.Context, ref extension.ContainerRef) (extension.ContainerState, error) {
	p, err := x.get(ctx)
	if err != nil {
		return extension.ContainerState{}, err
	}
	return p.Inspect(ctx, ref)
}

func (x *provider) List(ctx context.Context, owner extension.ResourceOwner) ([]extension.ContainerState, error) {
	p, err := x.get(ctx)
	if err != nil {
		return nil, err
	}
	return p.List(ctx, owner)
}

func (x *provider) Remove(ctx context.Context, ref extension.ContainerRef) error {
	p, err := x.get(ctx)
	if err != nil {
		return err
	}
	return p.Remove(ctx, ref)
}

var _ extension.ContainerProvider = (*provider)(nil)

// Build returns the served extension.
func Build() *sdk.Extension {
	x := &provider{}
	return &sdk.Extension{Manifest: Manifest(),
		Configure: func(_ context.Context, _ *sdk.Host, p protocol.ConfigureParams) (protocol.ConfigureResult, error) {
			x.configure(p.Containers)
			return protocol.ConfigureResult{}, nil
		},
		Validate: func(_ context.Context, v protocol.ValidateParams) error {
			if v.Kind != protocol.ValidateContainer || v.Type != ssh.Type {
				return nil
			}
			return ssh.ValidateConfig(v.Config)
		},
		Containers: map[string]extension.ContainerProvider{ssh.Type: x},
		CheckRuns: map[string]func(context.Context, *sdk.Host, protocol.CheckRunParams) (protocol.CheckRunResult, error){
			Name: func(ctx context.Context, _ *sdk.Host, c protocol.CheckRunParams) (protocol.CheckRunResult, error) {
				return x.check(ctx, c.Config), nil
			},
		}}
}

// check is doctor's view of each remote the config's ssh bindings use:
// reachable with its pinned host key, with what a run needs, and able to
// write a host path mirrored there (the provider's own directory, under
// the state directory whose workspaces, run directories, homes and
// toolbelt tokens a run mirrors at the same paths).
func (x *provider) check(ctx context.Context, cfg protocol.CheckConfig) protocol.CheckRunResult {
	type use struct {
		name     string
		settings ssh.Settings
	}
	var uses []use
	for _, s := range cfg.Scopes {
		for _, b := range s.Bindings {
			if b.Container == nil || b.Container.Provider != ssh.Type {
				continue
			}
			if _, st, err := ssh.ParseConfig(b.Container.Config); err == nil {
				uses = append(uses, use{s.ID + "/" + b.Name, st})
			}
		}
	}
	if len(uses) == 0 {
		return protocol.CheckRunResult{OK: true, Status: "info", Code: "not_configured", Detail: "no binding runs on a remote over ssh"}
	}
	p, err := x.get(ctx)
	if err != nil {
		return protocol.CheckRunResult{Status: "fail", Code: "ssh_unavailable", Detail: err.Error()}
	}
	out := protocol.CheckRunResult{OK: true, Status: "ok", Code: "ok"}
	seen := map[string]bool{}
	var details, names []string
	for _, u := range uses {
		names = append(names, u.name)
		key := u.settings.Endpoint() + "\x00" + u.settings.HostKey
		if seen[key] {
			continue
		}
		seen[key] = true
		h := p.Health(ctx, u.settings, x.dir, false)
		out.Items = append(out.Items, h.Items...)
		if h.Status != "ok" && out.OK {
			out.OK, out.Status, out.Code = false, "fail", h.Code
		}
		details = append(details, h.Detail)
	}
	out.Detail = strings.Join(details, "; ")
	if !out.OK {
		out.Detail += "; ssh bindings run nothing until this is fixed"
	}
	out.Items = append(out.Items, "on remotes: "+strings.Join(names, ", "))
	return out
}

// Certification is the package's certification.json: a static fixture of
// container configs, one ssh accepts and ones it refuses (an image that
// doesn't pin the host key, and a network it can't hold).
func Certification() protocol.CertificationFile {
	key := "ssh-ed25519 " + base64.StdEncoding.EncodeToString([]byte("open-conduit certification host key"))
	digest, _ := ssh.HostKeyDigest(key)
	settings := func(network string) string {
		return `{"host":"runner.test","host_key":"` + key + `","network":"` + network + `"}`
	}
	pinned := "runner.test@sha256:" + digest
	return protocol.CertificationFile{Fixture: &protocol.Fixture{Containers: []protocol.ContainerCase{
		{Type: ssh.Type, Config: json.RawMessage(`{"image":"` + pinned + `","write":["{workspace}"],"provider":` + settings("host") + `}`)},
		{Type: ssh.Type, Config: json.RawMessage(`{"image":"runner.test@sha256:` + strings.Repeat("0", 64) + `","provider":` + settings("host") + `}`), Refused: true},
		{Type: ssh.Type, Config: json.RawMessage(`{"image":"` + pinned + `","provider":` + settings("none") + `}`), Refused: true},
	}}}
}

// Main serves the package over protocol version 1 on stdin and stdout, as
// `open-conduit extension-run ssh` does. Its subcommands package-manifest
// and package-certification print the package files, and `run --records
// <dir> <ref>` is what an executor's wrapper runs to start a created run
// (ssh.RunHelper).
func Main(args []string) int {
	if len(args) > 0 && args[0] == "run" {
		return ssh.RunHelper(args[1:], ssh.HelperOptions{Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr})
	}
	ext := Build()
	if sdk.PackageCommand(args, ext.Manifest, Certification(), os.Stdout) {
		return 0
	}
	if err := sdk.Serve(ext); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}
