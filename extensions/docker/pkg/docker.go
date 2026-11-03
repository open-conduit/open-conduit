// Package dockerext serves the Docker container provider (extensions/docker)
// as a package over protocol version 1, through protocol/sdk (#477): the
// host plans, creates, signals, stops, inspects, lists and removes a
// binding's containers through container_provider.*, asks where a
// container's toolbelt forward listens through container_provider.forward,
// and checks a binding's container config through validate. Nothing in the
// daemon knows about Docker: a binding names container provider "docker"
// once this package is installed (or bundled beside the binary).
// open-conduit serves it as `open-conduit extension-run docker`, and an
// executor's wrapper starts a created container with its `run` subcommand.
//
// The host keeps the authority: it merges its protected set into a launch's
// needs before the package sees them, pins each plan's digest and the
// container itself, and stops, reconciles and removes containers through
// extension.StopContainer, ReconcileContainers and RemoveContainers.
package dockerext

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/extensions/docker"
	"github.com/open-conduit/open-conduit/protocol"
	"github.com/open-conduit/open-conduit/protocol/sdk"
)

// Name is the package's registration name: the provider's.
const Name = docker.Type

// Version is this package build's own version.
const Version = "0.1.0"

// Manifest declares the docker container provider, whose containers may
// reach the toolbelt through a forward (macOS), and its doctor check.
func Manifest() protocol.Manifest {
	return protocol.Manifest{Protocol: []int{protocol.Version}, Name: Name, Version: Version, Title: "Docker containers",
		Summary: "Runs a container binding's launches in Docker containers, behind a gateway that holds the operator's network policy.",
		Command: protocol.HostRunCommand(Name),
		Provides: protocol.Provides{
			Containers: []protocol.ContainerDecl{{Type: docker.Type, Forward: true}},
			Checks:     []protocol.CheckDecl{{Name: Name}},
		}}
}

// provider creates the docker provider on first use, once the host has
// named its directory and label (initialize), and lists on the contexts
// the last configure's bindings use.
type provider struct {
	once sync.Once
	p    *docker.Provider
	dir  string
	err  error

	mu       sync.Mutex
	contexts []string
}

func (x *provider) get(ctx context.Context) (*docker.Provider, error) {
	x.once.Do(func() {
		h := sdk.HostFrom(ctx)
		if h == nil || h.ContainerDir(docker.Type) == "" || h.ContainerHost() == "" {
			x.err = errors.New("docker extension: the host named no container directory or label")
			return
		}
		self, err := sdk.Self()
		if err != nil {
			x.err = err
			return
		}
		x.dir = h.ContainerDir(docker.Type)
		gateway := docker.GatewayImage
		if v := os.Getenv("OPEN_CONDUIT_GATEWAY_IMAGE"); v != "" {
			gateway = v
		}
		x.p = docker.New(docker.Options{Helper: append(self, "run"), Host: h.ContainerHost(), Gateway: gateway, Forward: os.Getenv("OPEN_CONDUIT_CONTAINER_FORWARD"), Contexts: x.configured})
	})
	return x.p, x.err
}

// configured are the Docker contexts the last configure's bindings use.
func (x *provider) configured() []string {
	x.mu.Lock()
	defer x.mu.Unlock()
	return slices.Clone(x.contexts)
}

func (x *provider) configure(uses []protocol.ContainerUse) {
	var contexts []string
	for _, u := range uses {
		if u.Provider != docker.Type {
			continue
		}
		if _, s, err := docker.ParseConfig(u.Config); err == nil && !slices.Contains(contexts, s.Context) {
			contexts = append(contexts, s.Context)
		}
	}
	x.mu.Lock()
	x.contexts = contexts
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

func (x *provider) Forward(ctx context.Context, config domain.Payload) (string, error) {
	p, err := x.get(ctx)
	if err != nil {
		return "", err
	}
	return p.Forward(ctx, config)
}

var _ interface {
	extension.ContainerProvider
	extension.ContainerForwarder
} = (*provider)(nil)

// Build returns the served extension.
func Build() *sdk.Extension {
	x := &provider{}
	return &sdk.Extension{Manifest: Manifest(),
		Configure: func(_ context.Context, _ *sdk.Host, p protocol.ConfigureParams) (protocol.ConfigureResult, error) {
			x.configure(p.Containers)
			return protocol.ConfigureResult{}, nil
		},
		Validate: func(_ context.Context, v protocol.ValidateParams) error {
			if v.Kind != protocol.ValidateContainer || v.Type != docker.Type {
				return nil
			}
			return docker.ValidateConfig(v.Config)
		},
		Containers: map[string]extension.ContainerProvider{docker.Type: x},
		CheckRuns: map[string]func(context.Context, *sdk.Host, protocol.CheckRunParams) (protocol.CheckRunResult, error){
			Name: func(ctx context.Context, _ *sdk.Host, c protocol.CheckRunParams) (protocol.CheckRunResult, error) {
				return x.check(ctx, c.Config), nil
			},
		}}
}

// check is doctor's view of Docker for each context and image the config's
// docker bindings use: the daemon reachable, and its ID; Engine 28 or
// later; a file written under --user into a host directory (the
// provider's own, under the state directory the workspaces share) owned by
// the operator on the host; the gateway image there by digest; and the
// internal-network preflight. Any failure blocks the provider.
func (x *provider) check(ctx context.Context, cfg protocol.CheckConfig) protocol.CheckRunResult {
	type use struct{ name, context, image string }
	var uses []use
	for _, s := range cfg.Scopes {
		for _, b := range s.Bindings {
			if b.Container == nil || b.Container.Provider != docker.Type {
				continue
			}
			if c, st, err := docker.ParseConfig(b.Container.Config); err == nil {
				uses = append(uses, use{s.ID + "/" + b.Name, st.Context, c.Image})
			}
		}
	}
	if len(uses) == 0 {
		return protocol.CheckRunResult{OK: true, Status: "info", Code: "not_configured", Detail: "no binding runs in a docker container"}
	}
	p, err := x.get(ctx)
	if err != nil {
		return protocol.CheckRunResult{Status: "fail", Code: "docker_unavailable", Detail: err.Error()}
	}
	out := protocol.CheckRunResult{OK: true, Status: "ok", Code: "ok"}
	seen := map[string]bool{}
	var details, names []string
	for _, u := range uses {
		names = append(names, u.name)
		if seen[u.context+"\x00"+u.image] {
			continue
		}
		seen[u.context+"\x00"+u.image] = true
		h := p.Health(ctx, u.context, u.image, x.dir, false)
		out.Items = append(out.Items, h.Items...)
		if h.Status != "ok" && out.OK {
			out.OK, out.Status, out.Code = false, "fail", h.Code
		}
		details = append(details, h.Detail)
	}
	out.Detail = strings.Join(details, "; ")
	if !out.OK {
		out.Detail += "; docker container bindings run nothing until this is fixed"
	}
	out.Items = append(out.Items, "in docker containers: "+strings.Join(names, ", "))
	return out
}

// Certification is the package's certification.json: a static fixture of
// container configs, one docker accepts and ones it refuses (an image not
// pinned by digest, and no Docker context).
func Certification() protocol.CertificationFile {
	image := "alpine@sha256:" + strings.Repeat("0", 64)
	return protocol.CertificationFile{Fixture: &protocol.Fixture{Containers: []protocol.ContainerCase{
		{Type: docker.Type, Config: json.RawMessage(`{"image":"` + image + `","write":["{workspace}"],"network":{"allow":["github.com"]},"provider":{"context":"default"}}`)},
		{Type: docker.Type, Config: json.RawMessage(`{"image":"alpine:3.20","provider":{"context":"default"}}`), Refused: true},
		{Type: docker.Type, Config: json.RawMessage(`{"image":"` + image + `"}`), Refused: true},
	}}}
}

// Main serves the package over protocol version 1 on stdin and stdout, as
// `open-conduit extension-run docker` does. Its subcommands package-manifest
// and package-certification print the package files, and `run [--records
// <dir>] <ref>` is what an executor's wrapper runs to start a created
// container (docker.RunHelper).
func Main(args []string) int {
	if len(args) > 0 && args[0] == "run" {
		return docker.RunHelper(args[1:], docker.HelperOptions{Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr})
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
