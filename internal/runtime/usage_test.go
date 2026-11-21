package runtime_test

import (
	"context"
	"encoding/json"
	"math"
	"testing"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/internal/runtime"
	"github.com/open-conduit/open-conduit/storage"
)

// usageFeed reads the usage records after a cursor, as a collector does.
func usageFeed(t *testing.T, read func(func(storage.Tx) error), after uint64) (out []domain.UsageRecord, last uint64) {
	t.Helper()
	last = after
	read(func(tx storage.Tx) error {
		out, last = nil, after
		events, err := tx.TypeEvents(domain.TypeExecutionUsage, nil, after, 0)
		for _, a := range events {
			var r domain.UsageRecord
			if e := json.Unmarshal(a.Event.Data.Data, &r); e != nil {
				return e
			}
			if a.Event.WorkID != "" || a.Event.ExecutionID != "" || a.Event.CorrelationKey != nil {
				t.Fatalf("a usage record addresses Work: %+v", a.Event)
			}
			out, last = append(out, r), a.Sequence
		}
		return err
	})
	return out, last
}

// Park, resume, park, resume and finish with no budgets configured: one
// record for each landed result, durable through a store reopened for every
// transaction, each seen once by a cursor, summing to the run's usage.
func TestUsageRecordsFollowEachLandedResult(t *testing.T) {
	ctx := context.Background()
	f := waitFixture(t, "reopen", waitOptions(true))
	d := f.Driver.(*waitDriver)
	read := func(fn func(storage.Tx) error) { routingRead(t, f, fn) }
	work := seedSchedulerWork(t, f, "usage-feed")
	key := domain.CorrelationKey{Source: "fixture/account", ExternalID: "usage-feed"}
	usage := func(in, out uint64, cost, minutes float64) extension.Usage {
		return extension.Usage{InputTokens: &in, OutputTokens: &out, Cost: &cost, Currency: "USD", Units: map[string]float64{"minutes": minutes}, Attribution: map[string]string{"model": "fixture-model"}}
	}
	attempt, err := d.Scheduler.Start(ctx, work)
	if err != nil {
		t.Fatal(err)
	}
	var seen []domain.UsageRecord
	var cursor uint64
	follow := func(want int) {
		t.Helper()
		got, next := usageFeed(t, read, cursor)
		if len(got) != want {
			t.Fatalf("cursor %d read %d new records, want %d: %+v", cursor, len(got), want, got)
		}
		seen, cursor = append(seen, got...), next
	}
	lease := attempt.Lease
	for i, id := range []domain.ID{"usage-wait-1", "usage-wait-2"} {
		wait := domain.Wait{ID: id, AfterSequence: 1, Match: domain.EventMatch{Keys: []domain.CorrelationKey{key}, Types: []string{"fixture.changed"}}}
		read(func(tx storage.Tx) error {
			var e error
			wait.AfterSequence, e = tx.LatestSequence()
			return e
		})
		parked := extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeWaiting, Summary: "parked"}, Usage: usage(100, 10, 0.25, 2)}
		if err = d.Scheduler.WaitWithResult(ctx, lease, wait, true, parked); err != nil {
			t.Fatal(err)
		}
		follow(1)
		if err = d.Scheduler.SettleBudgets(ctx); err != nil {
			t.Fatal(err)
		}
		follow(0)
		requireAccept(t, f, "usage-wake-"+string(rune('1'+i)), &key)
		if err = d.Wake(id); err != nil {
			t.Fatal(err)
		}
		resumed, err := d.Scheduler.Start(ctx, work)
		if err != nil {
			t.Fatal(err)
		}
		lease = resumed.Lease
		follow(0)
	}
	if err = d.Scheduler.Complete(ctx, lease, extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeCompleted}, Usage: usage(20, 5, 0.125, 1)}); err != nil {
		t.Fatal(err)
	}
	follow(1)
	if len(seen) != 3 {
		t.Fatalf("records: %+v", seen)
	}
	sum := map[string]float64{}
	for i, r := range seen {
		if r.Work != string(work) || r.Execution != string(attempt.Lease.ExecutionID) || r.Scope == "" || r.Binding == "" || r.Executor == "" || r.Segment != i+1 || r.StartedAt == nil || r.EndedAt == nil || r.Attribution["model"] != "fixture-model" {
			t.Fatalf("record %d: %+v", i, r)
		}
		for unit, v := range r.Amounts {
			sum[unit] += v
		}
	}
	var run runtime.Run
	read(func(tx storage.Tx) error {
		var e error
		run, e = runtime.RunInTx(tx, attempt.Lease.ExecutionID)
		return e
	})
	u := run.Usage
	for unit, want := range map[string]float64{"input_tokens": float64(*u.InputTokens), "output_tokens": float64(*u.OutputTokens), "tokens": float64(*u.InputTokens + *u.OutputTokens), "usd": *u.Cost, "minutes": u.Units["minutes"]} {
		if math.Abs(sum[unit]-want) > 1e-9 {
			t.Fatalf("%s: records sum to %v, the run used %v", unit, sum[unit], want)
		}
	}
}

// A result that reports nothing writes no record.
func TestUsageRecordNeedsUsage(t *testing.T) {
	ctx := context.Background()
	f := waitFixture(t, "reopen", waitOptions(true))
	d := f.Driver.(*waitDriver)
	work := seedSchedulerWork(t, f, "usage-none")
	attempt, err := d.Scheduler.Start(ctx, work)
	if err != nil {
		t.Fatal(err)
	}
	if err = d.Scheduler.Complete(ctx, attempt.Lease, extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeCompleted}}); err != nil {
		t.Fatal(err)
	}
	if got, _ := usageFeed(t, func(fn func(storage.Tx) error) { routingRead(t, f, fn) }, 0); len(got) != 0 {
		t.Fatalf("records for a result with no usage: %+v", got)
	}
}

// A usage record past the causal depth limit stays in the feed and reaches
// no route, so a route on usage can't feed itself. Within the limit it is
// routable: the negative control.
func TestUsageRecordPastTheDepthLimitStartsNothing(t *testing.T) {
	ctx := context.Background()
	o := waitOptions(true)
	o.MaxDepth = 1
	f := waitFixture(t, "reopen", o)
	d := f.Driver.(*waitDriver)
	work := seedSchedulerWork(t, f, "usage-depth")
	attempt, err := d.Scheduler.Start(ctx, work)
	if err != nil {
		t.Fatal(err)
	}
	n := uint64(5)
	if err = d.Scheduler.Complete(ctx, attempt.Lease, extension.Result{Outcome: domain.Outcome{Kind: domain.OutcomeCompleted}, Usage: extension.Usage{InputTokens: &n}}); err != nil {
		t.Fatal(err)
	}
	var id domain.ID
	routingRead(t, f, func(tx storage.Tx) error {
		events, e := tx.TypeEvents(domain.TypeExecutionUsage, nil, 0, 0)
		if e != nil || len(events) != 1 {
			t.Fatalf("usage events: %v %v", events, e)
		}
		id = events[0].Event.ID
		within, e := runtime.UsageRoutableInTx(tx, id, runtime.CausalLimits{MaxDepth: 2})
		if e != nil || !within {
			t.Fatalf("a record within the limit is not routable: %v %v", within, e)
		}
		return nil
	})
	requireRoute(t, f.Driver, id)
	routingRead(t, f, func(tx storage.Tx) error {
		a, e := tx.Event(id)
		if e != nil || !a.Routed || a.Disposition != "unmatched" || a.RoutedWorkID != "" {
			t.Fatalf("record past the limit: %+v %v", a, e)
		}
		return nil
	})
}
