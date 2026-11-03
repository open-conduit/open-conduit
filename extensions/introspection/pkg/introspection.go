// Package introspectionext serves the introspection extension
// (extensions/introspection, unchanged) as a package over protocol
// version 1, through protocol/sdk/inprocsdk (#428): its entry type, the
// introspection route, the reporting, triage and correction operations,
// the standing sweep and triage Work with their introspection.v1
// recurrence, the removal policy, the host watch (its reports root
// Events it returns), the note-count annotation, rendering, its telemetry
// exporter, its doctor check and its playbooks. open-conduit serves it as
// `open-conduit extension-run introspection`, and the release bundles it.
package introspectionext

import (
	"fmt"
	"os"

	"github.com/open-conduit/open-conduit/extensions/introspection"
	"github.com/open-conduit/open-conduit/protocol"
	"github.com/open-conduit/open-conduit/protocol/sdk"
	"github.com/open-conduit/open-conduit/protocol/sdk/inprocsdk"
)

// Name is the package's registration name: the extension's.
const Name = introspection.Type

// Version is this package build's own version.
const Version = "0.1.0"

// PlaybooksDir is where the package's playbooks are, relative to its
// manifest.
const PlaybooksDir = "playbooks"

const (
	reportLoss = "A report an agent or the host watch made; nothing upstream re-derives it."
	fixLoss    = "A fix an agent requested for reports; nothing upstream re-derives it."
	dueLoss    = "A sweep or triage slot fired by its core timer; a slot missed while the daemon is down is caught up, older ones are skipped."
)

// base is what the bridge cannot read from the extension's surfaces: the
// families it emits, its payload schemas, each operation's kind, commit
// and guidance, the telemetry it captures, its check's name and its
// playbooks.
func base() protocol.Manifest {
	p := protocol.Provides{
		Families: []protocol.Family{{Type: introspection.TypeReported, Loss: reportLoss}, {Type: introspection.TypeFixRequested, Loss: fixLoss},
			{Type: introspection.TypeSweepDue, Loss: dueLoss}, {Type: introspection.TypeTriageDue, Loss: dueLoss}},
		Schemas:   []protocol.SchemaDecl{{Name: "introspection.report", Versions: []int{1}}, {Name: "introspection.fix", Versions: []int{1}}, {Name: "introspection.due", Versions: []int{1}}},
		Checks:    []protocol.CheckDecl{{Name: Name}},
		Playbooks: PlaybooksDir,
	}
	for _, op := range introspection.Operations() {
		d := protocol.OperationDecl{Name: op, Kind: protocol.OperationHandler}
		switch op {
		case introspection.OpReport, introspection.OpFix:
			d.Kind, d.Commit = protocol.OperationEmit, true
		}
		switch op {
		case introspection.OpReport:
			d.Guidance = introspection.Guidance("report-issue")
		case introspection.OpCorrection:
			d.Guidance = introspection.Guidance("correction")
		case introspection.OpProblem:
			d.Guidance = introspection.Guidance("problem")
		}
		p.Operations = append(p.Operations, d)
	}
	return protocol.Manifest{Protocol: []int{protocol.Version}, Name: Name, Version: Version, Title: "Introspection",
		Summary:     "Lets agents report defects in Open Conduit and log corrections and problems, and runs the daily sweep, triage and fixes that act on them.",
		Command:     protocol.HostRunCommand(Name),
		Permissions: []string{protocol.PermissionTelemetryCapture},
		Provides:    p}
}

// New returns the bridge serving a new introspection extension.
func New() (*inprocsdk.Bridge, error) { return inprocsdk.New(introspection.New(), base()) }

// Manifest is the package manifest.
func Manifest() protocol.Manifest {
	b, err := New()
	if err != nil {
		panic(err)
	}
	return b.Extension().Manifest
}

// Playbooks are the package's playbooks, written beside its manifest.
func Playbooks() map[string]string { return introspection.New().Playbooks() }

// Main serves the package over protocol version 1 on stdin and stdout, as
// `open-conduit extension-run introspection` does. Its subcommands
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
