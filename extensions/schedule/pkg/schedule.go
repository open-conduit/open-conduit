// Package scheduleext serves the schedule extension (extensions/schedule,
// unchanged) as a package over protocol version 1, through
// protocol/sdk/inprocsdk (#428): its entry type, job webhooks, timed slots
// (core timers with the schedule.v1 recurrence), triggers derived from
// code-host deliveries, the schedule route, the standing job Work kind,
// run-ended announcements, `job:` wait targets and its doctor checks.
// Its derived triggers and announcements are returned emissions, accepted
// by the host in the transaction that reads them. open-conduit serves it
// as `open-conduit extension-run schedule`, and the release bundles it.
package scheduleext

import (
	"fmt"
	"os"

	"github.com/open-conduit/open-conduit/extensions/schedule"
	"github.com/open-conduit/open-conduit/protocol"
	"github.com/open-conduit/open-conduit/protocol/sdk"
	"github.com/open-conduit/open-conduit/protocol/sdk/inprocsdk"
)

// Name is the package's registration name: the extension's.
const Name = schedule.Type

// Version is this package build's own version.
const Version = "0.1.0"

// slotLoss and derivedLoss are the loss of the families the extension
// mints rather than observes.
const (
	slotLoss    = "A timed slot is fired by the job's core timer; a slot missed while the daemon is down is caught up within six hours, older ones are skipped."
	derivedLoss = "Derived by the extension from an accepted Event (a code-host delivery or a finished run); nothing upstream re-derives it, and it is derived again from the same parent with the same identity."
)

// base is what the bridge cannot read from the extension's surfaces: the
// families it emits, its payload schemas and its checks' names, in the
// order the extension lists them.
func base() protocol.Manifest {
	var p protocol.Provides
	for _, f := range schedule.Families() {
		p.Families = append(p.Families, protocol.Family{Type: f.Type, ReDerivable: f.ReDerivable, Loss: f.Loss})
	}
	p.Families = append(p.Families, protocol.Family{Type: schedule.TypeSlot, Loss: slotLoss}, protocol.Family{Type: schedule.TypeEvent, Loss: derivedLoss})
	for _, t := range schedule.RunTypes {
		p.Families = append(p.Families, protocol.Family{Type: t, Loss: derivedLoss})
	}
	p.Schemas = []protocol.SchemaDecl{{Name: "schedule.trigger", Versions: []int{1}}, {Name: "schedule.run", Versions: []int{1}}}
	p.Checks = []protocol.CheckDecl{{Name: Name}, {Name: Name + "-webhooks"}}
	return protocol.Manifest{Protocol: []int{protocol.Version}, Name: Name, Version: Version, Title: "Scheduled jobs",
		Summary: "Runs jobs on a timetable, on a webhook or on code-host events, each as its own standing Work, and announces each finished run.",
		Command: protocol.HostRunCommand(Name), Provides: p}
}

// New returns the bridge serving a new schedule extension.
func New() (*inprocsdk.Bridge, error) { return inprocsdk.New(schedule.New(), base()) }

// Manifest is the package manifest.
func Manifest() protocol.Manifest {
	b, err := New()
	if err != nil {
		panic(err)
	}
	return b.Extension().Manifest
}

// Main serves the package over protocol version 1 on stdin and stdout, as
// `open-conduit extension-run schedule` does. Its subcommands
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
