package sqlite

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/storage"
)

// A standby repeats this read for as long as it waits, against a holder that
// keeps committing. It must take no copy and must not fail because of those
// commits. The control is Inspect, which copies and gives up on the same state.
func TestViewUnownedReadsABusyDatabaseWithoutACopy(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "conduit.db")
	holder, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	commits := 0
	commit := func() {
		commits++
		if err := holder.Transact(ctx, storage.Guard{Now: time.Now()}, func(tx storage.Tx) error {
			return tx.Put("test.view", "n", []byte(fmt.Sprint(commits)))
		}); err != nil {
			t.Error(err)
		}
	}
	commit()
	copies := 0
	saved := inspectCopied
	defer func() { inspectCopied = saved }()
	inspectCopied = func(string) {
		copies++
		commit()
	}
	if i, err := InspectUnowned(ctx, path); err == nil {
		i.Close()
		t.Fatal("control: Inspect read a database that never stopped changing")
	}
	if copies == 0 {
		t.Fatal("control: Inspect took no copy")
	}
	copies = 0
	want := fmt.Sprint(commits)
	var got []byte
	if err = ViewUnowned(ctx, path, func(tx storage.Tx) error {
		var err error
		got, err = tx.Get("test.view", "n")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("read %q, want the latest commit %q", got, want)
	}
	if copies != 0 {
		t.Fatalf("took %d copies", copies)
	}
}

func TestViewUnownedRefusals(t *testing.T) {
	ctx := context.Background()
	read := func(storage.Tx) error { t.Error("read state it should refuse"); return nil }
	if err := ViewUnowned(ctx, filepath.Join(t.TempDir(), "absent.db"), read); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("absent: %v", err)
	}
	path := filepath.Join(t.TempDir(), "conduit.db")
	db, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	func() {
		defer SimulateRecordFormat(recordFormat - 1)()
		if err := ViewUnowned(ctx, path, read); !errors.Is(err, storage.ErrRecordsNewer) {
			t.Fatalf("newer records: %v", err)
		}
	}()
}
