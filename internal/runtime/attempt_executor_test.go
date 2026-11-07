package runtime_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/internal/runtime"
	"github.com/open-conduit/open-conduit/internal/storage/sqlite"
	"github.com/open-conduit/open-conduit/storage"
)

// BackfillExecutors records, from their state, the executor of attempts
// registered before attempts recorded it, so storage selects their Work
// by executor; an attempt with no state stays without, and a second run
// does nothing.
func TestBackfillExecutorsRecordsLegacyAttempts(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	g := storage.Guard{Now: ingressTime}
	err = store.Transact(ctx, g, func(tx storage.Tx) error {
		if err := tx.PutScope(domain.Scope{ID: "s", Name: "s"}, 0); err != nil {
			return err
		}
		if _, _, err := tx.AcceptEvent(domain.Event{ID: "e", Type: "fixture.changed", Source: "fixture", DeliveryID: "e", OccurredAt: ingressTime, ReceivedAt: ingressTime}); err != nil {
			return err
		}
		for _, id := range []domain.ID{"w1", "w2"} {
			if err := tx.PutWork(domain.Work{ID: id, Kind: "fixture.item", ScopeID: "s", State: domain.WorkReady}, 0); err != nil {
				return err
			}
			if err := tx.PutExecution(domain.Execution{ID: id + "/attempt/1", WorkID: id, ExecutorBinding: "run", Attempt: 1, State: domain.ExecutionPending, TriggerID: "e"}, 0); err != nil {
				return err
			}
		}
		return runtime.PutLegacyAttemptState(tx, "w1/attempt/1", "w1", "shell")
	})
	if err != nil {
		t.Fatal(err)
	}
	router, err := runtime.NewRouter(store, func() time.Time { return ingressTime }, runtime.RouterOptions{Generation: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if n, err := router.BackfillExecutors(ctx); err != nil || n != 1 {
		t.Fatalf("backfill recorded %d executors (%v), want 1", n, err)
	}
	err = store.Transact(ctx, g, func(tx storage.Tx) error {
		ws, err := tx.QueryWorks(storage.WorkQuery{Executor: "shell"})
		if err == nil && (len(ws) != 1 || ws[0].ID != "w1") {
			t.Errorf("Work by the backfilled executor: %v, want w1", ws)
		}
		x, err := tx.Execution("w2/attempt/1")
		if err == nil && x.Executor != "" {
			t.Errorf("an attempt with no state got executor %q", x.Executor)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if n, err := router.BackfillExecutors(ctx); err != nil || n != 0 {
		t.Fatalf("second backfill recorded %d (%v), want none", n, err)
	}
}
