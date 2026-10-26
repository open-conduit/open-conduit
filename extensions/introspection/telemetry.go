package introspection

import (
	"context"
	"encoding/json"
	"io"
	"sync"
	"time"

	"github.com/open-conduit/open-conduit/observability"
)

// Telemetry events (05-03): v1 captured named product-telemetry events
// (agent_problem, host_metrics, host_alert) on its sinks. Here they are an
// optional adapter on the public observability contract, not a runtime
// feature: Exporter is an observability.Exporter the host adds to its
// lifecycle pipeline, which turns the committed acceptance records of the
// extension's own Event families into named events, each exactly once (the
// pipeline reports every committed record once), and the host watch
// captures its readings on the same sink. Nothing is sent anywhere unless an
// enabled entry sets `telemetry`.

// Telemetry event names.
const (
	EventReport      = "introspection_report"
	EventFix         = "introspection_fix"
	EventRunDue      = "introspection_run_due"
	EventHostMetrics = "host_metrics"
	EventHostAlert   = "host_alert"
)

// Sink receives telemetry events.
type Sink interface {
	Capture(ctx context.Context, name string, at time.Time, props map[string]any) error
}

// JSONSink writes one JSON line per event.
type JSONSink struct {
	mu sync.Mutex
	w  io.Writer
}

// NewJSONSink writes to w.
func NewJSONSink(w io.Writer) *JSONSink { return &JSONSink{w: w} }

// Capture writes the event.
func (s *JSONSink) Capture(_ context.Context, name string, at time.Time, props map[string]any) error {
	line, err := json.Marshal(map[string]any{"event": name, "at": at.UTC(), "properties": props})
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err = s.w.Write(append(line, '\n'))
	return err
}

// Exporter maps lifecycle records to telemetry events.
type Exporter struct {
	// Enabled reports whether any enabled entry turns telemetry on; it is
	// read per batch, so a config apply takes effect without a restart.
	Enabled func() bool
	Sink    Sink
}

var _ observability.Exporter = (*Exporter)(nil)

// Export captures the extension's Event acceptances.
func (x *Exporter) Export(ctx context.Context, records []observability.Record) error {
	if x.Sink == nil || x.Enabled == nil || !x.Enabled() {
		return nil
	}
	for _, r := range records {
		name, ok := telemetryName(r)
		if !ok {
			continue
		}
		props := map[string]any{"scope": string(r.Correlation.ScopeID), "event_id": string(r.Correlation.EventID), "event_type": r.Attributes["event.type"], "source": r.Attributes["event.source"]}
		if c := r.Attributes["event.causation_id"]; c != "" {
			props["causation_id"] = c
		}
		if w := r.Correlation.WorkID; w != "" {
			props["work_id"] = string(w)
		}
		if err := x.Sink.Capture(ctx, name, r.At, props); err != nil {
			return err
		}
	}
	return nil
}

func telemetryName(r observability.Record) (string, bool) {
	if r.Name != "event.accepted" {
		return "", false
	}
	switch t := r.Attributes["event.type"]; {
	case t == TypeReported:
		return EventReport, true
	case t == TypeFixRequested:
		return EventFix, true
	case t == TypeSweepDue || t == TypeTriageDue:
		return EventRunDue, true
	}
	return "", false
}

// Shutdown has nothing to release; the host owns the sink's file.
func (x *Exporter) Shutdown(context.Context) error { return nil }
