// Package humanext serves the human extension (extensions/human,
// unchanged) as a package over protocol version 1, through
// protocol/sdk/inprocsdk (#428): its entry type, human.ask in every
// toolbelt, the answer action and question panel under each scope's entry
// name, the open question as a Work annotation, its operation and start
// approval gates, and the task that settles asks. open-conduit serves it
// as `open-conduit extension-run human`, and the release bundles it.
package humanext

import (
	"fmt"
	"os"

	"github.com/open-conduit/open-conduit/extensions/human"
	"github.com/open-conduit/open-conduit/protocol"
	"github.com/open-conduit/open-conduit/protocol/sdk"
	"github.com/open-conduit/open-conduit/protocol/sdk/inprocsdk"
)

// Name is the package's registration name: the extension's.
const Name = human.Type

// Version is this package build's own version.
const Version = "0.1.0"

// base is what the bridge cannot read from the extension's surfaces:
// human.ask is a wait operation whose dependency carries a filter; the
// gates hold calls and Work starts for approval, and an escalation is
// recorded as telemetry. Its actions and annotations go under the scope's
// entry name.
func base() protocol.Manifest {
	return protocol.Manifest{Protocol: []int{protocol.Version}, Name: Name, Version: Version, Title: "Human",
		Summary:     "Lets an agent ask a person a question, or ask approval for a call or a start, and parks its Work until they answer.",
		Command:     protocol.HostRunCommand(Name),
		Permissions: []string{protocol.PermissionOperationGate, protocol.PermissionStartGate, protocol.PermissionTelemetryCapture},
		Provides: protocol.Provides{
			Families:         []protocol.Family{{Type: human.TypeAnswer, Loss: "A person's answer, given through the answer action; nothing upstream re-derives it."}},
			Schemas:          []protocol.SchemaDecl{{Name: "human.answer", Versions: []int{1}}, {Name: "human.filter", Versions: []int{1}}, {Name: "human.tool", Versions: []int{1}}},
			Operations:       []protocol.OperationDecl{{Name: human.OpAsk, Kind: protocol.OperationWait, Guidance: human.AskGuidance()}},
			DependencyFilter: true, OperationGate: true, StartGate: true, Panels: true, EntryNamed: true}}
}

// New returns the bridge serving a new human extension.
func New() (*inprocsdk.Bridge, error) { return inprocsdk.New(human.New(), base()) }

// Manifest is the package manifest.
func Manifest() protocol.Manifest {
	b, err := New()
	if err != nil {
		panic(err)
	}
	return b.Extension().Manifest
}

// Main serves the package over protocol version 1 on stdin and stdout, as
// `open-conduit extension-run human` does. Its subcommands
// package-manifest and package-certification print the package files.
func Main(args []string) int {
	b, err := New()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if sdk.PackageCommand(args, b.Extension().Manifest, protocol.CertificationFile{}, os.Stdout) {
		return 0
	}
	_ = b.Serve()
	return 0
}
