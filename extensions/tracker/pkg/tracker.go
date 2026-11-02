// Package trackerext serves a tracker built on the tracker kit
// (extensions/tracker) as a package over protocol version 1, through
// protocol/sdk/inprocsdk (#428): its connection and entry types, ingress
// and webhooks, route, tools, projection task, doctor checks and the
// playbooks every tracker shares. extensions/linear/pkg and
// extensions/jira/pkg use it, and open-conduit serves each as
// `open-conduit extension-run <type>`.
package trackerext

import (
	"fmt"
	"os"

	"github.com/open-conduit/open-conduit/extensions/tracker"
	"github.com/open-conduit/open-conduit/extensions/tracker/playbooks"
	"github.com/open-conduit/open-conduit/protocol"
	"github.com/open-conduit/open-conduit/protocol/sdk"
	"github.com/open-conduit/open-conduit/protocol/sdk/inprocsdk"
)

// Version is the tracker packages' own version.
const Version = "0.1.0"

// PlaybooksDir is where a tracker package's playbooks are, relative to
// its manifest.
const PlaybooksDir = "playbooks"

// Package is one tracker served as a package.
type Package struct {
	// Kind is the tracker, as its in-process extension is built from.
	Kind tracker.Kind
	// Title and Summary describe it in the manifest.
	Title, Summary string
	// Fixture starts the tracker's certification fixture.
	Fixture func() (Fixture, error)
}

// base is what the bridge cannot read from the extension's surfaces: the
// families its ingress emits, its payload schemas, its handler
// operations, the Work kind its route creates, its checks' names (in the
// order the extension lists them) and its playbooks.
func (p Package) base() protocol.Manifest {
	typ := p.Kind.Type
	t := tracker.TypesOf(typ)
	var pr protocol.Provides
	for _, f := range tracker.Families(typ) {
		pr.Families = append(pr.Families, protocol.Family{Type: f.Type, ReDerivable: f.ReDerivable, Loss: f.Loss})
	}
	for _, s := range []string{t.SchemaIssue, t.SchemaComment, t.SchemaDelivery, typ + ".tool.v1"} {
		name, _, _ := protocol.ParseSchemaID(s)
		pr.Schemas = append(pr.Schemas, protocol.SchemaDecl{Name: name, Versions: []int{1}})
	}
	for _, op := range tracker.Operations(typ) {
		pr.Operations = append(pr.Operations, protocol.OperationDecl{Name: op, Kind: protocol.OperationHandler})
	}
	pr.WorkKinds = []protocol.WorkKindDecl{{Kind: tracker.WorkKind(typ)}}
	pr.Checks = []protocol.CheckDecl{{Name: typ}, {Name: typ + "-auth"}}
	pr.Playbooks = PlaybooksDir
	return protocol.Manifest{Protocol: []int{protocol.Version}, Name: typ, Version: Version, Title: p.Title, Summary: p.Summary,
		Command: protocol.HostRunCommand(typ), Provides: pr}
}

// New returns the bridge serving a new tracker extension.
func (p Package) New() (*inprocsdk.Bridge, error) {
	return inprocsdk.New(tracker.NewExtension(p.Kind), p.base())
}

// Manifest is the package manifest.
func (p Package) Manifest() protocol.Manifest {
	b, err := p.New()
	if err != nil {
		panic(err)
	}
	return b.Extension().Manifest
}

// Playbooks are the package's playbooks: the ones every tracker shares.
func (Package) Playbooks() map[string]string { return playbooks.All() }

// Main serves the package over protocol version 1 on stdin and stdout, as
// `open-conduit extension-run <type>` does. Its subcommands
// package-manifest and package-certification print the package files, and
// certify-fixture serves the conformance runner's fixture.
func (p Package) Main(args []string) int {
	b, err := p.New()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if sdk.PackageCommand(args, b.Extension().Manifest, p.Certification(), os.Stdout) {
		return 0
	}
	if len(args) == 1 && args[0] == "certify-fixture" {
		return p.certifyFixture()
	}
	_ = b.Serve()
	return 0
}
