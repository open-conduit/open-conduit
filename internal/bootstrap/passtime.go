package bootstrap

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/open-conduit/open-conduit/internal/runtime"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// passWindow is how many of the latest passes the loop check summarises:
// ten minutes at the default one-second poll interval.
const passWindow = 600

// passPhases are the phases of a pass, in the order it runs them.
var passPhases = []string{"route", "poll", "start", "controls", "subscriptions", "ingress", "packages", "endpoints", "retention", "backup"}

// passTimes records how long each of the host's passes and its phases take:
// the latest passWindow of each in memory, which the loop check summarises,
// and every one as an OpenTelemetry histogram
// (open_conduit.host.pass.duration, and open_conduit.host.phase.duration by
// phase).
type passTimes struct {
	mu     sync.Mutex
	passes window
	phases map[string]*window
	// total counts every pass recorded, beyond the window.
	total  uint64
	pass   metric.Float64Histogram
	phase  metric.Float64Histogram
	since  func(time.Time) time.Duration
	nowFor func() time.Time
}

// window is a ring of the latest durations.
type window struct {
	at   int
	full bool
	d    [passWindow]time.Duration
}

func (w *window) add(d time.Duration) {
	w.d[w.at] = d
	w.at++
	if w.at == passWindow {
		w.at, w.full = 0, true
	}
}

func (w *window) sorted() []time.Duration {
	n := w.at
	if w.full {
		n = passWindow
	}
	out := slices.Clone(w.d[:n])
	slices.Sort(out)
	return out
}

// quantile is the q quantile of sorted durations (nearest rank).
func quantile(sorted []time.Duration, q float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	i := int(q*float64(len(sorted))+0.999999) - 1
	return sorted[max(0, min(i, len(sorted)-1))]
}

// newPassTimes registers the histograms on meter (none when nil).
func newPassTimes(meter metric.Meter) (*passTimes, error) {
	p := &passTimes{phases: map[string]*window{}, since: time.Since, nowFor: time.Now}
	if meter == nil {
		return p, nil
	}
	var err error
	if p.pass, err = meter.Float64Histogram("open_conduit.host.pass.duration", metric.WithDescription("Wall-clock duration of one pass of the host loop"), metric.WithUnit("s")); err != nil {
		return nil, err
	}
	if p.phase, err = meter.Float64Histogram("open_conduit.host.phase.duration", metric.WithDescription("Wall-clock duration of one phase of a host loop pass, by phase"), metric.WithUnit("s")); err != nil {
		return nil, err
	}
	return p, nil
}

// pass times one pass: each phase runs through it, and done records the
// whole.
type pass struct {
	t     *passTimes
	start time.Time
	took  map[string]time.Duration
}

func (p *passTimes) begin() *pass {
	return &pass{t: p, start: p.nowFor(), took: map[string]time.Duration{}}
}

// phase runs fn as the named phase of the pass.
func (x *pass) phase(name string, fn func()) {
	start := x.t.nowFor()
	fn()
	x.took[name] += x.t.since(start)
}

// done records the pass and its phases.
func (x *pass) done(ctx context.Context) {
	whole := x.t.since(x.start)
	p := x.t
	p.mu.Lock()
	p.total++
	p.passes.add(whole)
	for name, d := range x.took {
		w := p.phases[name]
		if w == nil {
			w = &window{}
			p.phases[name] = w
		}
		w.add(d)
	}
	p.mu.Unlock()
	if p.pass != nil {
		p.pass.Record(ctx, whole.Seconds())
		for name, d := range x.took {
			p.phase.Record(ctx, d.Seconds(), metric.WithAttributes(attribute.String("open_conduit.phase", name)))
		}
	}
}

// loopCheck summarises the latest passes for doctor: the pass's median,
// 95th percentile and longest, and the same for each phase. It reports
// what it measured and judges nothing; how long a pass may take is the
// operator's to decide.
func (d *daemon) loopCheck(context.Context) runtime.Check {
	check := runtime.Check{Name: "loop", Status: runtime.CheckOK, Code: "ok"}
	p := d.passTimes
	if p == nil {
		check.Detail = "no pass yet"
		return check
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	passes := p.passes.sorted()
	if len(passes) == 0 {
		check.Detail = "no pass yet"
		return check
	}
	check.Detail = fmt.Sprintf("latest %d of %d passes: %s", len(passes), p.total, spread(passes))
	for _, name := range passPhases {
		if w := p.phases[name]; w != nil {
			check.Items = append(check.Items, name+": "+spread(w.sorted()))
		}
	}
	return check
}

func spread(sorted []time.Duration) string {
	round := func(d time.Duration) time.Duration {
		switch {
		case d >= time.Second:
			return d.Round(time.Millisecond)
		case d >= time.Millisecond:
			return d.Round(10 * time.Microsecond)
		}
		return d.Round(time.Microsecond)
	}
	return fmt.Sprintf("median %s, p95 %s, longest %s", round(quantile(sorted, 0.5)), round(quantile(sorted, 0.95)), round(sorted[len(sorted)-1]))
}
