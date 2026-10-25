package telemetry

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/alsiola/open-conduit/observability"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	otellog "go.opentelemetry.io/otel/log"
	lognoop "go.opentelemetry.io/otel/log/noop"
	"go.opentelemetry.io/otel/metric"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

// Scope is the OpenTelemetry instrumentation scope name.
const Scope = "github.com/alsiola/open-conduit"

// OTelOptions supplies host-configured providers. The host (bootstrap) owns
// SDK setup and exporters, which come from extensions; nil selects no-op.
type OTelOptions struct {
	Tracer trace.TracerProvider
	Meter  metric.MeterProvider
	Logger otellog.LoggerProvider
}

// OTel maps lifecycle records onto OpenTelemetry spans, logs and metrics. It
// is an observability.Exporter so it runs behind the Pipeline's isolation.
//
// Trace IDs are derived deterministically from the record's correlation trace,
// so spans emitted before and after a restart join the same trace. Each record
// is a span under a deterministic virtual parent: the execution for attempt
// records, otherwise the causal Event. Terminal execution records span the
// attempt duration. High-cardinality identifiers are span/log attributes only;
// metric attributes are limited to record names, generic states, executor
// binding names, outcome kinds and currencies.
type OTel struct {
	tracer   trace.Tracer
	logger   otellog.Logger
	records  metric.Int64Counter
	duration metric.Float64Histogram
	tokens   metric.Int64Counter
	cost     metric.Float64Counter
}

func NewOTel(o OTelOptions) (*OTel, error) {
	if o.Tracer == nil {
		o.Tracer = tracenoop.NewTracerProvider()
	}
	if o.Meter == nil {
		o.Meter = metricnoop.NewMeterProvider()
	}
	if o.Logger == nil {
		o.Logger = lognoop.NewLoggerProvider()
	}
	m := o.Meter.Meter(Scope)
	x := &OTel{tracer: o.Tracer.Tracer(Scope), logger: o.Logger.Logger(Scope)}
	var err error
	if x.records, err = m.Int64Counter("open_conduit.lifecycle.records", metric.WithDescription("Committed lifecycle transitions by record name and generic state"), metric.WithUnit("{record}")); err != nil {
		return nil, err
	}
	if x.duration, err = m.Float64Histogram("open_conduit.execution.duration", metric.WithDescription("Attempt wall-clock duration from first claim to terminal outcome"), metric.WithUnit("s")); err != nil {
		return nil, err
	}
	if x.tokens, err = m.Int64Counter("open_conduit.execution.tokens", metric.WithDescription("Executor-reported token usage"), metric.WithUnit("{token}")); err != nil {
		return nil, err
	}
	if x.cost, err = m.Float64Counter("open_conduit.execution.cost", metric.WithDescription("Executor-reported cost")); err != nil {
		return nil, err
	}
	return x, nil
}

func digest(parts ...string) []byte {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(strconv.Itoa(len(p)) + ":" + p))
	}
	return h.Sum(nil)
}

// TraceID maps a correlation trace onto a W3C trace ID. Runtime traces are
// hex SHA-256 identities and keep their first 128 bits; other strings hash.
func TraceID(s string) trace.TraceID {
	var id trace.TraceID
	if b, err := hex.DecodeString(s); err == nil && len(b) >= 16 {
		copy(id[:], b[:16])
	} else {
		copy(id[:], digest("trace", s)[:16])
	}
	if !id.IsValid() {
		id[15] = 1
	}
	return id
}

// ParentSpanID is the deterministic virtual parent of a record's span.
func ParentSpanID(c observability.Correlation) trace.SpanID {
	var id trace.SpanID
	anchor := "event:" + string(c.EventID)
	if c.ExecutionID != "" {
		anchor = "execution:" + string(c.ExecutionID)
	} else if c.EventID == "" {
		anchor = "trace:" + c.TraceID
	}
	copy(id[:], digest("span", anchor)[:8])
	if !id.IsValid() {
		id[7] = 1
	}
	return id
}

func failed(r observability.Record) bool {
	return strings.HasSuffix(r.Name, ".failed") || strings.HasSuffix(r.Name, ".rejected") || strings.HasSuffix(r.Name, ".cancelled") && strings.HasPrefix(r.Name, "execution.") || r.Name == "ingress.scan_partial"
}

func severity(r observability.Record) otellog.Severity {
	switch {
	case failed(r), r.Name == "execution.recovered", r.Name == "retry.scheduled", r.Name == "event.duplicate":
		return otellog.SeverityWarn
	}
	return otellog.SeverityInfo
}

// Attributes returns the full span/log attribute set for a record.
func Attributes(r observability.Record) []attribute.KeyValue {
	c := r.Correlation
	kv := []attribute.KeyValue{attribute.String("open_conduit.record", r.Name), attribute.String("open_conduit.scope.id", string(c.ScopeID)), attribute.String("open_conduit.trace.id", c.TraceID)}
	add := func(k, v string) {
		if v != "" {
			kv = append(kv, attribute.String(k, v))
		}
	}
	add("open_conduit.event.id", string(c.EventID))
	add("open_conduit.work.id", string(c.WorkID))
	add("open_conduit.execution.id", string(c.ExecutionID))
	add("open_conduit.executor.binding", c.ExecutorBinding)
	add("open_conduit.state.from", r.From)
	add("open_conduit.state.to", r.To)
	keys := make([]string, 0, len(r.Attributes))
	for k := range r.Attributes {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		add("open_conduit."+k, r.Attributes[k])
	}
	if u := r.Usage; u != nil {
		if u.InputTokens != nil {
			kv = append(kv, attribute.Int64("open_conduit.usage.input_tokens", int64(*u.InputTokens)))
		}
		if u.OutputTokens != nil {
			kv = append(kv, attribute.Int64("open_conduit.usage.output_tokens", int64(*u.OutputTokens)))
		}
		if u.CachedTokens != nil {
			kv = append(kv, attribute.Int64("open_conduit.usage.cached_tokens", int64(*u.CachedTokens)))
		}
		if u.Cost != nil {
			kv = append(kv, attribute.Float64("open_conduit.usage.cost", *u.Cost), attribute.String("open_conduit.usage.currency", u.Currency))
		}
	}
	return kv
}

func (x *OTel) Export(ctx context.Context, records []observability.Record) error {
	for _, r := range records {
		x.one(ctx, r)
	}
	return nil
}

func (x *OTel) one(ctx context.Context, r observability.Record) {
	c := r.Correlation
	parent := trace.NewSpanContext(trace.SpanContextConfig{TraceID: TraceID(c.TraceID), SpanID: ParentSpanID(c), TraceFlags: trace.FlagsSampled, Remote: true})
	ctx = trace.ContextWithRemoteSpanContext(ctx, parent)
	attrs := Attributes(r)
	start := r.At
	var seconds float64
	if ms, err := strconv.ParseInt(r.Attributes["execution.duration_ms"], 10, 64); err == nil && ms >= 0 {
		seconds = float64(ms) / 1000
		start = r.At.Add(-time.Duration(ms) * time.Millisecond)
	}
	spanCtx, span := x.tracer.Start(ctx, r.Name, trace.WithTimestamp(start), trace.WithSpanKind(trace.SpanKindInternal), trace.WithAttributes(attrs...))
	if failed(r) {
		span.SetStatus(codes.Error, r.Name)
	}
	span.End(trace.WithTimestamp(r.At))

	var lr otellog.Record
	lr.SetTimestamp(r.At)
	lr.SetObservedTimestamp(r.At)
	lr.SetSeverity(severity(r))
	lr.SetSeverityText(severity(r).String())
	lr.SetEventName(r.Name)
	lr.SetBody(otellog.StringValue(r.Name))
	for _, a := range attrs {
		lr.AddAttributes(otellog.KeyValueFromAttribute(a))
	}
	x.logger.Emit(spanCtx, lr)

	metricAttrs := []attribute.KeyValue{attribute.String("record", r.Name)}
	if r.From != "" {
		metricAttrs = append(metricAttrs, attribute.String("state.from", r.From))
	}
	if r.To != "" {
		metricAttrs = append(metricAttrs, attribute.String("state.to", r.To))
	}
	x.records.Add(ctx, 1, metric.WithAttributes(metricAttrs...))
	binding := attribute.String("executor.binding", c.ExecutorBinding)
	if strings.HasPrefix(r.Name, "execution.") && r.Attributes["execution.duration_ms"] != "" {
		x.duration.Record(ctx, seconds, metric.WithAttributes(binding, attribute.String("outcome", r.Attributes["outcome.kind"])))
	}
	if u := r.Usage; u != nil {
		for kind, v := range map[string]*uint64{"input": u.InputTokens, "output": u.OutputTokens, "cached": u.CachedTokens} {
			if v != nil {
				x.tokens.Add(ctx, int64(*v), metric.WithAttributes(binding, attribute.String("token.type", kind)))
			}
		}
		if u.Cost != nil {
			x.cost.Add(ctx, *u.Cost, metric.WithAttributes(binding, attribute.String("currency", u.Currency)))
		}
	}
}

// Shutdown is a no-op: the host owns and shuts down the providers.
func (x *OTel) Shutdown(context.Context) error { return nil }
