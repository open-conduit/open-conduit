package bootstrap

import (
	"context"
	"fmt"
	"runtime/debug"
	"sort"
	"sync"
	"time"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/internal/runtime"
	"github.com/open-conduit/open-conduit/internal/telemetry"
	"github.com/open-conduit/open-conduit/observability"
)

// DefaultMetricInterval is how often the host collects its meter for the
// metrics exporter packages (Options.MetricInterval), the OpenTelemetry
// SDK's default export interval.
const DefaultMetricInterval = time.Minute

// metricCallTimeout bounds one package's telemetry.metrics call.
const metricCallTimeout = 10 * time.Second

// metricTicker paces the collections; tests replace it so none waits on
// real time.
var metricTicker = func(every time.Duration) (<-chan time.Time, func()) {
	t := time.NewTicker(every)
	return t.C, t.Stop
}

// metricExports is what doctor's metrics check reports: each metrics
// exporter package's last export, and its failure code while it fails.
type metricExports struct {
	mu   sync.Mutex
	last map[string]metricExport
}

type metricExport struct {
	at      time.Time
	metrics int
	code    string
}

// metricExporters are the running packages that declare metrics, by name.
func (d *daemon) metricExporters() map[string]observability.MetricExporter {
	out := map[string]observability.MetricExporter{}
	for _, x := range d.extensionPackages() {
		if x.r == nil {
			continue
		}
		if e, ok := x.r.MetricExporter(); ok {
			out[x.Name()] = e
		}
	}
	return out
}

// exportMetrics collects the meter through reader every MetricInterval
// while a metrics exporter package runs, and sends each collection to
// every one of them at once, each call bounded. The host collects nothing
// while none runs, so the gauges' reads cost nothing then. A package that
// fails or is slow loses that collection only: sums and histograms are
// cumulative, so the next carries everything.
func (d *daemon) exportMetrics(ctx context.Context, reader sdkmetric.Reader) {
	tick, stop := metricTicker(d.o.MetricInterval)
	defer stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick:
		}
		d.exportMetricsOnce(ctx, reader)
	}
}

func (d *daemon) exportMetricsOnce(ctx context.Context, reader sdkmetric.Reader) {
	// A panic in a collection loses that one, not every later one.
	defer func() {
		if v := recover(); v != nil {
			d.log.Error("recovered from a panic", "kind", "metrics", "panic", fmt.Sprint(v), "stack", string(debug.Stack()))
			d.report("metrics", "", "panic")
		}
	}()
	d.exportMetricsTo(ctx, reader, d.metricExporters())
}

func (d *daemon) exportMetricsTo(ctx context.Context, reader sdkmetric.Reader, targets map[string]observability.MetricExporter) {
	if len(targets) == 0 {
		return
	}
	var rm metricdata.ResourceMetrics
	collect, done := context.WithTimeout(ctx, metricCallTimeout)
	defer done()
	if err := reader.Collect(collect, &rm); err != nil {
		if ctx.Err() == nil {
			d.report("metrics", "", "collect_failed")
		}
		return
	}
	d.clear("metrics", "")
	metrics := telemetry.Snapshot(rm)
	var wg sync.WaitGroup
	for name, e := range targets {
		wg.Add(1)
		go func() {
			defer wg.Done()
			call, cancel := context.WithTimeout(ctx, metricCallTimeout)
			defer cancel()
			code := ""
			if err := e.ExportMetrics(call, metrics); err != nil {
				code = "export_failed"
				if call.Err() != nil {
					code = "export_timeout"
				}
			}
			d.metricExports.mu.Lock()
			if d.metricExports.last == nil {
				d.metricExports.last = map[string]metricExport{}
			}
			d.metricExports.last[name] = metricExport{at: d.now(), metrics: len(metrics), code: code}
			d.metricExports.mu.Unlock()
			switch {
			case code == "":
				d.clear("metrics", domain.ID(name))
			case ctx.Err() == nil:
				d.report("metrics", domain.ID(name), code)
			}
		}()
	}
	wg.Wait()
}

// metricsCheck is doctor's metrics check: the running metrics exporter
// packages and how their last export went.
func (d *daemon) metricsCheck(context.Context) runtime.Check {
	check := runtime.Check{Name: "metrics", Status: runtime.CheckOK, Code: "ok"}
	targets := d.metricExporters()
	if len(targets) == 0 {
		check.Status, check.Code, check.Detail = runtime.CheckInfo, "no_exporter", "no metrics exporter package is running, so the host's metrics are not exported"
		return check
	}
	names := make([]string, 0, len(targets))
	for name := range targets {
		names = append(names, name)
	}
	sort.Strings(names)
	d.metricExports.mu.Lock()
	defer d.metricExports.mu.Unlock()
	failing := 0
	for _, name := range names {
		last, ok := d.metricExports.last[name]
		switch {
		case !ok:
			check.Items = append(check.Items, name+": no collection yet")
		case last.code != "":
			failing++
			check.Items = append(check.Items, fmt.Sprintf("%s: last export %s (%s)", name, last.code, last.at.UTC().Format(time.RFC3339)))
		default:
			check.Items = append(check.Items, fmt.Sprintf("%s: exported %d metric(s) at %s", name, last.metrics, last.at.UTC().Format(time.RFC3339)))
		}
	}
	check.Detail = fmt.Sprintf("%d metrics exporter(s), every %s", len(names), d.o.MetricInterval)
	if failing > 0 {
		check.Status, check.Code = runtime.CheckWarn, "export_failing"
	}
	return check
}
