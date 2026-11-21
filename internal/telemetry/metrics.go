package telemetry

import (
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/open-conduit/open-conduit/observability"
)

// Snapshot maps one collection of the host's meter onto the public metric
// types (observability.Metric), which telemetry.metrics carries to
// metrics exporter packages. Exponential histograms and summaries, which
// the host's instruments never produce, are left out.
func Snapshot(rm metricdata.ResourceMetrics) []observability.Metric {
	var out []observability.Metric
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			x := observability.Metric{Name: m.Name, Description: m.Description, Unit: m.Unit}
			switch d := m.Data.(type) {
			case metricdata.Sum[int64]:
				x.Kind, x.Monotonic = observability.MetricSum, d.IsMonotonic
				for _, p := range d.DataPoints {
					x.Points = append(x.Points, observability.MetricPoint{Attributes: attrs(p.Attributes), Start: p.StartTime, Time: p.Time, Value: float64(p.Value)})
				}
			case metricdata.Sum[float64]:
				x.Kind, x.Monotonic = observability.MetricSum, d.IsMonotonic
				for _, p := range d.DataPoints {
					x.Points = append(x.Points, observability.MetricPoint{Attributes: attrs(p.Attributes), Start: p.StartTime, Time: p.Time, Value: p.Value})
				}
			case metricdata.Gauge[int64]:
				x.Kind = observability.MetricGauge
				for _, p := range d.DataPoints {
					x.Points = append(x.Points, observability.MetricPoint{Attributes: attrs(p.Attributes), Start: p.StartTime, Time: p.Time, Value: float64(p.Value)})
				}
			case metricdata.Gauge[float64]:
				x.Kind = observability.MetricGauge
				for _, p := range d.DataPoints {
					x.Points = append(x.Points, observability.MetricPoint{Attributes: attrs(p.Attributes), Start: p.StartTime, Time: p.Time, Value: p.Value})
				}
			case metricdata.Histogram[int64]:
				x.Kind = observability.MetricHistogram
				for _, p := range d.DataPoints {
					x.Points = append(x.Points, histogramPoint(p.Attributes, p.StartTime, p.Time, p.Count, float64(p.Sum), p.Bounds, p.BucketCounts, extremum(p.Min), extremum(p.Max)))
				}
			case metricdata.Histogram[float64]:
				x.Kind = observability.MetricHistogram
				for _, p := range d.DataPoints {
					x.Points = append(x.Points, histogramPoint(p.Attributes, p.StartTime, p.Time, p.Count, p.Sum, p.Bounds, p.BucketCounts, extremum(p.Min), extremum(p.Max)))
				}
			default:
				continue
			}
			if len(x.Points) > 0 {
				out = append(out, x)
			}
		}
	}
	return out
}

func histogramPoint(set attribute.Set, start, at time.Time, count uint64, sum float64, bounds []float64, buckets []uint64, lo, hi *float64) observability.MetricPoint {
	return observability.MetricPoint{Attributes: attrs(set), Start: start, Time: at, Count: count, Sum: sum, Bounds: bounds, Buckets: buckets, Min: lo, Max: hi}
}

func extremum[N int64 | float64](e metricdata.Extrema[N]) *float64 {
	v, ok := e.Value()
	if !ok {
		return nil
	}
	f := float64(v)
	return &f
}

func attrs(set attribute.Set) map[string]string {
	if set.Len() == 0 {
		return nil
	}
	out := make(map[string]string, set.Len())
	for _, kv := range set.ToSlice() {
		out[string(kv.Key)] = kv.Value.Emit()
	}
	return out
}
