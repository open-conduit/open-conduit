package bootstrap

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/conformance/reference"
	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/extension"
	"github.com/open-conduit/open-conduit/internal/storage/sqlite"
	"github.com/open-conduit/open-conduit/storage"
)

// countingSweeper counts its sweeps and fails while fail is set.
type countingSweeper struct {
	reference.StateExecutor
	sweeps int
	fail   bool
}

func (e *countingSweeper) SweepState(ctx context.Context, known extension.ExecutionKnownFunc) (extension.StateSweepReport, error) {
	e.sweeps++
	if e.fail {
		return extension.StateSweepReport{}, errors.New("sweep failed")
	}
	return e.StateExecutor.SweepState(ctx, known)
}

// The daemon sweeps each executor's state against the coordination state
// once: what the store holds stays, what it doesn't goes. An executor that
// doesn't sweep is left alone, and a failed sweep is asked again only
// after SweepInterval.
func TestDaemonSweepsExecutorStateOnce(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "conduit.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	at := time.Now().UTC()
	if err := store.Transact(ctx, storage.Guard{Now: at}, func(tx storage.Tx) error {
		if err := tx.PutScope(domain.Scope{ID: "eng", Name: "eng"}, 0); err != nil {
			return err
		}
		event := domain.Event{ID: "e", Type: "fixture.changed", Source: "fixture", DeliveryID: "e", OccurredAt: at, ReceivedAt: at}
		if _, _, err := tx.AcceptEvent(event); err != nil {
			return err
		}
		if err := tx.PutWork(domain.Work{ID: "w", ScopeID: "eng", Kind: "fixture.work", State: domain.WorkReady}, 0); err != nil {
			return err
		}
		return tx.PutExecution(domain.Execution{ID: "x-known", WorkID: "w", ExecutorBinding: "run", Executor: "fixture", Attempt: 1, State: domain.ExecutionPending, TriggerID: event.ID}, 0)
	}); err != nil {
		t.Fatal(err)
	}
	clock := at
	sweeper, failing := &countingSweeper{}, &countingSweeper{fail: true}
	for _, id := range []domain.ID{"x-known", "x-later"} {
		if _, err := sweeper.Run(ctx, extension.RunRequest{Execution: domain.Execution{ID: id}, PersistRecovery: func(domain.Payload) error { return nil }}); err != nil {
			t.Fatal(err)
		}
	}
	d := &daemon{o: Options{SweepInterval: time.Hour}, coord: store, now: func() time.Time { return clock }, log: slog.New(slog.NewTextHandler(io.Discard, nil)), reported: map[string]string{}, reportedAt: map[string]time.Time{}}
	d.external.executors = map[string]extension.Executor{"sweeps": sweeper, "fails": failing, "plain": &reference.Executor{}}
	pass := func() {
		d.sweepExecutorState(ctx)
		d.wg.Wait()
	}
	pass()
	if !slices.Equal(sweeper.Held(), []domain.ID{"x-known"}) {
		t.Fatalf("held after the sweep: %v", sweeper.Held())
	}
	if d.reported["executor_sweep\x00fails"] == "" {
		t.Fatal("a failed sweep was not reported")
	}
	pass()
	if sweeper.sweeps != 1 || failing.sweeps != 1 {
		t.Fatalf("swept again at once: %d, %d", sweeper.sweeps, failing.sweeps)
	}
	failing.fail = false
	clock = clock.Add(time.Hour)
	pass()
	if sweeper.sweeps != 1 || failing.sweeps != 2 {
		t.Fatalf("after SweepInterval: %d, %d", sweeper.sweeps, failing.sweeps)
	}
	if d.reported["executor_sweep\x00fails"] != "" {
		t.Fatal("a sweep that passed stayed reported")
	}
}
