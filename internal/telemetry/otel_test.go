package telemetry

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/alsiola/open-conduit/observability"
	"go.opentelemetry.io/otel/codes"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

type logCollector struct {
	mu      sync.Mutex
	records []sdklog.Record
}

func (c *logCollector) OnEmit(_ context.Context, r *sdklog.Record) error {
	c.mu.Lock()
	c.records = append(c.records, r.Clone())
	c.mu.Unlock()
	return nil
}
func (c *logCollector) Enabled(context.Context, sdklog.EnabledParameters) bool { return true }
func (c *logCollector) Shutdown(context.Context) error                         { return nil }
func (c *logCollector) ForceFlush(context.Context) error                       { return nil }

// TestOTelMapping exercises the real OpenTelemetry SDK: spans, logs and
// metrics carry the lifecycle correlation, deterministic trace identity and
// executor usage, while metric attributes stay low-cardinality.
func TestOTelMapping(t *testing.T) {
	spans := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(spans))
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	logs := &logCollector{}
	lp := sdklog.NewLoggerProvider(sdklog.WithProcessor(logs))
	x, err := NewOTel(OTelOptions{Tracer: tp, Meter: mp, Logger: lp})
	if err != nil {
		t.Fatal(err)
	}
	p, _ := New(Options{Exporters: []observability.Exporter{x}})
	at := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	trace := "d5a9c4abca38a17bfdca8596df774e6f5e7ad41e83e3b6a22cf9fe46798c32ef"
	c := observability.Correlation{WorkID: "work/w", EventID: "event/e", ExecutionID: "work/w/attempt/1", ScopeID: "ops", ExecutorBinding: "agent", TraceID: trace}
	in, out := uint64(1200), uint64(300)
	cost := 0.042
	started := observability.Record{Name: "execution.started", At: at, Correlation: c, From: "pending", To: "running", Attributes: map[string]string{"trigger.type": "operator.requested", "record_id": "a"}}
	failed := observability.Record{Name: "execution.failed", At: at.Add(90 * time.Second), Correlation: c, From: "running", To: "failed", Attributes: map[string]string{"outcome.kind": "failed", "execution.duration_ms": "90000", "record_id": "b"}, Usage: &observability.Measurement{InputTokens: &in, OutputTokens: &out, Cost: &cost, Currency: "USD"}}
	accepted := observability.Record{Name: "event.accepted", At: at, Correlation: observability.Correlation{EventID: "event/root", ScopeID: "ops", TraceID: "not-hex"}, Attributes: map[string]string{"event.type": "operator.requested", "record_id": "c"}}
	for _, r := range []observability.Record{started, failed, accepted} {
		if err := p.Observe(context.Background(), r); err != nil {
			t.Fatal(err)
		}
	}
	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}

	got := spans.GetSpans()
	if len(got) != 3 {
		t.Fatalf("spans %d", len(got))
	}
	byName := map[string]tracetest.SpanStub{}
	for _, s := range got {
		byName[s.Name] = s
	}
	start, fail := byName["execution.started"], byName["execution.failed"]
	if start.SpanContext.TraceID() != TraceID(trace) || start.SpanContext.TraceID().String() != trace[:32] {
		t.Fatalf("trace id %s", start.SpanContext.TraceID())
	}
	if start.Parent.SpanID() != ParentSpanID(c) || fail.Parent.SpanID() != start.Parent.SpanID() || !start.Parent.IsRemote() {
		t.Fatal("attempt records must share the deterministic attempt parent")
	}
	if !fail.StartTime.Equal(at) || !fail.EndTime.Equal(at.Add(90*time.Second)) || fail.Status.Code != codes.Error {
		t.Fatalf("terminal span timing/status: %v %v %v", fail.StartTime, fail.EndTime, fail.Status)
	}
	attrs := map[string]string{}
	for _, kv := range start.Attributes {
		attrs[string(kv.Key)] = kv.Value.Emit()
	}
	for k, v := range map[string]string{"open_conduit.work.id": "work/w", "open_conduit.execution.id": "work/w/attempt/1", "open_conduit.event.id": "event/e", "open_conduit.scope.id": "ops", "open_conduit.executor.binding": "agent", "open_conduit.trigger.type": "operator.requested", "open_conduit.state.to": "running"} {
		if attrs[k] != v {
			t.Fatalf("span attribute %s=%q want %q", k, attrs[k], v)
		}
	}
	if ev := byName["event.accepted"]; !ev.SpanContext.TraceID().IsValid() || ev.Parent.SpanID() != ParentSpanID(accepted.Correlation) {
		t.Fatal("non-hex trace must still map deterministically")
	}

	logs.mu.Lock()
	if len(logs.records) != 3 {
		t.Fatalf("logs %d", len(logs.records))
	}
	for _, l := range logs.records {
		if !l.TraceID().IsValid() || !l.SpanID().IsValid() || l.Body().AsString() != l.EventName() {
			t.Fatalf("log not correlated: %+v", l)
		}
		if l.EventName() == "execution.failed" && l.SeverityText() != "WARN" {
			t.Fatalf("failure severity %s", l.SeverityText())
		}
	}
	logs.mu.Unlock()

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	metrics := map[string]metricdata.Aggregation{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			metrics[m.Name] = m.Data
		}
	}
	records := metrics["open_conduit.lifecycle.records"].(metricdata.Sum[int64])
	if len(records.DataPoints) != 3 {
		t.Fatalf("record series %d", len(records.DataPoints))
	}
	for _, dp := range records.DataPoints {
		for _, kv := range dp.Attributes.ToSlice() {
			switch string(kv.Key) {
			case "record", "state.from", "state.to":
			default:
				t.Fatalf("high-cardinality metric attribute %s", kv.Key)
			}
		}
	}
	duration := metrics["open_conduit.execution.duration"].(metricdata.Histogram[float64])
	if len(duration.DataPoints) != 1 || duration.DataPoints[0].Sum != 90 {
		t.Fatalf("duration %+v", duration.DataPoints)
	}
	tokens := metrics["open_conduit.execution.tokens"].(metricdata.Sum[int64])
	total := int64(0)
	for _, dp := range tokens.DataPoints {
		total += dp.Value
		if v, _ := dp.Attributes.Value("executor.binding"); v.AsString() != "agent" {
			t.Fatalf("token attrs %v", dp.Attributes)
		}
	}
	costs := metrics["open_conduit.execution.cost"].(metricdata.Sum[float64])
	if total != 1500 || len(costs.DataPoints) != 1 || costs.DataPoints[0].Value != 0.042 {
		t.Fatalf("usage metrics tokens=%d cost=%+v", total, costs.DataPoints)
	}
}

func TestNoopOTelNeedsNoProviders(t *testing.T) {
	x, err := NewOTel(OTelOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err = x.Export(context.Background(), []observability.Record{record("execution.started", 1)}); err != nil {
		t.Fatal(err)
	}
}
