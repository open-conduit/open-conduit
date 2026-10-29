package sqlite

import (
	"context"
	"database/sql/driver"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/storage"
	modernsqlite "modernc.org/sqlite"
)

// blocking stands in for a long read: test_block() reports that it is running
// on started, then returns once release is closed.
var blocking struct {
	started chan struct{}
	release chan struct{}
}

func init() {
	modernsqlite.MustRegisterScalarFunction("test_block", 0, func(*modernsqlite.FunctionContext, []driver.Value) (driver.Value, error) {
		blocking.started <- struct{}{}
		<-blocking.release
		return []byte("{}"), nil
	})
}

// A caller cancelled in the middle of a transaction must leave the write lock
// free and the store on one connection (#155).
func TestCancelledTransactReleasesWriteLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "conduit.db")
	s, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	guard := storage.Guard{Now: time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)}

	for i := range 3 {
		blocking.started = make(chan struct{})
		blocking.release = make(chan struct{})
		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			<-blocking.started
			cancel()
			close(blocking.release)
		}()
		err := s.Transact(ctx, guard, func(tx storage.Tx) error {
			if err := tx.Put("test", "key", []byte("{}")); err != nil {
				return err
			}
			_, err := read[map[string]any](tx.(*transaction), "SELECT test_block()")
			if err != nil {
				return err
			}
			// The caller has gone: nothing more runs in its transaction.
			return tx.Put("test", "after", []byte("{}"))
		})
		cancel()
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation %d: Transact = %v, want context.Canceled", i, err)
		}
		if n := s.db.Stats().OpenConnections; n != 1 {
			t.Fatalf("cancellation %d: %d open connections, want 1", i, n)
		}
	}

	// Another connection to the file must be able to take the write lock: a
	// discarded connection still holding it would make this wait out the busy
	// timeout and fail.
	other, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	for _, store := range []*Store{other, s} {
		err := store.Transact(context.Background(), guard, func(tx storage.Tx) error {
			return tx.Put("test", "key", []byte(`{"ok":true}`))
		})
		if err != nil {
			t.Fatalf("write after cancellations: %v", err)
		}
	}
	err = s.Transact(context.Background(), guard, func(tx storage.Tx) error {
		if _, err := tx.Get("test", "after"); !errors.Is(err, storage.ErrNotFound) {
			t.Errorf("write after cancellation: Get = %v, want ErrNotFound", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
