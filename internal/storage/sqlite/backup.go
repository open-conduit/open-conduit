package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/open-conduit/open-conduit/storage"
	modernsqlite "modernc.org/sqlite"
)

// Backup uses SQLite's online backup API, never a copy of a live database file.
// It publishes a complete, synced standalone snapshot at a previously absent path.
// Execution artifact references must be read from this snapshot, not the live DB.
func (s *Store) Backup(ctx context.Context, destination string) error {
	return s.backup(ctx, destination, SchemaVersion)
}

// backup is Backup accepting a snapshot at any schema from oldest up: state
// an older binary left is preserved at the schema it holds.
func (s *Store) backup(ctx context.Context, destination string, oldest uint32) (err error) {
	abs, err := filepath.Abs(destination)
	if err != nil {
		return err
	}
	if abs == s.path {
		return fmt.Errorf("sqlite: backup destination is the source")
	}
	if _, err = os.Lstat(abs); err == nil {
		return os.ErrExist
	} else if !os.IsNotExist(err) {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(abs), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(abs), ".sqlite-backup-*")
	if err != nil {
		return err
	}
	temporary := f.Name()
	if err = f.Close(); err != nil {
		return err
	}
	defer os.Remove(temporary)
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	err = conn.Raw(func(raw any) error {
		source, ok := raw.(interface {
			NewBackup(string) (*modernsqlite.Backup, error)
		})
		if !ok {
			return fmt.Errorf("sqlite: driver does not provide online backup")
		}
		backup, e := source.NewBackup((&url.URL{Scheme: "file", Path: temporary}).String())
		if e != nil {
			return e
		}
		finished := false
		defer func() {
			if !finished {
				_ = backup.Finish()
			}
		}()
		for {
			if e = ctx.Err(); e != nil {
				return e
			}
			more, e := backup.Step(128)
			if e != nil {
				return e
			}
			if !more {
				break
			}
		}
		e = backup.Finish()
		finished = true
		return e
	})
	if err != nil {
		return err
	}
	if err = verifySnapshotFrom(ctx, temporary, oldest); err != nil {
		return err
	}
	return publishFile(temporary, abs)
}

func verifySnapshot(ctx context.Context, path string) error {
	return verifySnapshotFrom(ctx, path, SchemaVersion)
}

// verifySnapshotFrom checks a snapshot whose schema is from oldest up to this
// binary's.
func verifySnapshotFrom(ctx context.Context, path string, oldest uint32) error {
	db, err := readonlyDB(path)
	if err != nil {
		return err
	}
	defer db.Close()
	var version uint32
	if err = db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if version > SchemaVersion {
		return fmt.Errorf("sqlite: snapshot %w (schema %d; this binary supports %d)", storage.ErrSchemaNewer, version, SchemaVersion)
	}
	if version < oldest {
		return fmt.Errorf("sqlite: snapshot schema %d unsupported; expected %d", version, SchemaVersion)
	}
	var metadata string
	if err = db.QueryRowContext(ctx, "SELECT value FROM metadata WHERE key='schema_version'").Scan(&metadata); err != nil {
		return fmt.Errorf("sqlite: invalid snapshot metadata: %w", err)
	}
	if metadata != fmt.Sprint(version) {
		return fmt.Errorf("sqlite: snapshot schema metadata mismatch")
	}
	if err = checkRecordFormat(ctx, db); err != nil {
		return err
	}
	return (&Store{db: db}).IntegrityCheck(ctx)
}

// readonlyDB opens a published snapshot to read. immutable tells SQLite
// the file cannot change, so it takes no lock and writes no -shm or -wal
// beside it: verifying or restoring a bundle leaves the bundle as it was.
func readonlyDB(path string) (*sql.DB, error) {
	u := url.URL{Scheme: "file", Path: path}
	q := u.Query()
	q.Set("mode", "ro")
	q.Set("immutable", "1")
	q.Add("_pragma", "foreign_keys(1)")
	u.RawQuery = q.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err == nil {
		db.SetMaxOpenConns(1)
	}
	return db, err
}
func publishFile(temporary, destination string) error {
	f, err := os.OpenFile(temporary, os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	err = f.Sync()
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = os.Link(temporary, destination); err != nil {
		return err
	}
	d, err := os.Open(filepath.Dir(destination))
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// Restore verifies a standalone snapshot, restores it to a previously absent
// isolated destination and clears lease authority before publishing. Epochs are
// retained: any later Claim must advance them. No process is adopted or launched.
// The caller must keep the destination stopped and validate referenced artifacts.
func Restore(ctx context.Context, snapshotPath, destination string, now time.Time) (err error) {
	if now.IsZero() {
		return storage.ErrInvalid
	}
	snapshot, err := filepath.Abs(snapshotPath)
	if err != nil {
		return err
	}
	if err = verifySnapshot(ctx, snapshot); err != nil {
		return err
	}
	destination, err = filepath.Abs(destination)
	if err != nil {
		return err
	}
	if _, err = os.Lstat(destination); err == nil {
		return os.ErrExist
	} else if !os.IsNotExist(err) {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(destination), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(destination), ".sqlite-restore-*")
	if err != nil {
		return err
	}
	temporary := f.Name()
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Remove(temporary); err != nil {
		return err
	}
	defer func() { os.Remove(temporary); os.Remove(temporary + "-wal"); os.Remove(temporary + "-shm") }()
	sourceDB, err := readonlyDB(snapshot)
	if err != nil {
		return err
	}
	source := &Store{db: sourceDB, path: snapshot}
	err = source.Backup(ctx, temporary)
	closeErr := sourceDB.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	restored, err := Open(ctx, temporary)
	if err != nil {
		return err
	}
	err = restored.fenceRestored(ctx, now)
	if err == nil {
		_, err = restored.db.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)")
	}
	closeErr = restored.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return publishFile(temporary, destination)
}
