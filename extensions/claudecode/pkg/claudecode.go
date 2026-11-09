// Package claudecodeext serves the Claude Code executor
// (extensions/claudecode, unchanged) as a package over protocol version 1,
// through protocol/sdk/inprocsdk (#428): the executor with resume,
// interrupt, inject, stop, forget, attach, container attach and live
// output, its claude-code doctor check, its housekeeping task and its
// claude-code.status setup probe. open-conduit serves it as `open-conduit
// extension-run claude-code`, and the release bundles it.
package claudecodeext

import (
	"fmt"
	"os"

	"github.com/open-conduit/open-conduit/extensions/claudecode"
	"github.com/open-conduit/open-conduit/protocol"
	"github.com/open-conduit/open-conduit/protocol/sdk"
	"github.com/open-conduit/open-conduit/protocol/sdk/inprocsdk"
)

// Name is the package's registration name: the executor's.
const Name = claudecode.Name

// Version is this package build's own version.
const Version = "0.1.0"

// base is what the bridge cannot read from the extension's surfaces.
func base() protocol.Manifest {
	return protocol.Manifest{Protocol: []int{protocol.Version}, Name: Name, Version: Version, Title: "Claude Code",
		Summary: "Runs Claude Code turns with sessions, handoff, attach and the MCP toolbelt.",
		Command: protocol.HostRunCommand(Name),
		Provides: protocol.Provides{Checks: []protocol.CheckDecl{{Name: "claude-code", Offline: true}},
			Families: []protocol.Family{{Type: claudecode.TypeTaskEnded, Loss: "A task an agent started has ended, as its wrapper recorded or the sweep found; nothing upstream re-derives it."}},
			Schemas:  []protocol.SchemaDecl{{Name: claudecode.Name + ".task", Versions: []int{1}}, {Name: claudecode.Name + ".tool", Versions: []int{1}}},
			Operations: []protocol.OperationDecl{
				{Name: claudecode.OpTaskStart, Kind: protocol.OperationHandler, Guidance: claudecode.TaskGuidance(claudecode.OpTaskStart)},
				{Name: claudecode.OpTaskList, Kind: protocol.OperationHandler},
				{Name: claudecode.OpTaskStop, Kind: protocol.OperationHandler},
				{Name: claudecode.OpTaskWait, Kind: protocol.OperationWait},
			}}}
}

// New returns the bridge serving a new Claude Code executor.
func New() (*inprocsdk.Bridge, error) { return inprocsdk.New(claudecode.NewExtension(), base()) }

// Manifest is the package manifest.
func Manifest() protocol.Manifest {
	b, err := New()
	if err != nil {
		panic(err)
	}
	return b.Extension().Manifest
}

// Main serves the package over protocol version 1 on stdin and stdout, as
// `open-conduit extension-run claude-code` does. Its subcommands
// package-manifest and package-certification print the package files, and
// certify-fixture serves the certification fixture.
func Main(args []string) int {
	if len(args) == 1 && args[0] == "certify-fixture" {
		if err := ServeFixture(os.Stdin, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		return 0
	}
	b, err := New()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if sdk.PackageCommand(args, b.Extension().Manifest, Certification(), os.Stdout) {
		return 0
	}
	_ = b.Serve()
	return 0
}
