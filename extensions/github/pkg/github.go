// Package githubext serves the first-party GitHub extension
// (extensions/github, unchanged) as a package over protocol version 1,
// through protocol/sdk/inprocsdk (#428): the same entries, ingress,
// webhooks, routes, tools, projection, actions, annotations, rendering,
// doctor checks, hold executor, setup probes and work-issue playbook.
// open-conduit serves it as `open-conduit extension-run github`, and the
// release bundles it.
package githubext

import (
	"strings"

	gh "github.com/open-conduit/open-conduit/extensions/github"
	"github.com/open-conduit/open-conduit/protocol"
	"github.com/open-conduit/open-conduit/protocol/sdk/inprocsdk"
)

// Name is the package's registration name: the first-party GitHub name, so
// families, schemas, operations and config types are identical.
const Name = gh.Type

// Version is this package build's own version.
const Version = "0.1.0"

// emittedLoss is the loss of the families the extension emits rather than
// observes: nothing re-derives them, and none is lost while push is down.
const emittedLoss = "Emitted by the extension itself (an operator's claim, a released hold); nothing upstream re-derives it."

// base is what the bridge cannot read from the extension's surfaces.
func base() protocol.Manifest {
	p := protocol.Provides{}
	for _, f := range gh.Families() {
		p.Families = append(p.Families, protocol.Family{Type: f.Type, ReDerivable: f.ReDerivable, Loss: f.Loss})
	}
	for _, t := range []string{gh.TypeClaimRequested, gh.TypeHoldReleased} {
		p.Families = append(p.Families, protocol.Family{Type: t, Loss: emittedLoss})
	}
	for _, s := range gh.Schemas() {
		name, version, ok := protocol.ParseSchemaID(s)
		if !ok {
			panic("github: unversioned schema " + s)
		}
		p.Schemas = append(p.Schemas, protocol.SchemaDecl{Name: name, Versions: []int{version}})
	}
	for _, op := range gh.Operations() {
		kind := protocol.OperationHandler
		if op == gh.OpWait || op == gh.OpHold {
			kind = protocol.OperationWait
		}
		d := protocol.OperationDecl{Name: op, Kind: kind}
		if op == gh.OpGitCredential {
			d.Serves = "git.credential"
		}
		p.Operations = append(p.Operations, d)
	}
	p.DependencyFilter = true
	p.Playbooks = PlaybooksDir
	// The kinds its routes create: in process they need no policy, so the
	// extension declares none (it is not an inproc.WorkKinder).
	for _, k := range []string{gh.WorkKind, gh.WorkKindPull, gh.WorkKindHeld} {
		p.WorkKinds = append(p.WorkKinds, protocol.WorkKindDecl{Kind: k})
	}
	// The extension's checks, in the order it lists them (github,
	// github_capability, git_bot and github_auth in process).
	for _, c := range []string{"github", "github-capability", "github-git-bot", "github-auth"} {
		p.Checks = append(p.Checks, protocol.CheckDecl{Name: c})
	}
	return protocol.Manifest{Protocol: []int{protocol.Version}, Name: Name, Version: Version, Title: "GitHub",
		Summary: "Issues, pull requests, comments, reviews and checks of a repository; the engineering tools.",
		Command: protocol.HostRunCommand(Name), Provides: p}
}

// PlaybooksDir is where the package's playbooks are, relative to its
// manifest.
const PlaybooksDir = "playbooks"

// Playbooks are the package's playbooks, written beside its manifest.
func Playbooks() map[string]string { return gh.New().Playbooks() }

// New returns the bridge serving a new GitHub extension.
func New() (*inprocsdk.Bridge, error) { return inprocsdk.New(gh.New(), base()) }

// Manifest is the package manifest.
func Manifest() protocol.Manifest {
	b, err := New()
	if err != nil {
		panic(err)
	}
	return b.Extension().Manifest
}

// ValidNamespace is the subprocess rule for GitHub source namespaces.
func ValidNamespace(ns string) bool { return ns == Name || strings.HasPrefix(ns, Name+"/") }
