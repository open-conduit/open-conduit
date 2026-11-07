// Package conformance supplies reusable contract cases independent of backends.
package conformance

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/storage"
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

var errRollback = errors.New("injected rollback")

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
		{"work_display_and_wait_description", displayAndDescription},
		{"recovery_inventory_and_execution_revision", inventoryAndExecutionCAS},
		{"wait_wake_and_timeout_cancellation", waitWakeAndTimerCancel},
		{"terminal_correlation_and_causation", terminalCorrelationAndCausation},
		{"event_identity_not_correlation", eventIdentity},
		{"event_actor_round_trip", eventActor},
		{"acceptance_separate_from_routing", routeReplay},
		{"atomic_route_work_kv_timer", atomicRoute},
		{"correlation_revision_and_copy_isolation", workRevision},
		{"competing_correlation_owners", competingWork},
		{"competing_claims", competingClaims},
		{"lease_expiry_recovery_and_fencing", leaseFencing},
		{"lease_renew_release", leaseRenew},
		{"lease_renew_survives_clock_step_back", leaseRenewStepBack},
		{"terminal_work_cancellation_fences_worker", cancelFencing},
		{"timer_fire_rearm_rollback", timerRollback},
		{"one_shot_timer_and_duplicate_fire", oneShotTimer},
		{"wait_registration_atomic_with_park", waitRegistration},
		{"namespaced_kv_and_checkpoint_rollback", kvCheckpoint},
		{"context_cancellation_and_panic_rollback", cancelRollback},
		{"scope_parent_cycle_and_revision", scopeCycle},
		{"attempt_sequence_and_retry_exhaustion", attempts},
		{"per_work_routing_order", routingOrder},
		{"live_reads_follow_state", liveReads},
		{"work_events_follow_naming_and_routing", workEvents},
		{"event_scope_recorded_once_and_indexed", scopeEvents},
		{"work_queries_select_order_page_and_count", workQueries},
		{"view_reads_committed_state_alongside_writes", viewAlongsideWrites},
	}
}

// eventActor checks that an Event's Actor, part of its immutable envelope,
// survives acceptance and every read, in the accepting transaction and after.
func eventActor(s storage.Store) error {
	c := NewClock()
	e := event("acted", c.Now())
	e.Actor = "operator:alice"
	check := func(where string, got domain.AcceptedEvent) error {
		if got.Event.Actor != e.Actor {
			return fmt.Errorf("event actor lost %s: got %q, want %q", where, got.Event.Actor, e.Actor)
		}
		return nil
	}
	if err := transact(s, c, func(tx storage.Tx) error {
		accepted, _, err := tx.AcceptEvent(e)
		if err != nil {
			return err
		}
		return check("on acceptance", accepted)
	}); err != nil {
		return err
	}
	return transact(s, c, func(tx storage.Tx) error {
		got, err := tx.Event(e.ID)
		if err != nil {
			return err
		}
		if err = check("by Event", got); err != nil {
			return err
		}
		rows, err := tx.Events(0, 0)
		if err != nil {
			return err
		}
		if len(rows) != 1 {
			return fmt.Errorf("events: got %d, want 1", len(rows))
		}
		if err = check("by Events", rows[0]); err != nil {
			return err
		}
		dup, added, err := tx.AcceptEvent(event("acted", c.Now()))
		if err != nil {
			return err
		}
		if added {
			return fmt.Errorf("duplicate delivery accepted")
		}
		return check("on a duplicate delivery", dup)
	})
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
		if latest, err := tx.LatestSequence(); err != nil || latest != rows[2].Sequence {
			return fmt.Errorf("latest sequence %d, want %d: %v", latest, rows[2].Sequence, err)
		}
		if found, err := tx.EventByDelivery("another-source", "second"); err != nil || found.Event.ID != "c" {
			return fmt.Errorf("delivery lookup lost source namespace: %v", err)
		}
		if err := wantError(func() error { _, err := tx.EventByDelivery("test", "missing"); return err }(), storage.ErrNotFound); err != nil {
			return err
		}
		e.ID = "b"
		e.DeliveryID = "third"
		return wantError(func() error { _, _, err := tx.AcceptEvent(e); return err }(), storage.ErrConflict)
	})
}
func hasEvent(events []domain.AcceptedEvent, id domain.ID) bool {
	for _, a := range events {
		if a.Event.ID == id {
			return true
		}
	}
	return false
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
		return errRollback
	})
	if err := wantError(err, errRollback); err != nil {
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
		unrouted, err := tx.UnroutedEvents(0, 0)
		if err != nil {
			return err
		}
		if !hasEvent(unrouted, "trigger") {
			return fmt.Errorf("unrouted events omitted an unrouted event")
		}
		if err := tx.MarkRouted("trigger", "work", "queue"); err != nil {
			return err
		}
		if err := tx.MarkRouted("trigger", "work", "queue"); err != nil {
			return err
		}
		if unrouted, err = tx.UnroutedEvents(0, 0); err != nil {
			return err
		}
		if hasEvent(unrouted, "trigger") {
			return fmt.Errorf("unrouted events kept a routed event")
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
		return errRollback
	})
	if err := wantError(err, errRollback); err != nil {
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

// leaseRenewStepBack: a wall clock stepped back while the lease is held
// (an NTP correction, a VM resume) neither fails the renewal nor shortens
// the stored expiry, so the holder keeps its authority.
func leaseRenewStepBack(s storage.Store) error {
	c := NewClock()
	if err := seed(s, c); err != nil {
		return err
	}
	ref, err := claim(s, c)
	if err != nil {
		return err
	}
	held := c.Now().Add(time.Minute)
	c.Advance(-30 * time.Second)
	if err := s.Transact(context.Background(), storage.Guard{Now: c.Now(), Lease: &ref}, func(tx storage.Tx) error { return tx.Renew(ref, time.Minute) }); err != nil {
		return fmt.Errorf("renewal after clock step back: %w", err)
	}
	var expires time.Time
	if err := transact(s, c, func(tx storage.Tx) error {
		x, err := tx.Execution("execution")
		expires = x.Lease.ExpiresAt
		return err
	}); err != nil {
		return err
	}
	if expires.Before(held) {
		return fmt.Errorf("renewal after clock step back shortened the lease to %s, held until %s", expires, held)
	}
	c.Advance(time.Minute)
	if err := s.Transact(context.Background(), storage.Guard{Now: c.Now(), Lease: &ref}, func(tx storage.Tx) error { return tx.Renew(ref, time.Minute) }); err != nil {
		return fmt.Errorf("renewal once the clock caught up: %w", err)
	}
	return nil
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
		return errRollback
	})
	if err := wantError(err, errRollback); err != nil {
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
		return errRollback
	})
	if err := wantError(err, errRollback); err != nil {
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
		return errRollback
	})
	if err := wantError(err, errRollback); err != nil {
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

// displayAndDescription: a Work's display round-trips and can be replaced
// like any other field; a Wait's description and owner round-trip and are
// fixed once the Wait is stored, and only an owned active Wait's progress
// changes.
func displayAndDescription(s storage.Store) error {
	c := NewClock()
	if err := seed(s, c); err != nil {
		return err
	}
	shown := &domain.WorkDisplay{Title: "Fix the example widget", Ref: "#1", Requester: "example-user", Links: []domain.Link{{Label: "Issue #1", URL: "https://tracker.example/issues/1"}}}
	if err := transact(s, c, func(tx storage.Tx) error {
		w, err := tx.Work("work")
		if err != nil {
			return err
		}
		if w.Display != nil {
			return fmt.Errorf("seeded Work has a display: %+v", w.Display)
		}
		w.Display = shown
		return tx.PutWork(w, w.Revision)
	}); err != nil {
		return err
	}
	if err := transact(s, c, func(tx storage.Tx) error {
		w, err := tx.Work("work")
		if err != nil {
			return err
		}
		if w.Display == nil || !reflect.DeepEqual(*w.Display, *shown) {
			return fmt.Errorf("display did not round-trip: %+v", w.Display)
		}
		// A stored record is a copy: changing what was read changes nothing.
		w.Display.Links[0].URL = "https://tracker.example/other"
		again, err := tx.Work("work")
		if err != nil || again.Display.Links[0].URL != shown.Links[0].URL {
			return fmt.Errorf("display shares memory with the store: %+v %v", again.Display, err)
		}
		return nil
	}); err != nil {
		return err
	}
	ref, err := claim(s, c)
	if err != nil {
		return err
	}
	if err := s.Transact(context.Background(), storage.Guard{Now: c.Now(), Lease: &ref}, func(tx storage.Tx) error {
		if err := park(tx); err != nil {
			return err
		}
		return nil
	}); err != nil {
		return err
	}
	return transact(s, c, func(tx storage.Tx) error {
		w, err := tx.Wait("wait")
		if err != nil {
			return err
		}
		// park stores no description; a described Wait is a new one.
		described := w
		described.ID, described.Revision, described.Description = "described-wait", 0, "a reply in the example thread"
		if err := tx.PutWait(described, 0); err != nil {
			return err
		}
		got, err := tx.Wait("described-wait")
		if err != nil || got.Description != described.Description {
			return fmt.Errorf("description did not round-trip: %q %v", got.Description, err)
		}
		got.Description, got.State = "something else", domain.WaitCancelled
		if err := wantError(tx.PutWait(got, got.Revision), storage.ErrInvalid); err != nil {
			return err
		}
		// Only an owned Wait's progress changes while it is active, and it
		// freezes once the Wait is cancelled.
		got, err = tx.Wait("described-wait")
		if err != nil {
			return err
		}
		got.Progress = "1 of 2 example steps"
		if err := wantError(tx.PutWait(got, got.Revision), storage.ErrInvalid); err != nil {
			return fmt.Errorf("unowned progress: %w", err)
		}
		owned := w
		owned.ID, owned.Revision, owned.Owner = "owned-wait", 0, "example"
		if err := tx.PutWait(owned, 0); err != nil {
			return err
		}
		owned, err = tx.Wait("owned-wait")
		if err != nil {
			return err
		}
		owned.Progress = "1 of 2 example steps"
		if err := tx.PutWait(owned, owned.Revision); err != nil {
			return fmt.Errorf("progress: %w", err)
		}
		owned, err = tx.Wait("owned-wait")
		if err != nil || owned.Progress != "1 of 2 example steps" || owned.Owner != "example" {
			return fmt.Errorf("progress did not round-trip: %+v %v", owned, err)
		}
		changed := owned
		changed.Owner = "other"
		if err := wantError(tx.PutWait(changed, changed.Revision), storage.ErrInvalid); err != nil {
			return fmt.Errorf("owner change: %w", err)
		}
		owned.State = domain.WaitCancelled
		if err := tx.PutWait(owned, owned.Revision); err != nil {
			return err
		}
		owned, err = tx.Wait("owned-wait")
		if err != nil || owned.Progress != "1 of 2 example steps" {
			return fmt.Errorf("cancel did not keep progress: %+v %v", owned, err)
		}
		owned.Progress = "2 of 2 example steps"
		return wantError(tx.PutWait(owned, owned.Revision), storage.ErrInvalid)
	})
}

// liveReads: LiveWorks, LiveExecutions and ActiveWaits return exactly the
// unfinished Work, unfinished attempts and active Waits as states change,
// so the host's hot paths can read them instead of every record.
func liveReads(s storage.Store) error {
	c := NewClock()
	if err := seed(s, c); err != nil {
		return err
	}
	ids := func(tx storage.Tx) (string, error) {
		works, err := tx.LiveWorks()
		if err != nil {
			return "", err
		}
		xs, err := tx.LiveExecutions()
		if err != nil {
			return "", err
		}
		waits, err := tx.ActiveWaits()
		if err != nil {
			return "", err
		}
		var parts []string
		for _, w := range works {
			parts = append(parts, "work:"+string(w.ID))
		}
		for _, x := range xs {
			parts = append(parts, "execution:"+string(x.ID)+":"+string(x.State))
		}
		for _, w := range waits {
			parts = append(parts, "wait:"+string(w.ID))
		}
		return strings.Join(parts, " "), nil
	}
	want := func(expected string) error {
		return transact(s, c, func(tx storage.Tx) error {
			got, err := ids(tx)
			if err == nil && got != expected {
				err = fmt.Errorf("live reads %q, want %q", got, expected)
			}
			return err
		})
	}
	if err := transact(s, c, func(tx storage.Tx) error {
		if err := tx.PutWork(domain.Work{ID: "done", Kind: "test.objective", ScopeID: "scope", State: domain.WorkReady}, 0); err != nil {
			return err
		}
		w, err := tx.Work("done")
		if err != nil {
			return err
		}
		w.State = domain.WorkCancelled
		return tx.PutWork(w, w.Revision)
	}); err != nil {
		return err
	}
	if err := want("work:work execution:execution:pending"); err != nil {
		return err
	}
	if _, err := claim(s, c); err != nil {
		return err
	}
	if err := transact(s, c, park); err != nil {
		return err
	}
	if err := want("work:work execution:execution:parked wait:wait"); err != nil {
		return err
	}
	if err := transact(s, c, func(tx storage.Tx) error {
		wait, err := tx.Wait("wait")
		if err != nil {
			return err
		}
		e := event("wake", c.Now())
		e.CorrelationKey = &wait.Match.Keys[0]
		if _, _, err := tx.AcceptEvent(e); err != nil {
			return err
		}
		wait.State = domain.WaitWoken
		wait.WakeEventID = e.ID
		return tx.PutWait(wait, wait.Revision)
	}); err != nil {
		return err
	}
	if err := want("work:work execution:execution:parked"); err != nil {
		return err
	}
	if err := transact(s, c, func(tx storage.Tx) error {
		x, err := tx.Execution("execution")
		if err != nil {
			return err
		}
		x.State = domain.ExecutionCancelled
		x.Outcome = nil
		if err := tx.PutExecution(x, x.Revision); err != nil {
			return err
		}
		w, err := tx.Work("work")
		if err != nil {
			return err
		}
		w.State = domain.WorkCancelled
		return tx.PutWork(w, w.Revision)
	}); err != nil {
		return err
	}
	return want("")
}

// workEvents: WorkEvents returns exactly the Events naming the Work or
// routed to it, once each, in acceptance order after the cursor and within
// the limit, so a view of one Work reads its own events instead of the log.
func workEvents(s storage.Store) error {
	c := NewClock()
	if err := seed(s, c); err != nil {
		return err
	}
	if err := transact(s, c, func(tx storage.Tx) error {
		if err := tx.PutWork(domain.Work{ID: "other", Kind: "test.objective", ScopeID: "scope", State: domain.WorkReady}, 0); err != nil {
			return err
		}
		named := event("named", c.Now())
		named.WorkID = "work"
		elsewhere := event("elsewhere", c.Now())
		elsewhere.WorkID = "other"
		for _, e := range []domain.Event{named, event("unrelated", c.Now()), elsewhere, event("routed", c.Now()), event("routed-away", c.Now())} {
			if _, _, err := tx.AcceptEvent(e); err != nil {
				return err
			}
		}
		// named is routed to its own Work too, and must still come once.
		// Events are routed in order: an earlier unrouted input of a Work
		// blocks marking a later one routed to it.
		for _, r := range []struct{ event, work domain.ID }{{"named", "work"}, {"elsewhere", "other"}, {"routed", "work"}, {"routed-away", "other"}, {"unrelated", ""}} {
			disposition := "queue"
			if r.work == "" {
				disposition = "ignored"
			}
			if err := tx.MarkRouted(r.event, r.work, disposition); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return err
	}
	return transact(s, c, func(tx storage.Tx) error {
		ids := func(work domain.ID, after uint64, limit int) (string, []domain.AcceptedEvent, error) {
			got, err := tx.WorkEvents(work, after, limit)
			var parts []string
			for _, a := range got {
				parts = append(parts, string(a.Event.ID))
			}
			return strings.Join(parts, " "), got, err
		}
		all, events, err := ids("work", 0, 0)
		if err != nil {
			return err
		}
		if all != "named routed" {
			return fmt.Errorf("work events %q, want %q", all, "named routed")
		}
		for _, q := range []struct {
			work        domain.ID
			after       uint64
			limit       int
			want, label string
		}{
			{"work", events[0].Sequence, 0, "routed", "after the first"},
			{"work", 0, 1, "named", "limited to one"},
			{"other", 0, 0, "elsewhere routed-away", "of the other Work"},
			{"missing", 0, 0, "", "of a missing Work"},
			{"", 0, 0, "", "of no Work"},
		} {
			got, _, err := ids(q.work, q.after, q.limit)
			if err != nil {
				return err
			}
			if got != q.want {
				return fmt.Errorf("work events %s %q, want %q", q.label, got, q.want)
			}
		}
		return nil
	})
}

// scopeEvents: SetEventScope records an Event's scope once, the record
// keeps it through every rewrite (routing, a duplicate delivery), and
// ScopeEvents returns exactly the Events recorded in the scopes asked for,
// with "" standing for those not recorded yet, in acceptance order after the
// cursor and within the limit.
func scopeEvents(s storage.Store) error {
	c := NewClock()
	if err := seed(s, c); err != nil {
		return err
	}
	if err := transact(s, c, func(tx storage.Tx) error {
		if err := tx.PutScope(domain.Scope{ID: "other", Name: "other"}, 0); err != nil {
			return err
		}
		for _, id := range []domain.ID{"mine", "unscoped", "theirs", "later"} {
			if _, _, err := tx.AcceptEvent(event(id, c.Now())); err != nil {
				return err
			}
		}
		for _, r := range []struct{ event, scope domain.ID }{{"trigger", "scope"}, {"mine", "scope"}, {"theirs", "other"}, {"later", "scope"}} {
			if err := tx.SetEventScope(r.event, r.scope); err != nil {
				return fmt.Errorf("set event scope %s: %w", r.event, err)
			}
		}
		for _, q := range []struct {
			event, scope domain.ID
			want         error
			label        string
		}{
			{"mine", "scope", nil, "the same scope again"},
			{"mine", "other", storage.ErrConflict, "another scope"},
			{"unscoped", "", storage.ErrInvalid, "an empty scope"},
			{"missing", "scope", storage.ErrNotFound, "a missing event"},
			{"unscoped", "missing", storage.ErrNotFound, "a missing scope"},
		} {
			if err := tx.SetEventScope(q.event, q.scope); q.want == nil && err != nil || q.want != nil && !errors.Is(err, q.want) {
				return fmt.Errorf("scope events: set event scope to %s: %v, want %v", q.label, err, q.want)
			}
		}
		// Routing rewrites the record; the scope must survive it.
		return tx.MarkRouted("mine", "", "ignored")
	}); err != nil {
		return err
	}
	return transact(s, c, func(tx storage.Tx) error {
		if a, err := tx.Event("mine"); err != nil || a.ScopeID != "scope" || !a.Routed {
			return fmt.Errorf("scope events: recorded scope %q routed %v after routing (%v), want %q", a.ScopeID, a.Routed, err, "scope")
		}
		if a, added, err := tx.AcceptEvent(event("theirs", c.Now())); err != nil || added || a.ScopeID != "other" {
			return fmt.Errorf("scope events: duplicate delivery scope %q added %v (%v), want %q", a.ScopeID, added, err, "other")
		}
		if a, err := tx.Event("unscoped"); err != nil || a.ScopeID != "" {
			return fmt.Errorf("scope events: unrecorded scope %q (%v), want none", a.ScopeID, err)
		}
		ids := func(scopes []domain.ID, after uint64, limit int) (string, []domain.AcceptedEvent, error) {
			got, err := tx.ScopeEvents(scopes, after, limit)
			var parts []string
			for _, a := range got {
				parts = append(parts, string(a.Event.ID))
			}
			return strings.Join(parts, " "), got, err
		}
		all, events, err := ids([]domain.ID{"scope", ""}, 0, 0)
		if err != nil {
			return err
		}
		if want := "trigger mine unscoped later"; all != want {
			return fmt.Errorf("scope events of a scope and the unrecorded %q, want %q", all, want)
		}
		for _, q := range []struct {
			scopes      []domain.ID
			after       uint64
			limit       int
			want, label string
		}{
			{[]domain.ID{"scope"}, 0, 0, "trigger mine later", "of one scope"},
			{[]domain.ID{""}, 0, 0, "unscoped", "not recorded"},
			{[]domain.ID{"other", "scope"}, 0, 0, "trigger mine theirs later", "of two scopes"},
			{[]domain.ID{"scope", "scope"}, 0, 0, "trigger mine later", "of a scope named twice"},
			{[]domain.ID{"scope", ""}, events[1].Sequence, 0, "unscoped later", "after the second"},
			{[]domain.ID{"scope", ""}, events[1].Sequence, 1, "unscoped", "after the second, limited to one"},
			{[]domain.ID{"missing"}, 0, 0, "", "of a missing scope"},
			{nil, 0, 0, "", "of no scope"},
		} {
			got, _, err := ids(q.scopes, q.after, q.limit)
			if err != nil {
				return err
			}
			if got != q.want {
				return fmt.Errorf("scope events %s %q, want %q", q.label, got, q.want)
			}
		}
		return nil
	})
}

// viewWait bounds how long a View may take while a Transact is open: a View
// never waits for one, so it is a deadline on a condition, not a delay.
const viewWait = 3 * time.Second

// viewAlongsideWrites: storage.View reads committed state and refuses every
// write, keeping none. A Viewer's View, while a Transact is open, neither
// waits for it nor sees its writes, which a View after the commit does see:
// operator reads go through it, so a read never queues behind the
// runtime's writes.
func viewAlongsideWrites(s storage.Store) error {
	c := NewClock()
	if err := seed(s, c); err != nil {
		return err
	}
	ctx := context.Background()
	if err := storage.View(ctx, s, c.Now(), func(tx storage.Tx) error {
		if w, err := tx.Work("work"); err != nil || w.ID != "work" {
			return fmt.Errorf("view of committed Work: %+v %v", w, err)
		}
		named := event("in-view", c.Now())
		_, _, acceptErr := tx.AcceptEvent(named)
		w, _ := tx.Work("work")
		for name, err := range map[string]error{
			"Put":         tx.Put("view", "key", []byte("v")),
			"Delete":      tx.Delete("view", "key"),
			"AcceptEvent": acceptErr,
			"MarkRouted":  tx.MarkRouted("trigger", "work", "queue"),
			"PutWork":     tx.PutWork(w, w.Revision),
			"PutScope":    tx.PutScope(domain.Scope{ID: "view-scope", Name: "view"}, 0),
		} {
			if !errors.Is(err, storage.ErrReadOnly) {
				return fmt.Errorf("view write %s: got %v, want ErrReadOnly", name, err)
			}
		}
		return nil
	}); err != nil {
		return err
	}
	if err := transact(s, c, func(tx storage.Tx) error {
		if _, err := tx.Get("view", "key"); !errors.Is(err, storage.ErrNotFound) {
			return fmt.Errorf("view write was kept: %v", err)
		}
		if _, err := tx.Event("in-view"); !errors.Is(err, storage.ErrNotFound) {
			return fmt.Errorf("view event was kept: %v", err)
		}
		return nil
	}); err != nil {
		return err
	}

	// A store that is no Viewer reads in a Transact, which waits its turn.
	v, ok := s.(storage.Viewer)
	if !ok {
		return nil
	}
	// Hold a Transact open with a write in it.
	inside, release := make(chan struct{}), make(chan struct{})
	committed := make(chan error, 1)
	go func() {
		committed <- transact(s, c, func(tx storage.Tx) error {
			if err := tx.Put("view", "pending", []byte("v")); err != nil {
				return err
			}
			close(inside)
			<-release
			return nil
		})
	}()
	select {
	case <-inside:
	case err := <-committed:
		return fmt.Errorf("writer ended early: %v", err)
	}
	viewed := make(chan error, 1)
	go func() {
		viewed <- v.View(ctx, func(tx storage.Tx) error {
			if _, err := tx.Get("view", "pending"); !errors.Is(err, storage.ErrNotFound) {
				return fmt.Errorf("view saw a write not yet committed: %v", err)
			}
			_, err := tx.Work("work")
			return err
		})
	}()
	var err error
	select {
	case err = <-viewed:
	case <-time.After(viewWait):
		err = fmt.Errorf("view waited for a write to commit")
	}
	close(release)
	if e := <-committed; e != nil {
		return e
	}
	if err != nil {
		return err
	}
	return v.View(ctx, func(tx storage.Tx) error {
		if _, err := tx.Get("view", "pending"); err != nil {
			return fmt.Errorf("view after the commit: %v", err)
		}
		return nil
	})
}

// workQueries checks QueryWorks, CountWorks and WorkFacets against the
// Work they select: each filter, both orders both ways with ties broken by
// ID and times compared across zones, keyset paging, counts and facets;
// that the executor is the latest attempt's; and that an attempt's
// recorded executor is write-once and survives updates that omit it.
func workQueries(s storage.Store) error {
	c := NewClock()
	t0 := c.Now()
	if err := seed(s, c); err != nil {
		return err
	}
	at := func(d time.Duration, zone *time.Location, fn func(storage.Tx) error) error {
		return s.Transact(context.Background(), storage.Guard{Now: t0.Add(d).In(zone)}, fn)
	}
	attempt := func(tx storage.Tx, work domain.ID, n uint32, executor string) error {
		id := domain.ID(fmt.Sprintf("%s/attempt/%d", work, n))
		if err := tx.PutExecution(domain.Execution{ID: id, WorkID: work, ExecutorBinding: "run", Executor: executor, Attempt: n, State: domain.ExecutionPending, TriggerID: "trigger"}, 0); err != nil {
			return err
		}
		x, err := tx.Execution(id)
		if err != nil {
			return err
		}
		if x.Executor != executor {
			return fmt.Errorf("work queries: attempt recorded executor %q, want %q", x.Executor, executor)
		}
		x.State, x.Executor = domain.ExecutionCancelled, ""
		return tx.PutExecution(x, x.Revision)
	}
	finish := func(tx storage.Tx, id domain.ID, state domain.WorkState) error {
		w, err := tx.Work(id)
		if err != nil {
			return err
		}
		w.State = state
		return tx.PutWork(w, w.Revision)
	}
	east := time.FixedZone("east", 5*3600)
	github := []domain.CorrelationKey{{Source: "github.issue", ExternalID: "1"}}
	steps := []struct {
		d    time.Duration
		zone *time.Location
		fn   func(storage.Tx) error
	}{
		{0, time.UTC, func(tx storage.Tx) error {
			if err := tx.PutScope(domain.Scope{ID: "other", Name: "other"}, 0); err != nil {
				return err
			}
			if err := tx.SetExecutionExecutor("execution", "shell"); err != nil {
				return fmt.Errorf("work queries: set executor: %w", err)
			}
			for _, q := range []struct {
				id       domain.ID
				executor string
				want     error
				label    string
			}{
				{"execution", "shell", nil, "the same executor again"},
				{"execution", "claude", storage.ErrConflict, "another executor"},
				{"execution", "", storage.ErrInvalid, "an empty executor"},
				{"missing", "shell", storage.ErrNotFound, "a missing attempt"},
			} {
				if err := tx.SetExecutionExecutor(q.id, q.executor); q.want == nil && err != nil || q.want != nil && !errors.Is(err, q.want) {
					return fmt.Errorf("work queries: set executor to %s: %v, want %v", q.label, err, q.want)
				}
			}
			x, err := tx.Execution("execution")
			if err != nil {
				return err
			}
			x.Executor = "claude"
			if err = wantError(tx.PutExecution(x, x.Revision), storage.ErrInvalid); err != nil {
				return fmt.Errorf("work queries: an update changing the executor: %w", err)
			}
			x.Executor = ""
			return tx.PutExecution(x, x.Revision)
		}},
		{time.Second, time.UTC, func(tx storage.Tx) error {
			if err := tx.PutWork(domain.Work{ID: "w1", Kind: "test.objective", ScopeID: "scope", State: domain.WorkReady, CorrelationKeys: github}, 0); err != nil {
				return err
			}
			if err := attempt(tx, "w1", 1, "claude"); err != nil {
				return err
			}
			if err := attempt(tx, "w1", 2, "shell"); err != nil {
				return err
			}
			return finish(tx, "w1", domain.WorkCancelled)
		}},
		// w2 and w3 are created at the same instant, written in two zones.
		{2 * time.Second, east, func(tx storage.Tx) error {
			if err := tx.PutWork(domain.Work{ID: "w2", Kind: "test.other", ScopeID: "other", State: domain.WorkReady}, 0); err != nil {
				return err
			}
			if err := attempt(tx, "w2", 1, "claude"); err != nil {
				return err
			}
			return finish(tx, "w2", domain.WorkFailed)
		}},
		{2 * time.Second, time.UTC, func(tx storage.Tx) error {
			return tx.PutWork(domain.Work{ID: "w3", Kind: "test.objective", ScopeID: "scope", ParentID: "w1", State: domain.WorkReady, CorrelationKeys: []domain.CorrelationKey{{Source: "github.issue", ExternalID: "3"}}}, 0)
		}},
		// Written five hours east, the time reads later than it is.
		{5*time.Second + 500*time.Millisecond, east, func(tx storage.Tx) error {
			w, err := tx.Work("w3")
			if err != nil {
				return err
			}
			w.Display = &domain.WorkDisplay{Title: "three"}
			return tx.PutWork(w, w.Revision)
		}},
		{6*time.Second + 1, time.UTC, func(tx storage.Tx) error {
			w, err := tx.Work("work")
			if err != nil {
				return err
			}
			w.Display = &domain.WorkDisplay{Title: "seed"}
			return tx.PutWork(w, w.Revision)
		}},
	}
	for _, st := range steps {
		if err := at(st.d, st.zone, st.fn); err != nil {
			return err
		}
	}
	return transact(s, c, func(tx storage.Tx) error {
		if x, err := tx.Execution("execution"); err != nil || x.Executor != "shell" {
			return fmt.Errorf("work queries: recorded executor %q after an update that omitted it (%v), want shell", x.Executor, err)
		}
		works, err := tx.ListWorks()
		if err != nil {
			return err
		}
		key := func(id domain.ID, o storage.WorkOrder) *storage.WorkKey {
			for _, w := range works {
				if w.ID == id {
					k := o.Key(w)
					return &k
				}
			}
			return nil
		}
		closed := []domain.WorkState{domain.WorkCancelled, domain.WorkFailed}
		for _, q := range []struct {
			q           storage.WorkQuery
			want, label string
		}{
			{storage.WorkQuery{}, "work w1 w2 w3", "every Work by creation"},
			{storage.WorkQuery{Order: storage.WorkCreatedDesc}, "w3 w2 w1 work", "latest created first"},
			{storage.WorkQuery{Order: storage.WorkUpdated}, "w1 w2 w3 work", "by update"},
			{storage.WorkQuery{Order: storage.WorkUpdatedDesc}, "work w3 w2 w1", "latest updated first"},
			{storage.WorkQuery{States: closed}, "w1 w2", "of two states"},
			{storage.WorkQuery{States: []domain.WorkState{domain.WorkRunning}}, "", "of a state no Work is in"},
			{storage.WorkQuery{ScopeID: "other"}, "w2", "of a scope"},
			{storage.WorkQuery{Kind: "test.other"}, "w2", "of a kind"},
			{storage.WorkQuery{ParentID: "w1"}, "w3", "of a parent"},
			{storage.WorkQuery{Sources: []string{"github.issue"}}, "w1 w3", "from a correlation source"},
			{storage.WorkQuery{Sources: []string{"test", "test.other"}}, "work w2", "from a source or kind"},
			{storage.WorkQuery{Executor: "shell"}, "work w1", "whose latest attempt is shell's"},
			{storage.WorkQuery{Executor: "claude"}, "w2", "whose latest attempt is claude's, not an earlier one"},
			{storage.WorkQuery{Executor: "missing"}, "", "of an executor no attempt has"},
			{storage.WorkQuery{UpdatedSince: t0.Add(2 * time.Second).In(east)}, "work w2 w3", "updated since an instant, inclusive"},
			{storage.WorkQuery{States: closed, ScopeID: "scope", Sources: []string{"github.issue"}, Executor: "shell"}, "w1", "of every filter at once"},
			{storage.WorkQuery{Order: storage.WorkCreatedDesc, Limit: 2}, "w3 w2", "a first page"},
			{storage.WorkQuery{Order: storage.WorkCreatedDesc, After: key("w2", storage.WorkCreatedDesc), Limit: 2}, "w1 work", "the page after a tie"},
			{storage.WorkQuery{Order: storage.WorkCreated, After: key("w2", storage.WorkCreated)}, "w3", "after a tie, ascending"},
			{storage.WorkQuery{Order: storage.WorkUpdatedDesc, States: []domain.WorkState{domain.WorkReady}, After: key("work", storage.WorkUpdatedDesc), Limit: 1}, "w3", "a filtered page by update"},
		} {
			got, err := tx.QueryWorks(q.q)
			if err != nil {
				return fmt.Errorf("work queries %s: %w", q.label, err)
			}
			var ids []string
			for _, w := range got {
				ids = append(ids, string(w.ID))
			}
			if g := strings.Join(ids, " "); g != q.want {
				return fmt.Errorf("work queries %s: got %q, want %q", q.label, g, q.want)
			}
			cq := q.q
			cq.Order, cq.After, cq.Limit = "", nil, 0
			all, err := tx.QueryWorks(cq)
			if err != nil {
				return err
			}
			if n, err := tx.CountWorks(q.q); err != nil || n != len(all) {
				return fmt.Errorf("work queries count %s: %d (%v), want %d", q.label, n, err, len(all))
			}
		}
		if _, err := tx.QueryWorks(storage.WorkQuery{Order: "sideways"}); !errors.Is(err, storage.ErrInvalid) {
			return fmt.Errorf("work queries in an unknown order: %v, want %v", err, storage.ErrInvalid)
		}
		for _, q := range []struct {
			q           storage.WorkQuery
			want, label string
		}{
			{storage.WorkQuery{}, "4 [other scope] [github.issue test test.other] [claude shell]", "of every Work"},
			{storage.WorkQuery{States: closed}, "2 [other scope] [github.issue test.other] [claude shell]", "of closed Work"},
			{storage.WorkQuery{Executor: "claude"}, "1 [other] [test.other] [claude]", "of an executor's Work"},
			{storage.WorkQuery{ScopeID: "scope", States: []domain.WorkState{domain.WorkReady}}, "2 [scope] [github.issue test] [shell]", "of ready Work in a scope, one without attempts"},
			{storage.WorkQuery{ScopeID: "missing"}, "0 [] [] []", "of no Work"},
		} {
			f, err := tx.WorkFacets(q.q)
			if err != nil {
				return fmt.Errorf("work facets %s: %w", q.label, err)
			}
			if g := fmt.Sprintf("%d %v %v %v", f.Count, f.ScopeIDs, f.Sources, f.Executors); g != q.want {
				return fmt.Errorf("work facets %s: got %q, want %q", q.label, g, q.want)
			}
		}
		return nil
	})
}
