package bootstrap

import (
	"context"
	"fmt"
	"sync"

	"github.com/open-conduit/open-conduit/internal/runtime"
	"github.com/open-conduit/open-conduit/storage"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// txTimes records how long write transactions wait for the store's writer
// and hold it: the latest passWindow of each in memory, which the
// transactions check summarises, and every one as an OpenTelemetry
// histogram (open_conduit.storage.transaction.wait and .hold).
type txTimes struct {
	mu    sync.Mutex
	waits window
	holds window
	// total counts every transaction recorded, beyond the window, and
	// notBegun those that never got the writer.
	total, notBegun uint64
	wait            metric.Float64Histogram
	hold            metric.Float64Histogram
}

// newTxTimes registers the histograms on meter (none when nil).
func newTxTimes(meter metric.Meter) (*txTimes, error) {
	t := &txTimes{}
	if meter == nil {
		return t, nil
	}
	var err error
	if t.wait, err = meter.Float64Histogram("open_conduit.storage.transaction.wait", metric.WithDescription("Time a write transaction waited for the store's writer"), metric.WithUnit("s")); err != nil {
		return nil, err
	}
	if t.hold, err = meter.Float64Histogram("open_conduit.storage.transaction.hold", metric.WithDescription("Time a write transaction held the store's writer, until it committed or rolled back"), metric.WithUnit("s")); err != nil {
		return nil, err
	}
	return t, nil
}

// record takes one transaction's timing.
func (t *txTimes) record(x storage.TransactionTiming) {
	t.mu.Lock()
	t.total++
	t.waits.add(x.Wait)
	// A transaction that never began held nothing: a zero would pull the
	// hold's spread down.
	if x.Began {
		t.holds.add(x.Hold)
	} else {
		t.notBegun++
	}
	t.mu.Unlock()
	if t.wait != nil {
		ctx := context.Background()
		outcome := metric.WithAttributes(attribute.String("open_conduit.outcome", txOutcome(x)))
		t.wait.Record(ctx, x.Wait.Seconds(), outcome)
		if x.Began {
			t.hold.Record(ctx, x.Hold.Seconds(), outcome)
		}
	}
}

// txOutcome is how a write transaction ended, a small fixed set.
func txOutcome(x storage.TransactionTiming) string {
	switch {
	case !x.Began:
		return "not_begun"
	case x.Committed:
		return "committed"
	}
	return "rolled_back"
}

// transactionsCheck summarises the latest write transactions for doctor. It
// reports what it measured and judges nothing, as the loop check does.
func (d *daemon) transactionsCheck(context.Context) runtime.Check {
	check := runtime.Check{Name: "transactions", Status: runtime.CheckOK, Code: "ok"}
	t := d.txTimes
	if t == nil {
		check.Detail = "not timed by this store"
		return check
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	waits := t.waits.sorted()
	if len(waits) == 0 {
		check.Detail = "no transaction yet"
		return check
	}
	check.Detail = fmt.Sprintf("latest %d of %d write transactions", len(waits), t.total)
	if t.notBegun > 0 {
		check.Detail += fmt.Sprintf(", %d of them never got the writer", t.notBegun)
	}
	check.Items = []string{"wait: " + spread(waits)}
	if holds := t.holds.sorted(); len(holds) > 0 {
		check.Items = append(check.Items, "hold: "+spread(holds))
	}
	return check
}
