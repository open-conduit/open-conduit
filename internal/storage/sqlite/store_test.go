package sqlite_test

import (
	"bufio"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/open-conduit/open-conduit/conformance"
	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/internal/storage/sqlite"
	"github.com/open-conduit/open-conduit/storage"
)

var now = time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)

func open(t *testing.T, path string) *sqlite.Store {
	t.Helper()
	s, err := sqlite.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}
func transact(s storage.Store, fn func(storage.Tx) error) error {
	return s.Transact(context.Background(), storage.Guard{Now: now}, fn)
}

type reopening struct{ path string }

func (r reopening) Transact(ctx context.Context, g storage.Guard, fn func(storage.Tx) error) error {
	s, err := sqlite.Open(ctx, r.path)
	if err != nil {
		return err
	}
	defer s.Close()
	return s.Transact(ctx, g, fn)
}

type alternating struct {
	first, second *sqlite.Store
	next          atomic.Uint64
}

func (a *alternating) Transact(ctx context.Context, g storage.Guard, fn func(storage.Tx) error) error {
	if a.next.Add(1)%2 == 0 {
		return a.first.Transact(ctx, g, fn)
	}
	return a.second.Transact(ctx, g, fn)
}
func TestSQLiteStorageConformance(t *testing.T) {
	for _, mode := range []string{"connection", "reopen_every_transaction", "competing_connections"} {
		t.Run(mode, func(t *testing.T) {
			conformance.RunStorage(t, func() storage.Store {
				path := filepath.Join(t.TempDir(), "conduit.db")
				switch mode {
				case "reopen_every_transaction":
					return reopening{path}
				case "competing_connections":
					return &alternating{first: open(t, path), second: open(t, path)}
				default:
					return open(t, path)
				}
			})
		})
	}
}
func event(id string) domain.Event {
	return domain.Event{ID: domain.ID(id), Type: "fixture.changed.v1", Source: "fixture/account", DeliveryID: id, OccurredAt: now, ReceivedAt: now}
}
func seed(s storage.Store) error {
	return transact(s, func(tx storage.Tx) error {
		if err := tx.PutScope(domain.Scope{ID: "scope", Name: "Scope"}, 0); err != nil {
			return err
		}
		if err := tx.PutWork(domain.Work{ID: "work", ScopeID: "scope", Kind: "fixture.work", State: domain.WorkReady, CorrelationKeys: []domain.CorrelationKey{{Source: "fixture/account", ExternalID: "key"}}}, 0); err != nil {
			return err
		}
		if _, _, err := tx.AcceptEvent(event("trigger")); err != nil {
			return err
		}
		return tx.PutExecution(domain.Execution{ID: "execution", WorkID: "work", ExecutorBinding: "shell", Attempt: 1, State: domain.ExecutionPending, TriggerID: "trigger"}, 0)
	})
}
func TestBackupRestoreFencesAndPreservesCoordination(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s := open(t, filepath.Join(dir, "conduit.db"))
	if err := seed(s); err != nil {
		t.Fatal(err)
	}
	var old domain.LeaseRef
	if err := transact(s, func(tx storage.Tx) error {
		var err error
		old, err = tx.Claim("execution", "worker", time.Hour)
		if err != nil {
			return err
		}
		return tx.PutTimer(domain.Timer{ID: "timer", FireAt: now, Event: event("timer-event")}, 0)
	}); err != nil {
		t.Fatal(err)
	}
	snapshot := filepath.Join(dir, "snapshot.db")
	if err := s.Backup(ctx, snapshot); err != nil {
		t.Fatal(err)
	}
	if err := transact(s, func(tx storage.Tx) error { return tx.Put("future", "write", []byte("after snapshot")) }); err != nil {
		t.Fatal(err)
	}
	restoredPath := filepath.Join(dir, "restored.db")
	if err := sqlite.Restore(ctx, snapshot, restoredPath, now); err != nil {
		t.Fatal(err)
	}
	restored := open(t, restoredPath)
	if err := restored.Transact(ctx, storage.Guard{Now: now, Lease: &old}, func(tx storage.Tx) error { return tx.Put("stale", "write", []byte("bad")) }); !errors.Is(err, storage.ErrLeaseLost) {
		t.Fatalf("restored authority was live: %v", err)
	}
	if err := transact(restored, func(tx storage.Tx) error {
		w, err := tx.WorkByCorrelation(domain.CorrelationKey{Source: "fixture/account", ExternalID: "key"})
		if err != nil || w.ID != "work" {
			return fmt.Errorf("correlation missing: %v", err)
		}
		if _, err = tx.Timer("timer"); err != nil {
			return err
		}
		if _, err = tx.Get("future", "write"); !errors.Is(err, storage.ErrNotFound) {
			return fmt.Errorf("snapshot contains later write: %v", err)
		}
		fresh, err := tx.Claim("execution", "worker", time.Hour)
		if err != nil {
			return err
		}
		if fresh.Epoch <= old.Epoch {
			return fmt.Errorf("epoch did not advance")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := sqlite.Restore(ctx, snapshot, restoredPath, now); !errors.Is(err, os.ErrExist) {
		t.Fatalf("overwrote existing restored data: %v", err)
	}
	if err := s.Backup(ctx, snapshot); !errors.Is(err, os.ErrExist) {
		t.Fatalf("overwrote existing snapshot: %v", err)
	}
}
func TestCrashWorker(t *testing.T) {
	path := os.Getenv("CONDUIT_SQLITE_CRASH_PATH")
	if path == "" {
		return
	}
	mode := os.Getenv("CONDUIT_SQLITE_CRASH_MODE")
	s, err := sqlite.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	signal := func() { fmt.Println("READY"); var b [1]byte; _, _ = os.Stdin.Read(b[:]) }
	err = transact(s, func(tx storage.Tx) error {
		switch mode {
		case "event_kv", "committed":
			if _, _, err := tx.AcceptEvent(event("crash-event")); err != nil {
				return err
			}
			if err := tx.Put("crash", "cursor", []byte("advanced")); err != nil {
				return err
			}
		case "fire", "rearm":
			var next *domain.Timer
			if mode == "rearm" {
				next = &domain.Timer{ID: "timer", FireAt: now.Add(time.Hour), Event: event("next-event")}
			}
			if err := tx.FireTimer("timer", 1, next); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unknown crash mode")
		}
		if mode != "committed" {
			signal()
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if mode == "committed" {
		signal()
	}
}
func TestProcessKillAtomicity(t *testing.T) {
	for _, mode := range []string{"event_kv", "fire", "rearm", "committed"} {
		t.Run(mode, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "conduit.db")
			s, err := sqlite.Open(context.Background(), path)
			if err != nil {
				t.Fatal(err)
			}
			if err = transact(s, func(tx storage.Tx) error {
				return tx.PutTimer(domain.Timer{ID: "timer", FireAt: now, Event: event("timer-event")}, 0)
			}); err != nil {
				t.Fatal(err)
			}
			if err = s.Close(); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(os.Args[0], "-test.run=^TestCrashWorker$")
			cmd.Env = append(os.Environ(), "CONDUIT_SQLITE_CRASH_PATH="+path, "CONDUIT_SQLITE_CRASH_MODE="+mode)
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			stdin, err := cmd.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			defer stdin.Close()
			cmd.Stderr = os.Stderr
			if err = cmd.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
			ready := make(chan string, 1)
			go func() {
				scanner := bufio.NewScanner(stdout)
				if scanner.Scan() {
					ready <- scanner.Text()
				} else {
					ready <- ""
				}
			}()
			select {
			case line := <-ready:
				if line != "READY" {
					t.Fatalf("child failed before transaction barrier: %q", line)
				}
			case <-time.After(30 * time.Second):
				t.Fatal("child never reached transaction barrier")
			}
			if err = cmd.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			_ = cmd.Wait()
			reopened := open(t, path)
			if err = reopened.IntegrityCheck(context.Background()); err != nil {
				t.Fatal(err)
			}
			if err = transact(reopened, func(tx storage.Tx) error {
				_, err := tx.Event("crash-event")
				if mode == "committed" {
					if err != nil {
						return err
					}
					_, err = tx.Get("crash", "cursor")
					return err
				}
				if !errors.Is(err, storage.ErrNotFound) {
					return fmt.Errorf("uncommitted event survived: %v", err)
				}
				if _, err = tx.Get("crash", "cursor"); !errors.Is(err, storage.ErrNotFound) {
					return fmt.Errorf("half-committed KV: %v", err)
				}
				if _, err = tx.Event("timer-event"); !errors.Is(err, storage.ErrNotFound) {
					return fmt.Errorf("uncommitted fire survived: %v", err)
				}
				timer, err := tx.Timer("timer")
				if err != nil || timer.Revision != 1 || timer.Event.ID != "timer-event" {
					return fmt.Errorf("half-committed timer: %+v %v", timer, err)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}
func TestTimerTombstoneReopen(t *testing.T) {
	r := reopening{filepath.Join(t.TempDir(), "conduit.db")}
	if err := transact(r, func(tx storage.Tx) error {
		return tx.PutTimer(domain.Timer{ID: "timer", FireAt: now, Event: event("first")}, 0)
	}); err != nil {
		t.Fatal(err)
	}
	if err := transact(r, func(tx storage.Tx) error { return tx.DeleteTimer("timer", 1) }); err != nil {
		t.Fatal(err)
	}
	if err := transact(r, func(tx storage.Tx) error {
		return tx.PutTimer(domain.Timer{ID: "timer", FireAt: now, Event: event("second")}, 0)
	}); err != nil {
		t.Fatal(err)
	}
	if err := transact(r, func(tx storage.Tx) error { return tx.FireTimer("timer", 1, nil) }); !errors.Is(err, storage.ErrConflict) {
		t.Fatalf("stale fire after reopen: %v", err)
	}
}

func TestConcurrentFirstOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "conduit.db")
	start := make(chan struct{})
	results := make(chan error, 4)
	for range 4 {
		go func() {
			<-start
			s, err := sqlite.Open(context.Background(), path)
			if err == nil {
				err = s.Close()
			}
			results <- err
		}()
	}
	close(start)
	for range 4 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	s := open(t, path)
	if err := s.IntegrityCheck(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// A first open can reach the switch to WAL while a competing first open holds
// the write lock (it is in its migration, or switching to WAL itself). SQLite
// fails that switch at once with SQLITE_BUSY instead of waiting, so Open must
// wait for the lock rather than fail. Here a rollback-journal database is
// write-locked by another connection while Open runs.
func TestOpenWaitsForWriteLockBeforeWAL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "conduit.db")
	s := open(t, path)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	var journal string
	if err = conn.QueryRowContext(context.Background(), "PRAGMA journal_mode=DELETE").Scan(&journal); err != nil || journal != "delete" {
		t.Fatalf("journal mode %q: %v", journal, err)
	}
	if _, err = conn.ExecContext(context.Background(), "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	opened := make(chan error, 1)
	go func() {
		s, err := sqlite.Open(context.Background(), path)
		if err == nil {
			err = s.Close()
		}
		opened <- err
	}()
	select {
	case err = <-opened:
		t.Fatalf("Open returned while the write lock was held: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	if _, err = conn.ExecContext(context.Background(), "ROLLBACK"); err != nil {
		t.Fatal(err)
	}
	if err = <-opened; err != nil {
		t.Fatal(err)
	}
	// A read makes this connection see the WAL flag that Open wrote to the header.
	var version int
	if err = conn.QueryRowContext(context.Background(), "PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if err = conn.QueryRowContext(context.Background(), "PRAGMA journal_mode").Scan(&journal); err != nil || journal != "wal" {
		t.Fatalf("journal mode after Open %q: %v", journal, err)
	}
}
func TestKVPrefixIsLiteralBytes(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "conduit.db"))
	if err := transact(s, func(tx storage.Tx) error {
		for _, key := range []string{"a\x00_one", "a\x00_two", "a_other", "é%_one", "é%_two", "éxxx"} {
			if err := tx.Put("fixture", key, []byte(key)); err != nil {
				return err
			}
		}
		for _, prefix := range []string{"a\x00_", "é%_"} {
			entries, err := tx.List("fixture", prefix)
			if err != nil {
				return err
			}
			if len(entries) != 2 {
				return fmt.Errorf("literal prefix %q returned %d", prefix, len(entries))
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
