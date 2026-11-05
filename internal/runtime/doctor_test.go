package runtime

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/internal/storage/sqlite"
	"github.com/open-conduit/open-conduit/storage"
)

// countingStore counts the attempt states read and refuses a full listing
// of executions, so a check that walks the whole history fails.
type countingStore struct {
	inner   storage.Store
	reads   atomic.Int64
	listAll atomic.Int64
}

func (s *countingStore) Transact(ctx context.Context, g storage.Guard, fn func(storage.Tx) error) error {
	return s.inner.Transact(ctx, g, func(tx storage.Tx) error { return fn(countingTx{tx, s}) })
}
func (s *countingStore) View(ctx context.Context, fn func(storage.Tx) error) error {
	return s.inner.(storage.Viewer).View(ctx, func(tx storage.Tx) error { return fn(countingTx{tx, s}) })
}

type countingTx struct {
	storage.Tx
	s *countingStore
}

func (t countingTx) ListExecutions(work domain.ID) ([]domain.Execution, error) {
	if work == "" {
		t.s.listAll.Add(1)
	}
	return t.Tx.ListExecutions(work)
}
func (t countingTx) Get(ns, key string) ([]byte, error) {
	if ns == attemptNamespace {
		t.s.reads.Add(1)
	}
	return t.Tx.Get(ns, key)
}

// TestDoctorReadsOnlyOutstandingAttempts seeds many finished attempts whose
// resource exclusion is confirmed and a few that still hold, one of them
// pinned to an executor that is no longer installed. Both checks must report
// exactly the holding ones while reading no other attempt's state, once the
// unexcluded index is built; before it is, they fall back to every attempt.
func TestDoctorReadsOnlyOutstandingAttempts(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "doctor.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	guard := storage.Guard{Now: now}
	const excluded = 300
	held := map[int]string{excluded: "fixture", excluded + 1: "gone"}
	if err = db.Transact(ctx, guard, func(tx storage.Tx) error {
		if e := tx.PutScope(domain.Scope{ID: "doctor-scope", Name: "doctor", Bindings: []domain.Binding{{Name: "fixture", Executor: "fixture"}}}, 0); e != nil {
			return e
		}
		for i := 0; i < excluded+len(held); i++ {
			id := fmt.Sprintf("t%d", i)
			key := domain.CorrelationKey{Source: "fixture/account", ExternalID: id}
			work := domain.ID("work/" + id)
			if e := tx.PutWork(domain.Work{ID: work, Kind: "fixture.work", ScopeID: "doctor-scope", State: domain.WorkReady, CorrelationKeys: []domain.CorrelationKey{key}}, 0); e != nil {
				return e
			}
			event := domain.Event{ID: domain.ID("event/" + id), Type: "fixture.changed", Source: "fixture/account", DeliveryID: id, OccurredAt: now, ReceivedAt: now, CorrelationKey: &key}
			if _, _, e := AcceptInTx(tx, "doctor-scope", event); e != nil {
				return e
			}
			if e := tx.MarkRouted(event.ID, work, "seeded"); e != nil {
				return e
			}
			xid := domain.ID("execution/" + id)
			if e := tx.PutExecution(domain.Execution{ID: xid, WorkID: work, ExecutorBinding: "fixture", Attempt: 1, State: domain.ExecutionPending, TriggerID: event.ID}, 0); e != nil {
				return e
			}
			if _, e := tx.Claim(xid, "doctor-owner", time.Minute); e != nil {
				return e
			}
			x, e := tx.Execution(xid)
			if e != nil {
				return e
			}
			x.State, x.Outcome = domain.ExecutionCompleted, &domain.Outcome{Kind: domain.OutcomeCompleted}
			if e = tx.PutExecution(x, x.Revision); e != nil {
				return e
			}
			executor, holds := held[i]
			if !holds {
				executor = "fixture"
			}
			if e = putAttemptState(tx, AttemptState{ExecutionID: xid, WorkID: work, ScopeID: "doctor-scope", Generation: "g1", Executor: executor, Excluded: !holds}); e != nil {
				return e
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	store := &countingStore{inner: db}
	s := &Scheduler{store: store, now: func() time.Time { return now }, options: SchedulerOptions{Generation: "g1", Executors: map[string]extension.Executor{"fixture": fakeExecutor{}}}}

	check := func(indexed bool) {
		t.Helper()
		store.reads.Store(0)
		store.listAll.Store(0)
		var holds Check
		for _, c := range Diagnose(ctx, store, now, DoctorOptions{}) {
			if c.Name == "resource_holds" {
				holds = c
			}
		}
		if holds.Code != "unconfirmed_stop" || strings.Join(holds.Items, ",") != "execution/t300,execution/t301" {
			t.Fatalf("indexed=%v resource_holds %+v", indexed, holds)
		}
		pinned := s.PinnedExecutorCheck(ctx)
		if pinned.Code != "executor_missing" || len(pinned.Items) != 1 || !strings.HasPrefix(pinned.Items[0], "execution/t301 ") {
			t.Fatalf("indexed=%v pinned_executors %+v", indexed, pinned)
		}
		reads, lists := store.reads.Load(), store.listAll.Load()
		if indexed && (lists != 0 || reads > 2*int64(len(held))) {
			t.Fatalf("doctor walked the history: %d full listings, %d attempt states read for %d holding attempts", lists, reads, len(held))
		}
		if !indexed && reads < excluded {
			t.Fatalf("before the index is built doctor must read every attempt: %d read", reads)
		}
	}
	// putAttemptState keeps the index, but a store written before it
	// existed has no ready marker yet: drop it to see the fallback.
	if err = db.Transact(ctx, guard, func(tx storage.Tx) error { return tx.Delete(indexNamespace, unexcludedReady) }); err != nil && !errors.Is(err, storage.ErrNotFound) {
		t.Fatal(err)
	}
	check(false)
	if err = db.Transact(ctx, guard, indexUnexcluded); err != nil {
		t.Fatal(err)
	}
	check(true)
}

type fakeExecutor struct{ extension.Executor }
