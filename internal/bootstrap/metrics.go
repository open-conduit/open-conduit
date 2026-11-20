package bootstrap

import (
	"context"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/open-conduit/open-conduit/internal/runtime"
	"github.com/open-conduit/open-conduit/internal/telemetry"
	"github.com/open-conduit/open-conduit/storage"
)

// unroutedCap bounds the unrouted Events a collection reads: the gauge
// counts up to it, so a collection's cost stays fixed however large the
// backlog grows during an outage, and a backlog at the cap is alarming
// already. The oldest is the first read, so its age is exact.
var unroutedCap = 1000

// openMetrics registers the host's gauges on the meter: what it holds now,
// read when the meter's readers collect (passTimes records each pass's
// duration). No attribute carries an identifier: pools, states and
// outcomes are small fixed sets. It runs once
// the generation is active and ingress is open, before the host loop, so
// the gauges' callback reads nothing still being built.
func (d *daemon) openMetrics(meter metric.MeterProvider) error {
	m := meter.Meter(telemetry.Scope)
	lastPass, err := m.Float64ObservableGauge("open_conduit.host.last_pass_age", metric.WithDescription("Time since the host loop last completed a pass"), metric.WithUnit("s"))
	if err != nil {
		return err
	}
	limit, err := m.Int64ObservableGauge("open_conduit.capacity.limit", metric.WithDescription("A capacity pool's slots from settings (0: unbounded)"), metric.WithUnit("{slot}"))
	if err != nil {
		return err
	}
	inUse, err := m.Int64ObservableGauge("open_conduit.capacity.in_use", metric.WithDescription("Attempts holding a capacity pool's slot"), metric.WithUnit("{slot}"))
	if err != nil {
		return err
	}
	waiting, err := m.Int64ObservableGauge("open_conduit.capacity.waiting", metric.WithDescription("Ready Work a capacity pool's limit held back at its last start"), metric.WithUnit("{work}"))
	if err != nil {
		return err
	}
	unrouted, err := m.Int64ObservableGauge("open_conduit.events.unrouted", metric.WithDescription("Accepted Events not yet routed, counted up to 1000"), metric.WithUnit("{event}"))
	if err != nil {
		return err
	}
	unroutedAge, err := m.Float64ObservableGauge("open_conduit.events.unrouted.oldest_age", metric.WithDescription("Time since the oldest unrouted Event was accepted (0 with none)"), metric.WithUnit("s"))
	if err != nil {
		return err
	}
	lag, err := m.Float64ObservableGauge("open_conduit.reconcile.lag", metric.WithDescription("Longest time any ingress source has gone since its last complete reconcile scan"), metric.WithUnit("s"))
	if err != nil {
		return err
	}
	sources, err := m.Int64ObservableGauge("open_conduit.reconcile.sources", metric.WithDescription("Ingress sources by reconcile state (current, lagging, failing)"), metric.WithUnit("{source}"))
	if err != nil {
		return err
	}
	leases, err := m.Int64ObservableGauge("open_conduit.leases.held", metric.WithDescription("Execution leases this daemon holds and renews"), metric.WithUnit("{lease}"))
	if err != nil {
		return err
	}
	records, err := m.Int64ObservableCounter("open_conduit.telemetry.records", metric.WithDescription("Lifecycle records through the telemetry pipeline, by outcome (observed, exported, dropped, invalid)"), metric.WithUnit("{record}"))
	if err != nil {
		return err
	}
	failures, err := m.Int64ObservableCounter("open_conduit.telemetry.export_failures", metric.WithDescription("Failed exporter delivery attempts in the telemetry pipeline"), metric.WithUnit("{attempt}"))
	if err != nil {
		return err
	}
	_, err = m.RegisterCallback(func(ctx context.Context, o metric.Observer) error {
		now := d.now()
		if last := d.health.lastPass.Load(); last != 0 {
			o.ObserveFloat64(lastPass, now.Sub(time.Unix(0, last)).Seconds())
		}
		if d.current.Load() != nil {
			if pools, err := d.capacityPools(ctx); err == nil {
				for _, p := range pools {
					pool := metric.WithAttributes(attribute.String("pool", p.Name))
					o.ObserveInt64(limit, int64(p.Limit), pool)
					o.ObserveInt64(inUse, int64(p.InUse), pool)
					o.ObserveInt64(waiting, int64(p.Waiting), pool)
				}
			}
		}
		var count int
		var oldest time.Time
		if err := storage.View(ctx, d.coord, now, func(tx storage.Tx) error {
			got, e := tx.UnroutedEvents(0, unroutedCap)
			count, oldest = len(got), time.Time{}
			if len(got) > 0 {
				oldest = got[0].Event.ReceivedAt
			}
			return e
		}); err == nil {
			o.ObserveInt64(unrouted, int64(count))
			age := 0.0
			if !oldest.IsZero() {
				age = now.Sub(oldest).Seconds()
			}
			o.ObserveFloat64(unroutedAge, age)
		}
		if statuses, err := d.ingress.Inspect(ctx); err == nil {
			byState := map[string]int64{"current": 0, "lagging": 0, "failing": 0}
			longest := 0.0
			for _, s := range statuses {
				failing, lagging := runtime.SourceLag(s, now, 2)
				switch {
				case failing:
					byState["failing"]++
				case lagging:
					byState["lagging"]++
				default:
					byState["current"]++
				}
				if !s.LastComplete.IsZero() {
					longest = max(longest, now.Sub(s.LastComplete).Seconds())
				}
			}
			o.ObserveFloat64(lag, longest)
			for state, n := range byState {
				o.ObserveInt64(sources, n, metric.WithAttributes(attribute.String("state", state)))
			}
		}
		d.mu.Lock()
		held := len(d.inflight)
		d.mu.Unlock()
		o.ObserveInt64(leases, int64(held))
		st := d.pipeline.Stats()
		for outcome, n := range map[string]uint64{"observed": st.Observed, "exported": st.Exported, "dropped": st.Dropped, "invalid": st.Invalid} {
			o.ObserveInt64(records, int64(n), metric.WithAttributes(attribute.String("outcome", outcome)))
		}
		o.ObserveInt64(failures, int64(st.ExportFailures))
		return nil
	}, lastPass, limit, inUse, waiting, unrouted, unroutedAge, lag, sources, leases, records, failures)
	return err
}
