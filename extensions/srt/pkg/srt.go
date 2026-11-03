// Package srtext serves the srt process sandbox provider (extensions/srt)
// as a package over protocol version 1, through protocol/sdk (#477): the
// host plans a sandboxed binding's launches through sandbox_provider.plan
// and proves a recovered one through sandbox_provider.adopt, and the
// package's offline srt check is doctor's view of whether srt can isolate
// here. Nothing in the daemon knows about srt: a binding names sandbox
// provider "srt" once this package is installed (or bundled beside the
// binary). open-conduit serves it as `open-conduit extension-run srt`.
//
// The host keeps the authority: it merges its protected set into a launch's
// needs before the package sees them, pins each plan's digest itself, and
// fails closed (nothing runs) on any refusal or a package that isn't there.
package srtext

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/extensions/srt"
	"github.com/open-conduit/open-conduit/protocol"
	"github.com/open-conduit/open-conduit/protocol/sdk"
)

// Name is the package's registration name: the provider's.
const Name = srt.Type

// Version is this package build's own version.
const Version = "0.1.0"

// Manifest declares the srt sandbox provider and its offline doctor check.
func Manifest() protocol.Manifest {
	return protocol.Manifest{Protocol: []int{protocol.Version}, Name: Name, Version: Version, Title: "srt sandbox",
		Summary: "Runs a sandboxed binding's launches under Anthropic's sandbox runtime (srt), with the operator's filesystem and network policy.",
		Command: protocol.HostRunCommand(Name),
		Provides: protocol.Provides{
			Sandboxes: []protocol.SandboxDecl{{Type: srt.Type}},
			Checks:    []protocol.CheckDecl{{Name: Name, Offline: true}},
		}}
}

// Build returns the served extension around p.
func Build(p *srt.Provider) *sdk.Extension {
	return &sdk.Extension{Manifest: Manifest(),
		Validate: func(_ context.Context, v protocol.ValidateParams) error {
			if v.Kind != protocol.ValidateSandbox || v.Type != srt.Type {
				return nil
			}
			return srt.ValidateConfig(v.Config)
		},
		Sandboxes: map[string]extension.SandboxProvider{srt.Type: p},
		CheckRuns: map[string]func(context.Context, *sdk.Host, protocol.CheckRunParams) (protocol.CheckRunResult, error){
			Name: func(ctx context.Context, _ *sdk.Host, c protocol.CheckRunParams) (protocol.CheckRunResult, error) {
				return check(ctx, p, c.Config), nil
			},
		}}
}

// check is doctor's view of srt for the bindings that use it: that srt is
// present at a known version and that the kernel lets it isolate (on Linux,
// user namespaces and seccomp), by running a command under an empty policy.
func check(ctx context.Context, p *srt.Provider, cfg protocol.CheckConfig) protocol.CheckRunResult {
	var bindings []string
	for _, s := range cfg.Scopes {
		for _, b := range s.Bindings {
			if b.Sandbox != nil && b.Sandbox.Provider == srt.Type {
				bindings = append(bindings, s.ID+"/"+b.Name)
			}
		}
	}
	if len(bindings) == 0 {
		return protocol.CheckRunResult{OK: true, Status: "info", Code: "not_configured", Detail: "no binding runs in an srt sandbox"}
	}
	sort.Strings(bindings)
	h := p.Health(ctx)
	out := protocol.CheckRunResult{OK: h.Status != "fail", Status: h.Status, Code: h.Code, Detail: h.Detail, Items: h.Items}
	if h.Status != "ok" && h.Status != "warn" {
		out.Status = "fail"
		out.Detail += "; sandboxed bindings run nothing until this is fixed"
	}
	out.Items = append(out.Items, "sandboxed: "+strings.Join(bindings, ", "))
	return out
}

// Certification is the package's certification.json: a static fixture of
// sandbox configs, one srt accepts and ones it refuses (a pattern where it
// takes only literal paths, and a host that isn't a name).
func Certification() protocol.CertificationFile {
	return protocol.CertificationFile{Fixture: &protocol.Fixture{Sandboxes: []protocol.SandboxCase{
		{Type: srt.Type, Config: json.RawMessage(`{"write":["{workspace}"],"network":{"allow":["github.com"]}}`)},
		{Type: srt.Type, Config: json.RawMessage(`{"write":["/tmp/*"]}`), Refused: true},
		{Type: srt.Type, Config: json.RawMessage(`{"network":{"allow":["not a host"]}}`), Refused: true},
	}}}
}

// Main serves the package over protocol version 1 on stdin and stdout, as
// `open-conduit extension-run srt` does. Its subcommands package-manifest
// and package-certification print the package files.
func Main(args []string) int {
	ext := Build(srt.New(srt.Options{}))
	if sdk.PackageCommand(args, ext.Manifest, Certification(), os.Stdout) {
		return 0
	}
	if err := sdk.Serve(ext); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}
