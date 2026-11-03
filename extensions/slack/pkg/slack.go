// Package slackext serves the Slack extension (extensions/slack,
// unchanged) as a package over protocol version 1, through
// protocol/sdk/inprocsdk (#428): its connection and entry types, Socket
// Mode and Events API ingress, the support-thread route and its samples,
// its tools, the projection and socket tasks, rendering, its doctor checks
// and its playbook. open-conduit serves it as
// `open-conduit extension-run slack`, and the release bundles it.
package slackext

import (
	sl "github.com/open-conduit/open-conduit/extensions/slack"
	"github.com/open-conduit/open-conduit/protocol"
	"github.com/open-conduit/open-conduit/protocol/sdk/inprocsdk"
)

// Name is the package's registration name: the extension's.
const Name = sl.Type

// Version is this package build's own version.
const Version = "0.1.0"

// ProjectTask is the projection task's name in the package.
const ProjectTask = "slack-projection"

// PlaybooksDir is where the package's playbooks are, relative to its
// manifest.
const PlaybooksDir = "playbooks"

// base is what the bridge cannot read from the extension's surfaces: the
// families its ingress emits, its payload schemas, its handler operations,
// the Work kind its route creates, its checks' names (in the order the
// extension lists them) and its playbooks.
func base() protocol.Manifest {
	p := protocol.Provides{
		Schemas:   []protocol.SchemaDecl{{Name: "slack.message", Versions: []int{1}}, {Name: "slack.message.removed", Versions: []int{1}}, {Name: "slack.reply", Versions: []int{1}}, {Name: "slack.tool", Versions: []int{1}}},
		WorkKinds: []protocol.WorkKindDecl{{Kind: sl.WorkKind}},
		Checks:    []protocol.CheckDecl{{Name: Name}, {Name: Name + "-auth"}},
		Playbooks: PlaybooksDir,
	}
	for _, f := range sl.Families() {
		p.Families = append(p.Families, protocol.Family{Type: f.Type, ReDerivable: f.ReDerivable, Loss: f.Loss})
	}
	for _, op := range sl.Operations() {
		p.Operations = append(p.Operations, protocol.OperationDecl{Name: op, Kind: protocol.OperationHandler})
	}
	return protocol.Manifest{Protocol: []int{protocol.Version}, Name: Name, Version: Version, Title: "Slack",
		Summary: "Works support conversations in Slack channels over Socket Mode or the Events API, each thread its own Work.",
		Command: protocol.HostRunCommand(Name), Provides: p}
}

// New returns the bridge serving a new Slack extension.
func New() (*inprocsdk.Bridge, error) { return inprocsdk.New(sl.New(), base()) }

// Manifest is the package manifest.
func Manifest() protocol.Manifest {
	b, err := New()
	if err != nil {
		panic(err)
	}
	return b.Extension().Manifest
}

// Playbooks are the package's playbooks, written beside its manifest.
func Playbooks() map[string]string { return sl.New().Playbooks() }
