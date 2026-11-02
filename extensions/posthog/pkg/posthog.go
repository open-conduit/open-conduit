// Package posthogext serves the PostHog extension (extensions/posthog,
// unchanged) as a package over protocol version 1, through
// protocol/sdk/inprocsdk (#428): its connection and entry types, its
// telemetry exporter (agent_turn events and the lifecycle records an entry
// selects), its telemetry sink (the host's telemetry events) and its doctor
// checks. open-conduit serves it as `open-conduit extension-run posthog`,
// and the release bundles it.
package posthogext

import (
	"fmt"
	"os"

	"github.com/open-conduit/open-conduit/extensions/posthog"
	"github.com/open-conduit/open-conduit/protocol"
	"github.com/open-conduit/open-conduit/protocol/sdk"
	"github.com/open-conduit/open-conduit/protocol/sdk/inprocsdk"
)

// Name is the package's registration name: the extension's.
const Name = posthog.Type

// Version is this package build's own version.
const Version = "0.1.0"

// base is what the bridge cannot read from the extension's surfaces: the
// sink's permission and the checks' names, in the order the extension
// lists them.
func base() protocol.Manifest {
	return protocol.Manifest{Protocol: []int{protocol.Version}, Name: Name, Version: Version, Title: "PostHog",
		Summary:     "Sends an agent_turn event per finished execution, the lifecycle records an entry selects and the host's telemetry events to PostHog.",
		Command:     protocol.HostRunCommand(Name),
		Permissions: []string{protocol.PermissionTelemetrySink},
		Provides:    protocol.Provides{Checks: []protocol.CheckDecl{{Name: Name}, {Name: Name + "-auth"}}}}
}

// New returns the bridge serving a new PostHog extension.
func New() (*inprocsdk.Bridge, error) { return inprocsdk.New(posthog.New(), base()) }

// Manifest is the package manifest.
func Manifest() protocol.Manifest {
	b, err := New()
	if err != nil {
		panic(err)
	}
	return b.Extension().Manifest
}

// Main serves the package over protocol version 1 on stdin and stdout, as
// `open-conduit extension-run posthog` does. Its subcommands
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
