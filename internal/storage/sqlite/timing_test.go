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

// TestTimeTransactionsReportsOnesThatNeverBegan: a transaction that gives up
// on the writer still reports its wait, whether its context ended in the
// pool's queue or BEGIN IMMEDIATE stayed busy against another handle.
func TestTimeTransactionsReportsOnesThatNeverBegan(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "conduit.db")
	store, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	other, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	// The other handle's one connection gives up on the lock at once.
	if _, err = other.db.ExecContext(ctx, "PRAGMA busy_timeout=1"); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var got []storage.TransactionTiming
	report := func(x storage.TransactionTiming) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, x)
	}
	other.TimeTransactions(report)
	guard := storage.Guard{Now: time.Unix(1, 0)}
	entered, release := make(chan struct{}), make(chan struct{})
	holder := make(chan error, 1)
	go func() {
		holder <- store.Transact(ctx, guard, func(storage.Tx) error {
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered
	if err := other.Transact(ctx, guard, func(storage.Tx) error { return nil }); err == nil {
		t.Fatal("the other handle began while the writer was held")
	}
	// Queued behind the holder on the store's one connection, then cancelled.
	store.TimeTransactions(report)
	before := store.db.Stats().WaitCount
	queued, cancel := context.WithCancel(ctx)
	defer cancel()
	gaveUp := make(chan error, 1)
	go func() {
		gaveUp <- store.Transact(queued, guard, func(storage.Tx) error { return nil })
	}()
	for store.db.Stats().WaitCount == before {
		runtime.Gosched()
	}
	cancel()
	if err := <-gaveUp; !errors.Is(err, context.Canceled) {
		t.Fatalf("the queued transaction ended with %v", err)
	}
	close(release)
	if err := <-holder; err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	notBegun := 0
	for _, x := range got {
		if !x.Began {
			notBegun++
			if x.Wait <= 0 || x.Hold != 0 || x.Committed {
				t.Errorf("a transaction that never began reported %+v", x)
			}
		}
	}
	if notBegun != 2 {
		t.Fatalf("%d reports of a transaction that never began, want 2: %+v", notBegun, got)
	}
}
