package sqlite

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/storage"
)

// TestRerunCatchesAccumulatingCallback is the negative control for the
// -tags rerun harness: a callback that accumulates into a variable outside
// it hands back twice what it read, while one that sets what it hands back
// is exact, and only the last run commits.
func TestRerunCatchesAccumulatingCallback(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "rerun.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	g := storage.Guard{Now: time.Unix(1, 0)}
	runs := 0
	if err := rerunTransact(ctx, g, s.transact, func(tx storage.Tx) error {
		runs++
		return tx.Put("rerun.test", "k", []byte{byte(runs)})
	}); err != nil || runs != 2 {
		t.Fatalf("runs = %d, err = %v; want the callback run twice", runs, err)
	}
	var accumulated, set []string
	read := func(tx storage.Tx) ([]string, error) {
		v, err := tx.Get("rerun.test", "k")
		return []string{string(v)}, err
	}
	if err := rerunView(ctx, s.view, func(tx storage.Tx) error {
		got, err := read(tx)
		accumulated = append(accumulated, got...)
		return err
	}); err != nil || len(accumulated) != 2 {
		t.Fatalf("accumulated = %q, err = %v; the harness should expose the accumulation", accumulated, err)
	}
	if err := rerunView(ctx, s.view, func(tx storage.Tx) error {
		got, err := read(tx)
		set = got
		return err
	}); err != nil || len(set) != 1 || set[0] != "\x02" {
		t.Fatalf("set = %q, err = %v; want the second run's write alone committed", set, err)
	}
	refused := errors.New("refused")
	runs = 0
	if err := rerunTransact(ctx, g, s.transact, func(storage.Tx) error { runs++; return refused }); !errors.Is(err, refused) || runs != 1 {
		t.Fatalf("runs = %d, err = %v; a callback's own error is not rerun", runs, err)
	}
}
