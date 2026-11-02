// Package linearext serves the Linear extension (extensions/linear, unchanged)
// as a package over protocol version 1, through the tracker kit's package
// (extensions/tracker/pkg, #428). open-conduit serves it as
// `open-conduit extension-run linear`, and the release bundles it.
package linearext

import (
	"github.com/open-conduit/open-conduit/extensions/linear"
	trackerext "github.com/open-conduit/open-conduit/extensions/tracker/pkg"
	"github.com/open-conduit/open-conduit/protocol"
)

// Name is the package's registration name: the extension's.
const Name = linear.Type

var pkg = trackerext.Package{Kind: linear.Kind(), Title: "Linear", Fixture: fixture,
	Summary: "Works Linear issues: ingests issues and comments, gives each claimed issue Work, and projects its state back to Linear."}

// Manifest is the package manifest.
func Manifest() protocol.Manifest { return pkg.Manifest() }

// Playbooks are the package's playbooks, written beside its manifest.
func Playbooks() map[string]string { return pkg.Playbooks() }

// Main serves the package, as `open-conduit extension-run linear` does.
func Main(args []string) int { return pkg.Main(args) }
