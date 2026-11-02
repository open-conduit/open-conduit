package inprocsdk

import (
	"context"

	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/extension/inproc"
	"github.com/open-conduit/open-conduit/protocol"
	"github.com/open-conduit/open-conduit/protocol/sdk"
)

// The surfaces an in-process extension offers through its toolbelt
// registration or optional interfaces that the wire carries as their own
// methods (#428): the operation and start gates (operation.gate,
// work.admit), panels (view.render), the telemetry exporter
// (telemetry.export) and sink (telemetry.sink). The host asks for each only
// when the manifest declares it, which a registration made at start can't
// say beforehand, so the base manifest declares the gates.

// wireSurfaces sets the served gates, panels, exporter and sink.
func (b *Bridge) wireSurfaces() {
	e := b.ext
	p := e.Manifest.Provides
	if p.OperationGate {
		e.Gate = func(ctx context.Context, a extension.Authority, c extension.GateCall) (*extension.WaitPlan, error) {
			if g := b.registration().Gate; g != nil {
				return g(ctx, a, c)
			}
			return nil, nil
		}
	}
	if p.StartGate {
		e.Start = func(ctx context.Context, c extension.StartCall) (*extension.StartHold, error) {
			if g := b.registration().StartGate; g != nil {
				return g(ctx, c)
			}
			return nil, nil
		}
	}
	if a, ok := b.x.(inproc.Actor); ok && p.Panels {
		e.Panels = func(ctx context.Context, _ *sdk.Host, t extension.ActionTarget) ([]extension.Panel, error) {
			var out []extension.Panel
			for _, pr := range providers(a, t.Scope.ID) {
				pp, ok := pr.(extension.PanelProvider)
				if !ok {
					continue
				}
				panels, err := pp.Panels(ctx, t)
				if err != nil {
					return nil, err
				}
				out = append(out, panels...)
			}
			return out, nil
		}
	}
	if x, ok := b.x.(inproc.Exporting); ok {
		e.Exporter = x.Exporter()
	}
	if s, ok := b.x.(inproc.Sink); ok && e.Manifest.Allows(protocol.PermissionTelemetrySink) {
		e.Sink = func(ctx context.Context, _ *sdk.Host, ev protocol.TelemetryEvent) error {
			return s.Capture(ctx, ev.ScopeID, ev.Name, ev.At, ev.Properties)
		}
	}
}
