// Package jiraext serves the Jira extension (extensions/jira, unchanged)
// as a package over protocol version 1, through the tracker kit's package
// (extensions/tracker/pkg, #428). open-conduit serves it as
// `open-conduit extension-run jira`, and the release bundles it.
package jiraext

import (
	"github.com/open-conduit/open-conduit/extensions/jira"
	trackerext "github.com/open-conduit/open-conduit/extensions/tracker/pkg"
	"github.com/open-conduit/open-conduit/protocol"
)

// Name is the package's registration name: the extension's.
const Name = jira.Type

var pkg = trackerext.Package{Kind: jira.Kind(), Title: "Jira", Fixture: fixture,
	Summary: "Works Jira issues: ingests issues and comments, gives each claimed issue Work, and projects its state back to Jira."}

// Manifest is the package manifest.
func Manifest() protocol.Manifest { return pkg.Manifest() }

// Playbooks are the package's playbooks, written beside its manifest.
func Playbooks() map[string]string { return pkg.Playbooks() }

// Main serves the package, as `open-conduit extension-run jira` does.
func Main(args []string) int { return pkg.Main(args) }
