package bootstrap

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"

	"github.com/open-conduit/open-conduit/observability"
)

type fakeMetricExporter struct {
	err error
	got [][]observability.Metric
}

func (f *fakeMetricExporter) ExportMetrics(_ context.Context, m []observability.Metric) error {
	f.got = append(f.got, m)
	return f.err
}

func TestExportMetricsIsolatesAFailingPackage(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 21, 9, 0, 0, 0, time.UTC)
	d := &daemon{log: slog.New(slog.NewTextHandler(io.Discard, nil)), now: func() time.Time { return now }, reported: map[string]string{}, reportedAt: map[string]time.Time{}}
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	if _, err := provider.Meter("test").Int64ObservableGauge("open_conduit.test", metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
		o.Observe(7)
		return nil
	})); err != nil {
		t.Fatal(err)
	}
	good, bad := &fakeMetricExporter{}, &fakeMetricExporter{err: errors.New("collector detail")}
	targets := map[string]observability.MetricExporter{"good": good, "bad": bad}

	d.exportMetricsTo(ctx, reader, targets)
	if len(good.got) != 1 || len(good.got[0]) != 1 || good.got[0][0].Points[0].Value != 7 {
		t.Fatalf("the healthy package got %+v", good.got)
	}
	if d.metricExports.last["good"].code != "" || d.metricExports.last["bad"].code != "export_failed" {
		t.Fatalf("exports = %+v", d.metricExports.last)
	}
	if got := d.reportedCode("metrics", "bad"); got != "export_failed" {
		t.Fatalf("reported %q", got)
	}

	// A later collection reaches the package that failed, and clears it.
	bad.err = nil
	d.exportMetricsTo(ctx, reader, targets)
	if len(bad.got) != 2 || d.metricExports.last["bad"].code != "" || d.reportedCode("metrics", "bad") != "" {
		t.Fatalf("recovery: got %d, last %+v, reported %q", len(bad.got), d.metricExports.last["bad"], d.reportedCode("metrics", "bad"))
	}
}

func TestExportMetricsCollectsNothingWithoutAPackage(t *testing.T) {
	calls := 0
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	if _, err := provider.Meter("test").Int64ObservableGauge("open_conduit.test", metric.WithInt64Callback(func(context.Context, metric.Int64Observer) error {
		calls++
		return nil
	})); err != nil {
		t.Fatal(err)
	}
	d := &daemon{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	d.exportMetricsTo(context.Background(), reader, nil)
	if calls != 0 {
		t.Fatalf("collected %d time(s) with no exporter", calls)
	}
}
