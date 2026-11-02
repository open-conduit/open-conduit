// Package githubext packages the first-party GitHub extension
// (extensions/github, unchanged) as an installable subprocess extension: the
// same entries, ingress, webhooks, routes, tools, projection, actions,
// annotations, rendering, doctor checks and hold executor, served over
// protocol version 1 by protocol/sdk/inprocsdk, which serves any extension
// written against the in-process contract. It is the proof that the
// contract crosses the process boundary without loss (#357); the daemon
// keeps registering GitHub in process, so this package cannot be installed
// beside it (its names conflict).
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
	for _, s := range []string{"checks", "claim", "comment", "conflicts", "delivery", "dependency.filter", "hold", "hold.recovery", "hold.released", "issue", "tool"} {
		p.Schemas = append(p.Schemas, protocol.SchemaDecl{Name: Name + "." + s, Versions: []int{1}})
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
	return protocol.Manifest{Protocol: []int{protocol.Version}, Name: Name, Version: Version,
		Summary: "The first-party GitHub extension (issues, pull requests, reviews and checks) as a subprocess package.",
		Command: []string{"github-extension"}, Provides: p}
}

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
