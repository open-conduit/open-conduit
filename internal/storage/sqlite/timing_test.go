package sqlite

import (
	"context"
	"errors"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/storage"
)

// TestTimeTransactionsReportsWaitAndHold: each write transaction reports
// once, committed or not. One that queued behind another writer reports a
// wait covering the time it was seen queued, and the writer it queued behind
// a hold covering the same time. After a nil observer nothing is reported.
func TestTimeTransactionsReportsWaitAndHold(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "conduit.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var mu sync.Mutex
	var got []storage.TransactionTiming
	store.TimeTransactions(func(x storage.TransactionTiming) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, x)
	})
	guard := storage.Guard{Now: time.Unix(1, 0)}
	entered, release := make(chan struct{}), make(chan struct{})
	first := make(chan error, 1)
	go func() {
		first <- store.Transact(ctx, guard, func(storage.Tx) error {
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered
	before := store.db.Stats().WaitCount
	second := make(chan error, 1)
	go func() {
		second <- store.Transact(ctx, guard, func(storage.Tx) error { return errors.New("roll back") })
	}()
	// The pool counts a waiter once it is queued for the one connection.
	for store.db.Stats().WaitCount == before {
		runtime.Gosched()
	}
	queued := time.Now()
	for time.Since(queued) == 0 {
		runtime.Gosched()
	}
	seen := time.Since(queued)
	close(release)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if err := <-second; err == nil {
		t.Fatal("the second transaction should have rolled back")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 2 {
		t.Fatalf("got %d reports, want 2: %+v", len(got), got)
	}
	var committed, rolled *storage.TransactionTiming
	for i := range got {
		if got[i].Committed {
			committed = &got[i]
		} else {
			rolled = &got[i]
		}
	}
	if committed == nil || rolled == nil {
		t.Fatalf("want one committed and one rolled back: %+v", got)
	}
	if committed.Hold < seen {
		t.Errorf("the holder reports a hold of %s, under the %s it was seen holding", committed.Hold, seen)
	}
	if rolled.Wait < seen {
		t.Errorf("the queued writer reports a wait of %s, under the %s it was seen queued", rolled.Wait, seen)
	}
	store.TimeTransactions(nil)
	if err := store.Transact(ctx, guard, func(storage.Tx) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("reported after the observer was removed: %+v", got)
	}
}
