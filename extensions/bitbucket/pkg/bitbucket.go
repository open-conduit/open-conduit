// Package bitbucketext serves the Bitbucket extension (extensions/bitbucket,
// unchanged) as a package over protocol version 1, through
// protocol/sdk/inprocsdk (#428): its connection and entry types, ingress
// and webhooks, route, tools, webhook management and doctor checks. A
// reopened issue's pull requests key its generation from the tracker's
// Work in the scope (bitbucket.StoreGenerations), so it needs no tracker
// in its process. open-conduit serves it as
// `open-conduit extension-run bitbucket`, and the release bundles it.
package bitbucketext

import (
	"fmt"
	"os"

	"github.com/open-conduit/open-conduit/extensions/bitbucket"
	"github.com/open-conduit/open-conduit/protocol"
	"github.com/open-conduit/open-conduit/protocol/sdk"
	"github.com/open-conduit/open-conduit/protocol/sdk/inprocsdk"
)

// Name is the package's registration name: the extension's.
const Name = bitbucket.Type

// Version is this package build's own version.
const Version = "0.1.0"

// base is what the bridge cannot read from the extension's surfaces: the
// families its ingress emits, its payload schemas, its handler operations
// and its checks' names, in the order the extension lists them.
func base() protocol.Manifest {
	var p protocol.Provides
	for _, f := range bitbucket.Families() {
		p.Families = append(p.Families, protocol.Family{Type: f.Type, ReDerivable: f.ReDerivable, Loss: f.Loss})
	}
	for _, s := range []string{"checks", "comment", "delivery", "pull_request", "tool"} {
		p.Schemas = append(p.Schemas, protocol.SchemaDecl{Name: Name + "." + s, Versions: []int{1}})
	}
	for _, op := range bitbucket.Operations() {
		p.Operations = append(p.Operations, protocol.OperationDecl{Name: op, Kind: protocol.OperationHandler})
	}
	p.Checks = []protocol.CheckDecl{{Name: Name}, {Name: Name + "-auth"}}
	return protocol.Manifest{Protocol: []int{protocol.Version}, Name: Name, Version: Version, Title: "Bitbucket Cloud",
		Summary: "Works Bitbucket Cloud pull requests: ingests pull requests, comments and build statuses, and correlates each with its tracker issue's Work. `open-conduit init linear` or `init jira` adds it with --bitbucket.",
		Command: protocol.HostRunCommand(Name), Provides: p}
}

// New returns the bridge serving a new Bitbucket extension.
func New() (*inprocsdk.Bridge, error) { return inprocsdk.New(bitbucket.New(), base()) }

// Manifest is the package manifest.
func Manifest() protocol.Manifest {
	b, err := New()
	if err != nil {
		panic(err)
	}
	return b.Extension().Manifest
}

// Main serves the package over protocol version 1 on stdin and stdout, as
// `open-conduit extension-run bitbucket` does. Its subcommands
// package-manifest and package-certification print the package files, and
// certify-fixture serves the conformance runner's fixture.
func Main(args []string) int {
	b, err := New()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if sdk.PackageCommand(args, b.Extension().Manifest, Certification(), os.Stdout) {
		return 0
	}
	if len(args) == 1 && args[0] == "certify-fixture" {
		return certifyFixture()
	}
	_ = b.Serve()
	return 0
}
