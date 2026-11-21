package runtime_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/open-conduit/open-conduit/internal/runtime"
	"github.com/open-conduit/open-conduit/storage"
)

func drainStatus(t *testing.T, f conformanceFixture) runtime.DrainStatus {
	t.Helper()
	var out runtime.DrainStatus
	routingRead(t, f, func(tx storage.Tx) (err error) {
		out, err = runtime.DrainStatusInTx(tx)
		return err
	})
	return out
}

func beginDrain(t *testing.T, f conformanceFixture, origin string) {
	t.Helper()
	if err := f.Store.Transact(context.Background(), storage.Guard{Now: f.Clock.Now()}, func(tx storage.Tx) error {
		_, err := runtime.BeginDrainInTx(tx, f.Clock.Now(), origin, "moving")
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

// A drain refuses new attempts, reports the one in flight as what it waits
// for, and is safe once that has gone. Ending it lets Work start again, and
// Work started with no drain is the negative control.
func TestDrainStopsNewAttempts(t *testing.T) {
	f, s, _ := admissionFixture(t, nil)
	ctx := context.Background()
	running := seedSchedulerWork(t, f, "running")
	queued := seedSchedulerWork(t, f, "queued")
	attempt, err := s.Claim(ctx, running)
	if err != nil {
		t.Fatalf("claim with no drain: %v", err)
	}
	beginDrain(t, f, "/state")
	if _, err := s.Claim(ctx, queued); !errors.Is(err, runtime.ErrDraining) {
		t.Fatalf("claim during a drain: %v", err)
	}
	st := drainStatus(t, f)
	if !st.Draining || st.Safe || !slices.ContainsFunc(st.Blocking, func(r runtime.QuiescenceReason) bool { return r.Code == runtime.QuiescentLiveExecution }) {
		t.Fatalf("with an attempt in flight: %+v", st)
	}
	if !slices.ContainsFunc(st.Carried, func(r runtime.QuiescenceReason) bool { return r.Code == runtime.QuiescentUnreadInput }) {
		t.Fatalf("the queued Work's input should be carried: %+v", st)
	}
	if len(st.Scopes) != 1 || st.Scopes[0].InFlight != 1 || len(st.Scopes[0].Blocking) != 1 || st.Scopes[0].Blocking[0] != string(running) {
		t.Fatalf("scopes: %+v", st.Scopes)
	}
	if _, err := s.Launch(ctx, attempt); err != nil {
		t.Fatalf("an attempt claimed before the drain must still launch: %v", err)
	}
	if err := f.Store.Transact(ctx, storage.Guard{Now: f.Clock.Now()}, func(tx storage.Tx) error {
		_, err := runtime.EndDrainInTx(tx)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Claim(ctx, queued); err != nil {
		t.Fatalf("claim after the drain ended: %v", err)
	}
}
