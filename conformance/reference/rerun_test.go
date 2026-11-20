package reference

import (
	"context"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/storage"
)

// TestRerunCatchesAccumulatingCallback is the negative control for the
// Memory's -tags rerun harness: an accumulating callback hands back twice
// what it read, and only the last run's writes commit.
func TestRerunCatchesAccumulatingCallback(t *testing.T) {
	ctx := context.Background()
	m := New()
	runs := 0
	err := storage.Retry(&abortFirst{transact: m.transact}, rerunPolicy).Transact(ctx, storage.Guard{Now: time.Unix(1, 0)}, func(tx storage.Tx) error {
		runs++
		return tx.Put("rerun.test", "k", []byte{byte(runs)})
	})
	if err != nil || runs != 2 {
		t.Fatalf("runs = %d, err = %v; want the callback run twice", runs, err)
	}
	var seen []byte
	err = storage.Retry(&abortFirst{view: m.view}, rerunPolicy).(storage.Viewer).View(ctx, func(tx storage.Tx) error {
		v, err := tx.Get("rerun.test", "k")
		seen = append(seen, v...)
		return err
	})
	if err != nil || len(seen) != 2 || seen[1] != 2 {
		t.Fatalf("seen = %v, err = %v; want the accumulation exposed over the second run's write", seen, err)
	}
}
