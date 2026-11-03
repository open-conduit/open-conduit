package inprocsdk_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/extension/inproc"
	"github.com/open-conduit/open-conduit/observability"
	"github.com/open-conduit/open-conduit/protocol"
	"github.com/open-conduit/open-conduit/protocol/sdk/inprocsdk"
	"github.com/open-conduit/open-conduit/protocol/sdk/sdktest"
)

// gated has the surfaces the wire carries as their own methods (#428): an
// operation gate and a start gate in its registration, a panel on its
// action provider, a telemetry exporter and a sink.
type gated struct {
	mu       sync.Mutex
	captured []string
}

func (*gated) Name() string                             { return "gated" }
func (*gated) Start(context.Context, inproc.Host) error { return nil }
func (*gated) Exporter() observability.Exporter         { return nopExporter{} }
func (*gated) Actions(domain.ID) map[string]extension.ActionProvider {
	return map[string]extension.ActionProvider{"desk": desk{}}
}
func (g *gated) Capture(_ context.Context, scope, name string, _ time.Time, _ map[string]any) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.captured = append(g.captured, scope+"/"+name)
	return nil
}
func (*gated) Registration() extension.Registration {
	return extension.Registration{Version: extension.ContractVersion, Name: "gated",
		Gate: func(_ context.Context, _ extension.Authority, c extension.GateCall) (*extension.WaitPlan, error) {
			if c.Operation == "other.deploy" {
				return &extension.WaitPlan{Wait: extension.WaitRequest{Match: domain.EventMatch{Types: []string{"gated.approved"}}, Description: "an approval"}}, nil
			}
			return nil, nil
		},
		StartGate: func(_ context.Context, c extension.StartCall) (*extension.StartHold, error) {
			if c.Work.Kind == "gated.held" {
				return &extension.StartHold{Reason: "needs approval"}, nil
			}
			return nil, nil
		}}
}

type nopExporter struct{}

func (nopExporter) Export(context.Context, []observability.Record) error { return nil }
func (nopExporter) Shutdown(context.Context) error                       { return nil }

type desk struct{}

func (desk) Actions(context.Context, extension.ActionTarget) ([]extension.Action, error) {
	return []extension.Action{{ID: "approve", Label: "Approve"}}, nil
}
func (desk) Invoke(context.Context, extension.ActionCall) (extension.ActionPlan, error) {
	return extension.ActionPlan{}, nil
}
func (desk) Panels(context.Context, extension.ActionTarget) ([]extension.Panel, error) {
	return []extension.Panel{{ID: "queue", Title: "Queue", Sections: []extension.PanelSection{{Text: "one waiting", Actions: []string{"approve"}}}}}, nil
}

func TestBridgeCarriesGatesPanelsAndTelemetry(t *testing.T) {
	g := &gated{}
	base := protocol.Manifest{Version: "0.1.0", Command: protocol.HostRunCommand("gated"),
		Permissions: []string{protocol.PermissionOperationGate, protocol.PermissionStartGate, protocol.PermissionTelemetrySink},
		Provides:    protocol.Provides{Families: []protocol.Family{{Type: "gated.approved", ReDerivable: true}}, OperationGate: true, StartGate: true, Panels: true}}
	b, err := inprocsdk.New(g, base)
	if err != nil {
		t.Fatal(err)
	}
	e := b.Extension()
	if !e.Manifest.Provides.Exporters || e.Exporter == nil || e.Sink == nil || e.Gate == nil || e.Start == nil || e.Panels == nil {
		t.Fatalf("surfaces not carried: manifest %+v", e.Manifest.Provides)
	}
	ctx := context.Background()
	h := sdktest.New(t, e)
	if _, err := h.Configure(ctx, protocol.ConfigureParams{}); err != nil {
		t.Fatal(err)
	}
	var gate protocol.OperationGateResult
	if err := h.Call(ctx, "operation.gate", protocol.OperationGateParams{Operation: "other.deploy", Owner: "other", Input: protocol.Payload{Data: json.RawMessage(`{}`)}}, &gate); err != nil || gate.Wait == nil || gate.Wait.Wait.Description != "an approval" {
		t.Fatalf("operation.gate: %+v %v", gate, err)
	}
	var admit protocol.WorkAdmitResult
	if err := h.Call(ctx, "work.admit", protocol.WorkAdmitParams{Work: protocol.Work{ID: "w", Kind: "gated.held", ScopeID: "eng"}}, &admit); err != nil || admit.Hold == nil || admit.Hold.Reason != "needs approval" {
		t.Fatalf("work.admit: %+v %v", admit, err)
	}
	var view protocol.ViewRenderResult
	if err := h.Call(ctx, "view.render", protocol.ViewRenderParams{Target: protocol.FromActionTarget(extension.ActionTarget{Scope: domain.Scope{ID: "eng"}})}, &view); err != nil || len(view.Panels) != 1 || view.Panels[0].ID != "queue" {
		t.Fatalf("view.render: %+v %v", view, err)
	}
	if err := h.Call(ctx, "telemetry.sink", protocol.TelemetryEvent{ScopeID: "eng", Name: "run_finished", At: time.Now()}, nil); err != nil {
		t.Fatal(err)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.captured) != 1 || g.captured[0] != "eng/run_finished" {
		t.Fatalf("sink captured %v", g.captured)
	}
}

// asker is an extension with one action provider that offers nothing now
// and refuses an invocation in its own words, as human does for an answer
// to a closed question.
type asker struct{}

func (asker) Name() string                             { return "asker" }
func (asker) Start(context.Context, inproc.Host) error { return nil }
func (asker) Actions(domain.ID) map[string]extension.ActionProvider {
	return map[string]extension.ActionProvider{"decisions": closedDesk{}}
}

type closedDesk struct{}

func (closedDesk) Actions(context.Context, extension.ActionTarget) ([]extension.Action, error) {
	return nil, nil
}
func (closedDesk) Invoke(context.Context, extension.ActionCall) (extension.ActionPlan, error) {
	return extension.ActionPlan{}, &extension.OperationError{Code: "not_outstanding", Message: "already answered"}
}

// work.act names no provider, so an action no provider offers now goes to
// the scope's only provider, whose own refusal comes back rather than
// unknown_action (#428).
func TestActOnAnUnlistedActionReachesTheOnlyProvider(t *testing.T) {
	b, err := inprocsdk.New(asker{}, protocol.Manifest{Version: "0.1.0", Command: protocol.HostRunCommand("asker"),
		Provides: protocol.Provides{Families: []protocol.Family{{Type: "asker.answer", ReDerivable: true}}}})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	h := sdktest.New(t, b.Extension())
	if _, err := h.Configure(ctx, protocol.ConfigureParams{}); err != nil {
		t.Fatal(err)
	}
	target := protocol.FromActionTarget(extension.ActionTarget{Scope: domain.Scope{ID: "eng"}, Work: &domain.Work{ID: "w", ScopeID: "eng"}})
	var out protocol.WorkActResult
	err = h.Call(ctx, "work.act", protocol.WorkActParams{Target: target, Action: "answer", Input: "yes", Caller: protocol.Caller{Kind: "operator", ID: "alice"}}, &out)
	if err == nil || !strings.Contains(err.Error(), "not_outstanding") {
		t.Fatalf("work.act: %+v %v, want the provider's not_outstanding", out, err)
	}
}

// sampled routes with one handler that offers samples and one that doesn't.
type sampled struct{}

func (sampled) Name() string                             { return "sampled" }
func (sampled) Start(context.Context, inproc.Host) error { return nil }
func (sampled) Routes() map[string]extension.RouteHandler {
	return map[string]extension.RouteHandler{"sampled-plain": plainRoute{}, "sampled": samplerRoute{}}
}

type plainRoute struct{}

func (plainRoute) Match(context.Context, extension.RouteInput) (extension.RouteDecision, error) {
	return extension.Skip("never"), nil
}

type samplerRoute struct{ plainRoute }

func (samplerRoute) Samples(context.Context, extension.SampleInput) []extension.RouteSample {
	return []extension.RouteSample{{ID: "one", Title: "One"}}
}

// TestBridgeDeclaresRouteSamples: a handler that offers samples is
// declared so, so the host asks it through route.samples for a route trace.
func TestBridgeDeclaresRouteSamples(t *testing.T) {
	b, err := inprocsdk.New(sampled{}, protocol.Manifest{Protocol: []int{protocol.Version}, Name: "sampled", Version: "0.1.0", Command: []string{"sampled"}})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, r := range b.Extension().Manifest.Provides.Routes {
		got[r.Handler] = r.Samples
	}
	if !got["sampled"] || got["sampled-plain"] || len(got) != 2 {
		t.Fatalf("routes declared %v, want sampler with samples and plain without", got)
	}
}

// once has a run-once Work kind.
type once struct{ sampled }

func (once) Name() string { return "sampled" }
func (once) WorkKinds() []inproc.WorkKind {
	return []inproc.WorkKind{{Kind: "sampled.fix", RunOnce: true}, {Kind: "sampled.sweep", Standing: true}}
}

// TestBridgeDeclaresRunOnce: a run-once Work kind is declared so, so the
// host completes it on a handoff as it does in process (#428).
func TestBridgeDeclaresRunOnce(t *testing.T) {
	b, err := inprocsdk.New(once{}, protocol.Manifest{Protocol: []int{protocol.Version}, Name: "sampled", Version: "0.1.0", Command: []string{"sampled"}})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]protocol.WorkKindDecl{}
	for _, k := range b.Extension().Manifest.Provides.WorkKinds {
		got[k.Kind] = k
	}
	if !got["sampled.fix"].RunOnce || got["sampled.sweep"].RunOnce || !got["sampled.sweep"].Standing {
		t.Fatalf("work kinds declared %+v", got)
	}
}

// probing offers a setup probe.
type probing struct{ sampled }

func (probing) SetupProbes() []extension.SetupProbe {
	return []extension.SetupProbe{{Name: "accounts", Summary: "the signed-in accounts", Args: []extension.ProbeArg{{Name: "host", Required: true}},
		Run: func(_ context.Context, args map[string]string) (extension.ProbeResult, error) {
			if args["host"] == "bad" {
				return extension.ProbeResult{}, errors.New("no such host")
			}
			return extension.ProbeResult{Command: "gh auth status", Output: map[string]string{"host": args["host"]}}, nil
		}}}
}

// TestBridgeServesSetupProbes: an extension's setup probes are declared
// and served over the wire, a failure answered as its reason (#428).
func TestBridgeServesSetupProbes(t *testing.T) {
	b, err := inprocsdk.New(probing{}, protocol.Manifest{Protocol: []int{protocol.Version}, Name: "sampled", Version: "0.1.0", Command: []string{"sampled"}})
	if err != nil {
		t.Fatal(err)
	}
	m := b.Extension().Manifest
	if len(m.Provides.Probes) != 1 || m.Provides.Probes[0].Name != "accounts" || !m.Provides.Probes[0].Args[0].Required {
		t.Fatalf("probes declared %+v", m.Provides.Probes)
	}
	h := sdktest.New(t, b.Extension())
	var res protocol.ProbeRunResult
	if err := h.Call(context.Background(), "probe.serve", protocol.ProbeRunParams{Name: "accounts", Args: map[string]string{"host": "github.com"}}, &res); err != nil {
		t.Fatal(err)
	}
	if res.Command != "gh auth status" || !strings.Contains(string(res.Output), "github.com") || res.Error != "" {
		t.Fatalf("probe.serve: %+v", res)
	}
	if err := h.Call(context.Background(), "probe.serve", protocol.ProbeRunParams{Name: "accounts", Args: map[string]string{"host": "bad"}}, &res); err != nil || res.Error != "no such host" {
		t.Fatalf("a failing probe: %+v %v", res, err)
	}
}
