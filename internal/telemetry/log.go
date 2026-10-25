package telemetry

import (
	"context"
	"io"
	"log/slog"
	"sort"
	"sync"

	"github.com/alsiola/open-conduit/observability"
	otellog "go.opentelemetry.io/otel/log"
)

// LogExporter writes one JSON line per lifecycle record: the local daemon log
// that works without any collector. Records arrive already validated and
// redacted by the Pipeline.
type LogExporter struct {
	mu     sync.Mutex
	logger *slog.Logger
}

func NewLogExporter(w io.Writer) *LogExporter {
	return &LogExporter{logger: slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: slog.LevelDebug}))}
}

func (l *LogExporter) Export(ctx context.Context, records []observability.Record) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, r := range records {
		c := r.Correlation
		attrs := []slog.Attr{slog.Time("at", r.At), slog.String("trace_id", c.TraceID), slog.String("scope_id", string(c.ScopeID))}
		add := func(k, v string) {
			if v != "" {
				attrs = append(attrs, slog.String(k, v))
			}
		}
		add("event_id", string(c.EventID))
		add("work_id", string(c.WorkID))
		add("execution_id", string(c.ExecutionID))
		add("executor_binding", c.ExecutorBinding)
		add("from", r.From)
		add("to", r.To)
		keys := make([]string, 0, len(r.Attributes))
		for k := range r.Attributes {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		group := make([]any, 0, len(keys))
		for _, k := range keys {
			group = append(group, slog.String(k, r.Attributes[k]))
		}
		if len(group) > 0 {
			attrs = append(attrs, slog.Group("attributes", group...))
		}
		if u := r.Usage; u != nil {
			var usage []any
			if u.InputTokens != nil {
				usage = append(usage, slog.Uint64("input_tokens", *u.InputTokens))
			}
			if u.OutputTokens != nil {
				usage = append(usage, slog.Uint64("output_tokens", *u.OutputTokens))
			}
			if u.CachedTokens != nil {
				usage = append(usage, slog.Uint64("cached_tokens", *u.CachedTokens))
			}
			if u.Cost != nil {
				usage = append(usage, slog.Float64("cost", *u.Cost), slog.String("currency", u.Currency))
			}
			attrs = append(attrs, slog.Group("usage", usage...))
		}
		level := slog.LevelInfo
		if severity(r) >= otellog.SeverityWarn {
			level = slog.LevelWarn
		}
		l.logger.LogAttrs(ctx, level, r.Name, attrs...)
	}
	return nil
}

func (l *LogExporter) Shutdown(context.Context) error { return nil }
