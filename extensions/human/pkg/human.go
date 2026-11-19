// Package humanext serves the human extension (extensions/human,
// unchanged) as a package over protocol version 1, through
// protocol/sdk/inprocsdk (#428): its entry type, human.ask in every
// toolbelt, the answer action and question panel under each scope's entry
// name, the open question as a Work annotation, its operation and start
// approval gates, and the task that settles asks. open-conduit serves it
// as `open-conduit extension-run human`, and the release bundles it.
package humanext

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/extensions/human"
	"github.com/open-conduit/open-conduit/protocol"
	"github.com/open-conduit/open-conduit/protocol/sdk"
	"github.com/open-conduit/open-conduit/protocol/sdk/inprocsdk"
)

// Name is the package's registration name: the extension's.
const Name = human.Type

// Version is this package build's own version.
const Version = "0.1.0"

// base is what the bridge cannot read from the extension's surfaces:
// human.ask is a wait operation whose dependency carries a filter; the
// gates hold calls and Work starts for approval, an escalation is recorded
// as telemetry, and each change of a request is announced as one of its
// sealed request families. Its actions and annotations go under the scope's
// entry name.
func base() protocol.Manifest {
	return protocol.Manifest{Protocol: []int{protocol.Version}, Name: Name, Version: Version, Title: "Human",
		Summary:     "Lets an agent ask a person a question, or ask approval for a call or a start, parks its Work until they answer, and announces each request so the people who decide can be told.",
		Command:     protocol.HostRunCommand(Name),
		Permissions: []string{protocol.PermissionOperationGate, protocol.PermissionStartGate, protocol.PermissionTelemetryCapture},
		Provides: protocol.Provides{
			Families: []protocol.Family{{Type: human.TypeAnswer, Loss: "A person's answer, given through the answer action; nothing upstream re-derives it."},
				{Type: human.TypeRequestOpened, Loss: human.RequestLoss, Sealed: true}, {Type: human.TypeRequestEscalated, Loss: human.RequestLoss, Sealed: true},
				{Type: human.TypeRequestSettled, Loss: human.RequestLoss, Sealed: true}},
			Schemas:          []protocol.SchemaDecl{{Name: "human.answer", Versions: []int{1}}, {Name: "human.filter", Versions: []int{1}}, {Name: "human.tool", Versions: []int{1}}, {Name: "human.request", Versions: []int{1}}},
			Operations:       []protocol.OperationDecl{{Name: human.OpAsk, Kind: protocol.OperationWait, Guidance: human.AskGuidance()}},
			DependencyFilter: true, OperationGate: true, StartGate: true, Panels: true, EntryNamed: true}}
}

// New returns the bridge serving a new human extension.
func New() (*inprocsdk.Bridge, error) { return inprocsdk.New(human.New(), base()) }

// Manifest is the package manifest.
func Manifest() protocol.Manifest {
	b, err := New()
	if err != nil {
		panic(err)
	}
	return b.Extension().Manifest
}

// Main serves the package over protocol version 1 on stdin and stdout, as
// `open-conduit extension-run human` does. Its subcommands
// package-manifest and package-certification print the package files.
func Main(args []string) int {
	b, err := New()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if sdk.PackageCommand(args, b.Extension().Manifest, Certification(), os.Stdout) {
		return 0
	}
	_ = b.Serve()
	return 0
}

// Certification is the package's certification.json: a human entry, and
// route cases for the `human` route: a request the extension announced
// starts its Work, and the same Event posted under another source starts
// nothing.
func Certification() protocol.CertificationFile {
	at := time.Date(2026, 11, 19, 9, 0, 0, 0, time.UTC)
	data, _ := json.Marshal(human.RequestPayload{Request: "certify-ask", Provider: "decisions", Work: "certify-work", Kind: human.KindApproval, Operation: "deploy.run",
		Label: "Approve or refuse deploy.run", Action: human.ActionAnswer, Options: []string{human.OptionApprove, human.OptionRefuse},
		Responders: []string{"operator:local"}, AskedAt: at, ExpiresAt: at.Add(24 * time.Hour), State: extension.RequestOpen})
	key := human.RequestKey("certify-ask")
	opened := domain.Event{ID: "certify-request-opened", Type: human.TypeRequestOpened, Source: human.Type, DeliveryID: "request/certify-ask/opened",
		OccurredAt: at, ReceivedAt: at, CorrelationKey: &key, Data: domain.Payload{Schema: human.SchemaRequest, Data: data}}
	posted := opened
	posted.ID, posted.Source = "certify-request-posted", "core.manual"
	scope := domain.Scope{ID: "certify", Name: "certify"}
	route := domain.Route{Name: "requests", Handler: human.Type, Binding: "notify"}
	var cases []protocol.RouteCase
	for i, e := range []domain.Event{opened, posted} {
		in := extension.RouteInput{Event: domain.AcceptedEvent{Event: e, Sequence: uint64(i + 1)}, Scope: scope, Route: route}
		want, err := human.Route{}.Match(context.Background(), in)
		if err != nil {
			panic(err)
		}
		cases = append(cases, protocol.RouteCase{Handler: human.Type, Input: protocol.FromRouteInput(in), Want: protocol.FromRouteDecision(want)})
	}
	return protocol.CertificationFile{Fixture: &protocol.Fixture{
		Entries: []protocol.ConfigEntry{{ScopeID: "certify", Type: human.Type, Name: "decisions", Config: json.RawMessage(`{}`)}},
		Routes:  cases,
	}}
}
