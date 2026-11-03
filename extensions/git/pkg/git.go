// Package gitext serves the git workspace provider (extensions/git) as a
// package over protocol version 1, through protocol/sdk (#477): the host
// acquires, looks up, reconciles and sweeps a binding's git workspaces
// through provider.*, and checks a binding's resource config through
// validate. Nothing in the daemon knows about git: a binding names resource
// provider "git" once this package is installed (or bundled beside the
// binary). open-conduit serves it as `open-conduit extension-run git`.
//
// The provider keeps its leases and workspaces in the directory the host
// names for its type (protocol.InitializeParams.ResourceDirs), the same one
// it used in process, so workspaces outlive the move. A relative checkout
// resolves against the config file's directory (ConfigDir).
package gitext

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"

	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/extensions/git"
	"github.com/open-conduit/open-conduit/protocol"
	"github.com/open-conduit/open-conduit/protocol/sdk"
)

// Name is the package's registration name: the provider's.
const Name = git.Type

// Version is this package build's own version.
const Version = "0.1.0"

func setting(field, typ, def string) protocol.SettingDecl {
	d := protocol.SettingDecl{Field: field, Type: typ}
	if def != "" {
		d.Default = json.RawMessage(def)
	}
	return d
}

// Manifest declares the git resource type: its modes, that it names a
// config's default mode and refuses a config a sandboxed binding can't use,
// that it sweeps spare workspaces, and the settings safe to show.
func Manifest() protocol.Manifest {
	return protocol.Manifest{Protocol: []int{protocol.Version}, Name: Name, Version: Version, Title: "Git workspaces",
		Summary: "Gives a binding's runs a git workspace: the checkout itself, a worktree per Work or per run, or a clone.",
		Command: protocol.HostRunCommand(Name),
		Provides: protocol.Provides{Resources: []protocol.ResourceDecl{{Type: git.Type,
			Modes:       []string{extension.ResourceShared, extension.ResourceWork, extension.ResourceFresh},
			ModeDefault: true, Sandbox: true, Sweep: true,
			Settings: []protocol.SettingDecl{
				setting("checkout", protocol.SettingString, ""),
				setting("mode", protocol.SettingString, `"shared"`),
				setting("remote", protocol.SettingString, `"origin"`),
				setting("base_branch", protocol.SettingString, ""),
				setting("branch_template", protocol.SettingString, `"agent/work-{work}"`),
				setting("sign", protocol.SettingBoolean, "false"),
			}}}}}
}

// provider creates the git provider on first use, once the host has named
// its directories (initialize).
type provider struct {
	once sync.Once
	p    *git.Provider
	err  error
}

func (x *provider) get(ctx context.Context) (*git.Provider, error) {
	x.once.Do(func() {
		h := sdk.HostFrom(ctx)
		dir := ""
		if h != nil {
			dir = h.ResourceDir(git.Type)
		}
		if dir == "" {
			x.err = errors.New("git extension: the host named no resource directory")
			return
		}
		x.p, x.err = git.New(git.Options{StateDir: dir, BaseDir: h.ConfigDir()})
	})
	return x.p, x.err
}

func (x *provider) Acquire(ctx context.Context, a extension.ResourceAcquisition) (extension.Resource, error) {
	p, err := x.get(ctx)
	if err != nil {
		return extension.Resource{}, err
	}
	return p.Acquire(ctx, a)
}

func (x *provider) Lookup(ctx context.Context, owner extension.ResourceOwner, ref string) (extension.Resource, error) {
	p, err := x.get(ctx)
	if err != nil {
		return extension.Resource{}, err
	}
	return p.Lookup(ctx, owner, ref)
}

func (x *provider) Reconcile(ctx context.Context, status extension.ResourceStatusFunc) (extension.ResourceReport, error) {
	p, err := x.get(ctx)
	if err != nil {
		return extension.ResourceReport{}, err
	}
	return p.Reconcile(ctx, status)
}

func (x *provider) Sweep(ctx context.Context, status extension.ResourceStatusFunc) (extension.ResourceSweepReport, error) {
	p, err := x.get(ctx)
	if err != nil {
		return extension.ResourceSweepReport{}, err
	}
	return p.Sweep(ctx, status)
}

func (*provider) ModeDefault(raw json.RawMessage) string { return git.ModeDefault(raw) }
func (*provider) SandboxWorkspace(raw json.RawMessage, mode string) error {
	return git.ValidateSandbox(raw, mode)
}

var _ interface {
	extension.ResourceProvider
	extension.ResourceSweeper
	extension.ResourceModeDefaulter
	extension.ResourceSandboxChecker
} = (*provider)(nil)

// Build returns the served extension.
func Build() *sdk.Extension {
	x := &provider{}
	return &sdk.Extension{Manifest: Manifest(),
		Validate: func(_ context.Context, v protocol.ValidateParams) error {
			if v.Kind != protocol.ValidateResource || v.Type != git.Type {
				return nil
			}
			return git.ValidateConfig(v.Config)
		},
		Providers: map[string]extension.ResourceProvider{git.Type: x}}
}

// Certification is the package's certification.json: resource configs and
// the answers config check must get, a config's own default mode and the
// sandbox check (only clone, without setup, may run sandboxed).
func Certification() protocol.CertificationFile {
	return protocol.CertificationFile{Fixture: &protocol.Fixture{Resources: []protocol.ResourceCase{
		{Type: git.Type, Config: json.RawMessage(`{"checkout":"/srv/repo","mode":"work"}`), ModeDefault: extension.ResourceWork, SandboxRefused: true},
		{Type: git.Type, Config: json.RawMessage(`{"checkout":"/srv/repo","mode":"clone"}`), ModeDefault: git.ModeClone},
		{Type: git.Type, Config: json.RawMessage(`{"checkout":"/srv/repo","mode":"clone","setup":"make deps"}`), ModeDefault: git.ModeClone, SandboxRefused: true},
		{Type: git.Type, Config: json.RawMessage(`{"checkout":"/srv/repo"}`), Mode: git.ModeClone},
	}}}
}

// Main serves the package over protocol version 1 on stdin and stdout, as
// `open-conduit extension-run git` does. Its subcommands package-manifest
// and package-certification print the package files.
func Main(args []string) int {
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
