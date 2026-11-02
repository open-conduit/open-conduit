// Package observeext serves the observe extension (extensions/observe,
// unchanged) as a package over protocol version 1, through
// protocol/sdk/inprocsdk (#428): its entry type, the observe route and its
// samples, observe.record on the toolbelt of a scope with an entry, and
// the observe.thread Work kind. open-conduit serves it as
// `open-conduit extension-run observe`, and the release bundles it.
package observeext

import (
	"fmt"
	"os"

	"github.com/open-conduit/open-conduit/extensions/observe"
	"github.com/open-conduit/open-conduit/protocol"
	"github.com/open-conduit/open-conduit/protocol/sdk"
	"github.com/open-conduit/open-conduit/protocol/sdk/inprocsdk"
)

// Name is the package's registration name: the extension's.
const Name = observe.Type

// Version is this package build's own version.
const Version = "0.1.0"

// base is what the bridge cannot read from the extension's surfaces:
// observe.record emits an item, answers through a commit and has guidance;
// the family it records and the item's schema.
func base() protocol.Manifest {
	return protocol.Manifest{Protocol: []int{protocol.Version}, Name: Name, Version: Version, Title: "Observe",
		Summary: "Lets an agent watching an outside source record each item it sees, so a thread becomes Work and its later items join it.",
		Command: protocol.HostRunCommand(Name),
		Provides: protocol.Provides{
			Families:   []protocol.Family{{Type: observe.TypeRecorded, Loss: "An item an agent saw and recorded; nothing upstream re-derives it."}},
			Schemas:    []protocol.SchemaDecl{{Name: "observe.item", Versions: []int{1}}},
			Operations: []protocol.OperationDecl{{Name: observe.OpRecord, Kind: protocol.OperationEmit, Commit: true, Guidance: observe.RecordGuidance()}}}}
}

// New returns the bridge serving a new observe extension.
func New() (*inprocsdk.Bridge, error) { return inprocsdk.New(observe.New(), base()) }

// Manifest is the package manifest.
func Manifest() protocol.Manifest {
	b, err := New()
	if err != nil {
		panic(err)
	}
	return b.Extension().Manifest
}

// Main serves the package over protocol version 1 on stdin and stdout, as
// `open-conduit extension-run observe` does. Its subcommands
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
