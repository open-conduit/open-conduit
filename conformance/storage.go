// Package conformance supplies reusable contract cases independent of backends.
package conformance

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/alsiola/open-conduit/domain"
	"github.com/alsiola/open-conduit/storage"
)

// Clock is an explicitly advanced clock; tests never depend on scheduling sleeps.
type Clock struct {
	mu  sync.Mutex
	now time.Time
}

func NewClock() *Clock                   { return &Clock{now: time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)} }
func (c *Clock) Now() time.Time          { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *Clock) Advance(d time.Duration) { c.mu.Lock(); defer c.mu.Unlock(); c.now = c.now.Add(d) }

type StorageCase struct {
	Name  string
	Check func(storage.Store) error
}

func RunStorage(t *testing.T, factory func() storage.Store) {
	t.Helper()
	for _, c := range StorageCases() {
		t.Run(c.Name, func(t *testing.T) {
			if err := c.Check(factory()); err != nil {
				t.Fatal(err)
			}
		})
	}
}

var rollback = errors.New("injected rollback")

func event(id domain.ID, now time.Time) domain.Event {
	return domain.Event{ID: id, Type: "test.changed.v1", Source: "test", DeliveryID: string(id), OccurredAt: now, ReceivedAt: now}
}
func transact(s storage.Store, c *Clock, fn func(storage.Tx) error) error {
	return s.Transact(context.Background(), storage.Guard{Now: c.Now()}, fn)
}
func seed(s storage.Store, c *Clock) error {
	return transact(s, c, func(tx storage.Tx) error {
		if err := tx.PutScope(domain.Scope{ID: "scope", Name: "scope"}, 0); err != nil {
			return err
		}
		if err := tx.PutWork(domain.Work{ID: "work", Kind: "test.objective", ScopeID: "scope", State: domain.WorkReady, CorrelationKeys: []domain.CorrelationKey{{Source: "test", ExternalID: "thread"}}}, 0); err != nil {
			return err
		}
		if _, _, err := tx.AcceptEvent(event("trigger", c.Now())); err != nil {
			return err
		}
		return tx.PutExecution(domain.Execution{ID: "execution", WorkID: "work", ExecutorBinding: "shell", Attempt: 1, State: domain.ExecutionPending, TriggerID: "trigger"}, 0)
	})
}
func wantError(got, want error) error {
	if !errors.Is(got, want) {
		return fmt.Errorf("got %v, want %v", got, want)
	}
	return nil
}
func StorageCases() []StorageCase {
	return []StorageCase{
		{"handoff_and_wait_outcomes", handoffAndWait},
		{"recovery_inventory_and_execution_revision", inventoryAndExecutionCAS},
		{"wait_wake_and_timeout_cancellation", waitWakeAndTimerCancel},
		{"terminal_correlation_and_causation", terminalCorrelationAndCausation},
		{"event_identity_not_correlation", eventIdentity},
		{"acceptance_separate_from_routing", routeReplay},
		{"atomic_route_work_kv_timer", atomicRoute},
		{"correlation_revision_and_copy_isolation", workRevision},
		{"competing_correlation_owners", competingWork},
		{"competing_claims", competingClaims},
		{"lease_expiry_recovery_and_fencing", leaseFencing},
		{"lease_renew_release", leaseRenew},
		{"terminal_work_cancellation_fences_worker", cancelFencing},
		{"timer_fire_rearm_rollback", timerRollback},
		{"one_shot_timer_and_duplicate_fire", oneShotTimer},
		{"wait_registration_atomic_with_park", waitRegistration},
		{"namespaced_kv_and_checkpoint_rollback", kvCheckpoint},
		{"context_cancellation_and_panic_rollback", cancelRollback},
		{"scope_parent_cycle_and_revision", scopeCycle},
		{"attempt_sequence_and_retry_exhaustion", attempts},
		{"per_work_routing_order", routingOrder},
	}
}
func eventIdentity(s storage.Store) error {
	c := NewClock()
	return transact(s, c, func(tx storage.Tx) error {
		key := domain.CorrelationKey{Source: "test", ExternalID: "thread"}
		e := event("a", c.Now())
		e.CorrelationKey = &key
		first, added, err := tx.AcceptEvent(e)
		if err != nil {
			return err
		}
		if !added {
			return fmt.Errorf("first delivery not accepted")
		}
		duplicate := e
		duplicate.ID = "other-id"
		duplicate.Data = domain.Payload{}
		duplicate.CorrelationKey = &domain.CorrelationKey{Source: "test", ExternalID: "different"}
		old, added, err := tx.AcceptEvent(duplicate)
		if err != nil {
			return err
		}
		if added || old.Event.ID != first.Event.ID || old.Event.CorrelationKey.ExternalID != "thread" {
			return fmt.Errorf("duplicate delivery was accepted or mutated")
		}
		e.ID = "b"
		e.DeliveryID = "second"
		if _, added, err = tx.AcceptEvent(e); err != nil {
			return err
		}
		if !added {
			return fmt.Errorf("shared correlation deduplicated distinct event")
		}
		e.ID = "c"
		e.Source = "another-source"
		if _, added, err = tx.AcceptEvent(e); err != nil {
			return err
		}
		if !added {
			return fmt.Errorf("delivery identity lost source namespace")
		}
		rows, err := tx.Events(0, 0)
		if err != nil {
			return err
		}
		if len(rows) != 3 {
			return fmt.Errorf("got %d events, want 3", len(rows))
		}
		for i, row := range rows {
			if i > 0 && row.Sequence <= rows[i-1].Sequence {
				return fmt.Errorf("acceptance order not monotonic")
			}
		}
		page, err := tx.Events(rows[0].Sequence, 1)
		if err != nil {
			return err
		}
		if len(page) != 1 || page[0].Event.ID != "b" {
			return fmt.Errorf("event pagination skipped or duplicated records")
		}
		e.ID = "b"
		e.DeliveryID = "third"
		return wantError(func() error { _, _, err := tx.AcceptEvent(e); return err }(), storage.ErrConflict)
	})
}
func routeReplay(s storage.Store) error {
	c := NewClock()
	if err := seed(s, c); err != nil {
		return err
	}
	err := transact(s, c, func(tx storage.Tx) error {
		if err := tx.MarkRouted("trigger", "work", "queue"); err != nil {
			return err
		}
		return rollback
	})
	if err := wantError(err, rollback); err != nil {
		return err
	}
	return transact(s, c, func(tx storage.Tx) error {
		a, err := tx.Event("trigger")
		if err != nil {
			return err
		}
		if a.Routed {
			return fmt.Errorf("route rollback lost replay checkpoint")
		}
		if err := tx.MarkRouted("trigger", "work", "queue"); err != nil {
			return err
		}
		if err := tx.MarkRouted("trigger", "work", "queue"); err != nil {
			return err
		}
		if err := wantError(tx.MarkRouted("trigger", "work", "drop"), storage.ErrConflict); err != nil {
			return err
		}
		a, _ = tx.Event("trigger")
		if a.Event.WorkID != "" || a.RoutedWorkID != "work" {
			return fmt.Errorf("routing mutated immutable event or lost association")
		}
		return nil
	})
}
func atomicRoute(s storage.Store) error {
	c := NewClock()
	if err := seed(s, c); err != nil {
		return err
	}
	mutate := func(tx storage.Tx) error {
		e := event("new", c.Now())
		if _, _, err := tx.AcceptEvent(e); err != nil {
			return err
		}
		if err := tx.PutWork(domain.Work{ID: "child", Kind: "test.child", ScopeID: "scope", State: domain.WorkReady, ParentID: "work", CorrelationKeys: []domain.CorrelationKey{{Source: "test", ExternalID: "child"}}}, 0); err != nil {
			return err
		}
		if err := tx.Put("test", "checkpoint", []byte("new")); err != nil {
			return err
		}
		if err := tx.PutTimer(domain.Timer{ID: "timer", FireAt: c.Now(), Event: event("timer-event", c.Now())}, 0); err != nil {
			return err
		}
		return tx.MarkRouted("new", "child", "created")
	}
	err := transact(s, c, func(tx storage.Tx) error {
		if err := mutate(tx); err != nil {
			return err
		}
		return rollback
	})
	if err := wantError(err, rollback); err != nil {
		return err
	}
	if err := transact(s, c, func(tx storage.Tx) error {
		if _, err := tx.Event("new"); !errors.Is(err, storage.ErrNotFound) {
			return fmt.Errorf("event leaked from rollback")
		}
		if _, err := tx.WorkByCorrelation(domain.CorrelationKey{Source: "test", ExternalID: "child"}); !errors.Is(err, storage.ErrNotFound) {
			return fmt.Errorf("correlation leaked from rollback")
		}
		if _, err := tx.Get("test", "checkpoint"); !errors.Is(err, storage.ErrNotFound) {
			return fmt.Errorf("KV leaked from rollback")
		}
		if _, err := tx.Timer("timer"); !errors.Is(err, storage.ErrNotFound) {
			return fmt.Errorf("timer leaked from rollback")
		}
		return nil
	}); err != nil {
		return err
	}
	if err := transact(s, c, mutate); err != nil {
		return err
	}
	return transact(s, c, func(tx storage.Tx) error {
		w, err := tx.Work("child")
		if err != nil {
			return err
		}
		a, err := tx.Event("new")
		if err != nil {
			return err
		}
		if w.ParentID != "work" || !a.Routed {
			return fmt.Errorf("route commit incomplete")
		}
		_, err = tx.Get("test", "checkpoint")
		if err != nil {
			return err
		}
		_, err = tx.Timer("timer")
		return err
	})
}
func workRevision(s storage.Store) error {
	c := NewClock()
	if err := seed(s, c); err != nil {
		return err
	}
	var stale domain.Work
	if err := transact(s, c, func(tx storage.Tx) error {
		var err error
		stale, err = tx.Work("work")
		if err != nil {
			return err
		}
		w := stale
		w.CorrelationKeys = append(w.CorrelationKeys, domain.CorrelationKey{Source: "test", ExternalID: "second"})
		return tx.PutWork(w, w.Revision)
	}); err != nil {
		return err
	}
	c.Advance(time.Minute)
	return transact(s, c, func(tx storage.Tx) error {
		if err := wantError(tx.PutWork(stale, stale.Revision), storage.ErrConflict); err != nil {
			return err
		}
		w, err := tx.WorkByCorrelation(domain.CorrelationKey{Source: "test", ExternalID: "second"})
		if err != nil {
			return err
		}
		w.CorrelationKeys[0].ExternalID = "mutated"
		fresh, _ := tx.Work("work")
		if fresh.CorrelationKeys[0].ExternalID != "thread" {
			return fmt.Errorf("read alias mutated stored work")
		}
		if fresh.Revision != 2 || !fresh.CreatedAt.Equal(fresh.UpdatedAt) {
			return fmt.Errorf("revision/timestamp creation contract violated")
		}
		fresh.State = domain.WorkRunning
		if err := tx.PutWork(fresh, fresh.Revision); err != nil {
			return err
		}
		fresh, _ = tx.Work("work")
		if !fresh.UpdatedAt.Equal(c.Now()) || !fresh.CreatedAt.Before(fresh.UpdatedAt) {
			return fmt.Errorf("update timestamp not assigned")
		}
		return nil
	})
}
func competingWork(s storage.Store) error {
	c := NewClock()
	if err := seed(s, c); err != nil {
		return err
	}
	return raceTransactions(s, c, func(tx storage.Tx, i int) error {
		return tx.PutWork(domain.Work{ID: domain.ID(fmt.Sprint("candidate", i)), Kind: "test.objective", ScopeID: "scope", State: domain.WorkReady, CorrelationKeys: []domain.CorrelationKey{{Source: "test", ExternalID: "new-key"}}}, 0)
	}, storage.ErrConflict)
}
func raceTransactions(s storage.Store, c *Clock, fn func(storage.Tx, int) error, loser error) error {
	start := make(chan struct{})
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func(i int) { <-start; results <- transact(s, c, func(tx storage.Tx) error { return fn(tx, i) }) }(i)
	}
	close(start)
	wins, losses := 0, 0
	for i := 0; i < 2; i++ {
		err := <-results
		if err == nil {
			wins++
		} else if errors.Is(err, loser) {
			losses++
		} else {
			return err
		}
	}
	if wins != 1 || losses != 1 {
		return fmt.Errorf("concurrent operations: %d winners, %d rejected; want one each", wins, losses)
	}
	return nil
}
func competingClaims(s storage.Store) error {
	c := NewClock()
	if err := seed(s, c); err != nil {
		return err
	}
	return raceTransactions(s, c, func(tx storage.Tx, i int) error {
		_, err := tx.Claim("execution", fmt.Sprint("owner", i), time.Minute)
		return err
	}, storage.ErrClaimed)
}
func claim(s storage.Store, c *Clock) (domain.LeaseRef, error) {
	var ref domain.LeaseRef
	err := transact(s, c, func(tx storage.Tx) error {
		var err error
		ref, err = tx.Claim("execution", "worker", time.Minute)
		return err
	})
	return ref, err
}
func leaseFencing(s storage.Store) error {
	c := NewClock()
	if err := seed(s, c); err != nil {
		return err
	}
	old, err := claim(s, c)
	if err != nil {
		return err
	}
	c.Advance(time.Minute)
	if err := wantError(s.Transact(context.Background(), storage.Guard{Now: c.Now(), Lease: &old}, func(tx storage.Tx) error { return tx.Put("test", "stale", nil) }), storage.ErrLeaseLost); err != nil {
		return err
	}
	fresh, err := claim(s, c)
	if err != nil {
		return err
	}
	if fresh.Epoch <= old.Epoch {
		return fmt.Errorf("recovery reused fence epoch")
	}
	if err := wantError(s.Transact(context.Background(), storage.Guard{Now: c.Now(), Lease: &old}, func(tx storage.Tx) error { return tx.Put("test", "stale", nil) }), storage.ErrLeaseLost); err != nil {
		return err
	}
	return s.Transact(context.Background(), storage.Guard{Now: c.Now(), Lease: &fresh}, func(tx storage.Tx) error {
		if err := wantError(tx.Renew(old, time.Minute), storage.ErrLeaseLost); err != nil {
			return err
		}
		if err := wantError(tx.Release(old), storage.ErrLeaseLost); err != nil {
			return err
		}
		if _, err := tx.Get("test", "stale"); !errors.Is(err, storage.ErrNotFound) {
			return fmt.Errorf("stale authority wrote KV")
		}
		return tx.Put("test", "fresh", []byte("ok"))
	})
}
func leaseRenew(s storage.Store) error {
	c := NewClock()
	if err := seed(s, c); err != nil {
		return err
	}
	ref, err := claim(s, c)
	if err != nil {
		return err
	}
	c.Advance(30 * time.Second)
	if err := transact(s, c, func(tx storage.Tx) error { return tx.Renew(ref, time.Minute) }); err != nil {
		return err
	}
	c.Advance(30 * time.Second)
	if err := s.Transact(context.Background(), storage.Guard{Now: c.Now(), Lease: &ref}, func(tx storage.Tx) error { return tx.Release(ref) }); err != nil {
		return err
	}
	if err := wantError(s.Transact(context.Background(), storage.Guard{Now: c.Now(), Lease: &ref}, func(storage.Tx) error { return nil }), storage.ErrLeaseLost); err != nil {
		return err
	}
	fresh, err := claim(s, c)
	if err != nil {
		return err
	}
	if fresh.Epoch <= ref.Epoch {
		return fmt.Errorf("release/reclaim reused fence")
	}
	return nil
}
func cancelFencing(s storage.Store) error {
	c := NewClock()
	if err := seed(s, c); err != nil {
		return err
	}
	ref, err := claim(s, c)
	if err != nil {
		return err
	}
	if err := transact(s, c, func(tx storage.Tx) error {
		w, err := tx.Work("work")
		if err != nil {
			return err
		}
		w.State = domain.WorkCancelled
		return tx.PutWork(w, w.Revision)
	}); err != nil {
		return err
	}
	return wantError(s.Transact(context.Background(), storage.Guard{Now: c.Now(), Lease: &ref}, func(tx storage.Tx) error { return tx.Put("test", "late-result", nil) }), storage.ErrLeaseLost)
}
func timerRollback(s storage.Store) error {
	c := NewClock()
	current := domain.Timer{ID: "timer", FireAt: c.Now(), Event: event("tick-1", c.Now())}
	if err := transact(s, c, func(tx storage.Tx) error { return tx.PutTimer(current, 0) }); err != nil {
		return err
	}
	next := current
	next.FireAt = c.Now().Add(time.Hour)
	next.Event = event("tick-2", c.Now())
	err := transact(s, c, func(tx storage.Tx) error {
		if err := tx.FireTimer("timer", 1, &next); err != nil {
			return err
		}
		if err := tx.Put("schedule", "cursor", []byte("2")); err != nil {
			return err
		}
		return rollback
	})
	if err := wantError(err, rollback); err != nil {
		return err
	}
	if err := transact(s, c, func(tx storage.Tx) error {
		v, err := tx.Timer("timer")
		if err != nil {
			return err
		}
		if v.Event.ID != "tick-1" || v.Revision != 1 || eventCount(tx) != 0 {
			return fmt.Errorf("fire/rearm rollback was not atomic")
		}
		return nil
	}); err != nil {
		return err
	}
	if err := transact(s, c, func(tx storage.Tx) error { return tx.FireTimer("timer", 1, &next) }); err != nil {
		return err
	}
	return transact(s, c, func(tx storage.Tx) error {
		v, err := tx.Timer("timer")
		if err != nil {
			return err
		}
		if v.Event.ID != "tick-2" || v.Revision != 2 {
			return fmt.Errorf("timer not rearmed")
		}
		e, err := tx.Event("tick-1")
		if err != nil {
			return err
		}
		if !e.Event.OccurredAt.Equal(current.FireAt) {
			return fmt.Errorf("timer occurrence time changed")
		}
		if err := wantError(tx.FireTimer("timer", 1, &next), storage.ErrConflict); err != nil {
			return err
		}
		return wantError(tx.FireTimer("timer", 2, nil), storage.ErrInvalid)
	})
}
func oneShotTimer(s storage.Store) error {
	c := NewClock()
	return transact(s, c, func(tx storage.Tx) error {
		if err := tx.PutTimer(domain.Timer{ID: "once", FireAt: c.Now(), Event: event("once", c.Now())}, 0); err != nil {
			return err
		}
		if err := tx.FireTimer("once", 1, nil); err != nil {
			return err
		}
		if err := wantError(tx.FireTimer("once", 1, nil), storage.ErrNotFound); err != nil {
			return err
		}
		if eventCount(tx) != 1 {
			return fmt.Errorf("one-shot timer duplicated")
		}
		return nil
	})
}
func park(tx storage.Tx) error {
	x, err := tx.Execution("execution")
	if err != nil {
		return err
	}
	x.State = domain.ExecutionParked
	x.Outcome = &domain.Outcome{Kind: domain.OutcomeWaiting}
	if err := tx.PutExecution(x, x.Revision); err != nil {
		return err
	}
	w, err := tx.Work("work")
	if err != nil {
		return err
	}
	w.State = domain.WorkRunning
	if err := tx.PutWork(w, w.Revision); err != nil {
		return err
	}
	w, err = tx.Work("work")
	if err != nil {
		return err
	}
	w.State = domain.WorkWaiting
	if err := tx.PutWork(w, w.Revision); err != nil {
		return err
	}
	return tx.PutWait(domain.Wait{State: domain.WaitActive, ID: "wait", WorkID: "work", ExecutionID: "execution", Match: domain.EventMatch{Keys: w.CorrelationKeys, Types: []string{"test.changed.v1"}}, AfterSequence: 1}, 0)
}
func waitRegistration(s storage.Store) error {
	c := NewClock()
	if err := seed(s, c); err != nil {
		return err
	}
	ref, err := claim(s, c)
	if err != nil {
		return err
	}
	err = s.Transact(context.Background(), storage.Guard{Now: c.Now(), Lease: &ref}, func(tx storage.Tx) error {
		if err := park(tx); err != nil {
			return err
		}
		return rollback
	})
	if err := wantError(err, rollback); err != nil {
		return err
	}
	if err := s.Transact(context.Background(), storage.Guard{Now: c.Now(), Lease: &ref}, park); err != nil {
		return err
	}
	return transact(s, c, func(tx storage.Tx) error {
		w, err := tx.Wait("wait")
		if err != nil {
			return err
		}
		x, err := tx.Execution("execution")
		if err != nil {
			return err
		}
		work, err := tx.Work("work")
		if err != nil {
			return err
		}
		if x.State != domain.ExecutionParked || x.Lease.Owner != "" || work.State != domain.WorkWaiting || w.AfterSequence != 1 {
			return fmt.Errorf("park/wait registration not atomic")
		}
		bad := w
		bad.ID = "foreign-wait"
		bad.Match.Keys = []domain.CorrelationKey{{Source: "test", ExternalID: "foreign"}}
		return wantError(tx.PutWait(bad, 0), storage.ErrInvalid)
	})
}
func kvCheckpoint(s storage.Store) error {
	c := NewClock()
	if err := transact(s, c, func(tx storage.Tx) error {
		for _, key := range []string{"observation/b", "observation/a", "cursor"} {
			if err := tx.Put("ingress.scope", key, []byte(key)); err != nil {
				return err
			}
		}
		if err := tx.Put("other", "cursor", []byte("isolated")); err != nil {
			return err
		}
		return nil
	}); err != nil {
		return err
	}
	err := transact(s, c, func(tx storage.Tx) error {
		if err := tx.Delete("ingress.scope", "cursor"); err != nil {
			return err
		}
		if _, _, err := tx.AcceptEvent(event("diff", c.Now())); err != nil {
			return err
		}
		return rollback
	})
	if err := wantError(err, rollback); err != nil {
		return err
	}
	return transact(s, c, func(tx storage.Tx) error {
		b, err := tx.Get("ingress.scope", "cursor")
		if err != nil {
			return err
		}
		if string(b) != "cursor" {
			return fmt.Errorf("checkpoint advanced without events")
		}
		b[0] = 'X'
		b, _ = tx.Get("ingress.scope", "cursor")
		if string(b) != "cursor" {
			return fmt.Errorf("KV read alias")
		}
		rows, err := tx.List("ingress.scope", "observation/")
		if err != nil {
			return err
		}
		if len(rows) != 2 || rows[0].Key != "observation/a" || rows[1].Key != "observation/b" {
			return fmt.Errorf("prefix scan not isolated and sorted")
		}
		other, err := tx.Get("other", "cursor")
		if err != nil {
			return err
		}
		if string(other) != "isolated" {
			return fmt.Errorf("namespace collision")
		}
		if err := wantError(tx.Put("", "key", nil), storage.ErrInvalid); err != nil {
			return err
		}
		if eventCount(tx) != 0 {
			return fmt.Errorf("diff event leaked")
		}
		return tx.Delete("ingress.scope", "missing")
	})
}
func cancelRollback(s storage.Store) error {
	c := NewClock()
	ctx, cancel := context.WithCancel(context.Background())
	err := s.Transact(ctx, storage.Guard{Now: c.Now()}, func(tx storage.Tx) error {
		if err := tx.Put("test", "cancelled", nil); err != nil {
			return err
		}
		cancel()
		return nil
	})
	if err := wantError(err, context.Canceled); err != nil {
		return err
	}
	panicked := false
	func() {
		defer func() { panicked = recover() != nil }()
		_ = transact(s, c, func(tx storage.Tx) error {
			if err := tx.Put("test", "panic", nil); err != nil {
				return err
			}
			panic("rollback")
		})
	}()
	if !panicked {
		return fmt.Errorf("callback panic swallowed")
	}
	return transact(s, c, func(tx storage.Tx) error {
		for _, k := range []string{"cancelled", "panic"} {
			if _, err := tx.Get("test", k); !errors.Is(err, storage.ErrNotFound) {
				return fmt.Errorf("%s transaction leaked", k)
			}
		}
		return nil
	})
}
func scopeCycle(s storage.Store) error {
	c := NewClock()
	return transact(s, c, func(tx storage.Tx) error {
		if err := tx.PutScope(domain.Scope{ID: "parent", Name: "parent"}, 0); err != nil {
			return err
		}
		if err := tx.PutScope(domain.Scope{ID: "child", Name: "child", ParentID: "parent"}, 0); err != nil {
			return err
		}
		parent, err := tx.Scope("parent")
		if err != nil {
			return err
		}
		parent.ParentID = "child"
		if err := wantError(tx.PutScope(parent, parent.Revision), storage.ErrInvalid); err != nil {
			return err
		}
		return wantError(tx.PutScope(parent, 99), storage.ErrConflict)
	})
}
func attempts(s storage.Store) error {
	c := NewClock()
	if err := seed(s, c); err != nil {
		return err
	}
	if _, err := claim(s, c); err != nil {
		return err
	}
	return transact(s, c, func(tx storage.Tx) error {
		x, err := tx.Execution("execution")
		if err != nil {
			return err
		}
		// A failed attempt does not implicitly fail its objective.
		x.State = domain.ExecutionFailed
		x.Outcome = &domain.Outcome{Kind: domain.OutcomeFailed, Summary: "retryable"}
		if err := tx.PutExecution(x, x.Revision); err != nil {
			return err
		}
		w, err := tx.Work("work")
		if err != nil {
			return err
		}
		if w.State.Terminal() {
			return fmt.Errorf("attempt failure terminated Work")
		}
		next := domain.Execution{ID: "next", WorkID: "work", Attempt: 3, ExecutorBinding: "shell", State: domain.ExecutionPending, TriggerID: "trigger"}
		if err := wantError(tx.PutExecution(next, 0), storage.ErrConflict); err != nil {
			return err
		}
		next.Attempt = 2
		if err := tx.PutExecution(next, 0); err != nil {
			return err
		}
		if _, err := tx.Claim("next", "worker", time.Minute); err != nil {
			return err
		}
		x, err = tx.Execution("next")
		if err != nil {
			return err
		}
		x.State = domain.ExecutionFailed
		x.Outcome = &domain.Outcome{Kind: domain.OutcomeFailed}
		if err := tx.PutExecution(x, x.Revision); err != nil {
			return err
		}
		w.State = domain.WorkFailed
		if err := tx.PutWork(w, w.Revision); err != nil {
			return err
		}
		w, err = tx.Work("work")
		if err != nil {
			return err
		}
		w.State = domain.WorkReady
		if err := wantError(tx.PutWork(w, w.Revision), storage.ErrInvalid); err != nil {
			return err
		}
		next.ID = "third"
		next.Attempt = 3
		return wantError(tx.PutExecution(next, 0), storage.ErrInvalid)
	})
}
func routingOrder(s storage.Store) error {
	c := NewClock()
	if err := seed(s, c); err != nil {
		return err
	}
	return transact(s, c, func(tx storage.Tx) error {
		first := event("first", c.Now())
		first.CorrelationKey = &domain.CorrelationKey{Source: "test", ExternalID: "thread"}
		if _, _, err := tx.AcceptEvent(first); err != nil {
			return err
		}
		second := first
		second.ID = "second"
		second.DeliveryID = "second"
		if _, _, err := tx.AcceptEvent(second); err != nil {
			return err
		}
		if err := wantError(tx.MarkRouted("second", "work", "queue"), storage.ErrConflict); err != nil {
			return err
		}
		// Uncorrelated trigger is not globally ahead of independent per-work routes.
		if err := tx.MarkRouted("first", "work", "queue"); err != nil {
			return err
		}
		return tx.MarkRouted("second", "work", "drop")
	})
}

func eventCount(tx storage.Tx) int {
	rows, err := tx.Events(0, 0)
	if err != nil {
		return -1
	}
	return len(rows)
}

func inventoryAndExecutionCAS(s storage.Store) error {
	c := NewClock()
	if err := seed(s, c); err != nil {
		return err
	}
	if _, err := claim(s, c); err != nil {
		return err
	}
	return transact(s, c, func(tx storage.Tx) error {
		scopes, err := tx.ListScopes()
		if err != nil {
			return err
		}
		works, err := tx.ListWorks()
		if err != nil {
			return err
		}
		runs, err := tx.ListExecutions("")
		if err != nil {
			return err
		}
		if len(scopes) != 1 || len(works) != 1 || len(runs) != 1 || runs[0].StartedAt == nil || !runs[0].StartedAt.Equal(c.Now()) {
			return fmt.Errorf("recovery enumeration lost records or claim timestamp")
		}
		none, err := tx.ListExecutions("foreign")
		if err != nil {
			return err
		}
		if len(none) != 0 {
			return fmt.Errorf("execution scan leaked unrelated work")
		}
		x := runs[0]
		if err := wantError(tx.PutExecution(x, x.Revision-1), storage.ErrConflict); err != nil {
			return err
		}
		x.State = domain.ExecutionCompleted
		x.Outcome = &domain.Outcome{Kind: domain.OutcomeCompleted}
		x.Artifacts = []domain.ArtifactRef{{ID: "artifact", Size: 12, MediaType: "text/plain"}}
		if err := tx.PutExecution(x, x.Revision); err != nil {
			return err
		}
		done, err := tx.Execution(x.ID)
		if err != nil {
			return err
		}
		if done.FinishedAt == nil || !done.FinishedAt.Equal(c.Now()) || len(done.Artifacts) != 1 {
			return fmt.Errorf("completion timestamp/artifact reference missing")
		}
		done.State = domain.ExecutionRunning
		return wantError(tx.PutExecution(done, done.Revision), storage.ErrInvalid)
	})
}
func waitWakeAndTimerCancel(s storage.Store) error {
	c := NewClock()
	if err := seed(s, c); err != nil {
		return err
	}
	if _, err := claim(s, c); err != nil {
		return err
	}
	if err := transact(s, c, park); err != nil {
		return err
	}
	return transact(s, c, func(tx storage.Tx) error {
		waits, err := tx.ListWaits("work")
		if err != nil {
			return err
		}
		if len(waits) != 1 || waits[0].State != domain.WaitActive {
			return fmt.Errorf("active wait missing from recovery scan")
		}
		wait := waits[0]
		e := event("wake", c.Now())
		e.CorrelationKey = &wait.Match.Keys[0]
		if _, _, err := tx.AcceptEvent(e); err != nil {
			return err
		}
		wait.State = domain.WaitWoken
		wait.WakeEventID = e.ID
		if err := tx.PutWait(wait, wait.Revision); err != nil {
			return err
		}
		wait, err = tx.Wait(wait.ID)
		if err != nil {
			return err
		}
		if err := wantError(tx.PutWait(wait, wait.Revision), storage.ErrInvalid); err != nil {
			return err
		}
		timer := domain.Timer{ID: "cancel-timeout", FireAt: c.Now().Add(time.Hour), Event: event("timeout", c.Now())}
		if err := tx.PutTimer(timer, 0); err != nil {
			return err
		}
		if err := wantError(tx.DeleteTimer(timer.ID, 2), storage.ErrConflict); err != nil {
			return err
		}
		if err := tx.DeleteTimer(timer.ID, 1); err != nil {
			return err
		}
		timers, err := tx.ListTimers()
		if err != nil {
			return err
		}
		if len(timers) != 0 {
			return fmt.Errorf("cancelled timeout remained armed")
		}
		if err := tx.PutTimer(timer, 0); err != nil {
			return err
		}
		recreated, err := tx.Timer(timer.ID)
		if err != nil {
			return err
		}
		if recreated.Revision <= 1 {
			return fmt.Errorf("timer recreation reused a stale revision")
		}
		return wantError(tx.FireTimer(timer.ID, 1, nil), storage.ErrConflict)
	})
}
func terminalCorrelationAndCausation(s storage.Store) error {
	c := NewClock()
	if err := seed(s, c); err != nil {
		return err
	}
	return transact(s, c, func(tx storage.Tx) error {
		e := event("derived", c.Now())
		e.CausationID = "missing"
		if _, _, err := tx.AcceptEvent(e); !errors.Is(err, storage.ErrNotFound) {
			return fmt.Errorf("dangling causation accepted: %v", err)
		}
		e.CausationID = "trigger"
		if _, _, err := tx.AcceptEvent(e); err != nil {
			return err
		}
		w, err := tx.Work("work")
		if err != nil {
			return err
		}
		w.State = domain.WorkCancelled
		if err := tx.PutWork(w, w.Revision); err != nil {
			return err
		}
		other := w
		other.ID = "replacement"
		other.State = domain.WorkReady
		if err := wantError(tx.PutWork(other, 0), storage.ErrConflict); err != nil {
			return err
		}
		owner, err := tx.WorkByCorrelation(w.CorrelationKeys[0])
		if err != nil {
			return err
		}
		if owner.ID != w.ID || owner.State != domain.WorkCancelled {
			return fmt.Errorf("terminal correlation ownership lost")
		}
		return nil
	})
}

func handoffAndWait(s storage.Store) error {
	c := NewClock()
	if err := seed(s, c); err != nil {
		return err
	}
	return transact(s, c, func(tx storage.Tx) error {
		for _, terminal := range []domain.ExecutionState{domain.ExecutionHandoff, domain.ExecutionWaiting} {
			id := domain.ID(terminal)
			if err := tx.PutWork(domain.Work{ID: id, Kind: "test.objective", ScopeID: "scope", State: domain.WorkReady}, 0); err != nil {
				return err
			}
			x := domain.Execution{ID: id, WorkID: id, ExecutorBinding: "test", Attempt: 1, State: domain.ExecutionPending, TriggerID: "trigger"}
			if err := tx.PutExecution(x, 0); err != nil {
				return err
			}
			ref, err := tx.Claim(id, "worker", time.Minute)
			if err != nil {
				return err
			}
			x, err = tx.Execution(id)
			if err != nil {
				return err
			}
			x.State = terminal
			x.Outcome = &domain.Outcome{Kind: domain.OutcomeKind(terminal), Summary: "continue later"}
			if err := tx.PutExecution(x, x.Revision); err != nil {
				return err
			}
			if err := wantError(tx.Renew(ref, time.Minute), storage.ErrLeaseLost); err != nil {
				return err
			}
			w, err := tx.Work(id)
			if err != nil {
				return err
			}
			w.State = domain.WorkReady
			if err := tx.PutWork(w, w.Revision); err != nil {
				return err
			}
			x.ID += "-next"
			x.Attempt = 2
			x.State = domain.ExecutionPending
			x.Outcome = nil
			x.Lease = domain.Lease{}
			if err := tx.PutExecution(x, 0); err != nil {
				return err
			}
		}
		return nil
	})
}
