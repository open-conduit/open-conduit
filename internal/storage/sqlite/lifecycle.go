package sqlite

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/open-conduit/open-conduit/storage"
)

// Name is the name this backend registers under, and the backend a state
// directory uses when its configuration names none.
const Name = "sqlite"

// Backend is SQLite's coordination lifecycle. A location is the path of
// the database file.
var Backend storage.Backend = backend{}

func init() { storage.RegisterBackend(Name, Backend) }

var _ storage.Database = (*Store)(nil)

type backend struct{}

func (backend) Open(ctx context.Context, location string) (storage.Database, error) {
	s, err := Open(ctx, location)
	if err != nil {
		return nil, err
	}
	return s, nil
}

func (backend) Restore(ctx context.Context, snapshot, destination string, now time.Time) error {
	return Restore(ctx, snapshot, destination, now)
}

// Inspect opens a private copy of the database and its WAL, so nothing
// Open does (recovering the WAL, recording the record format) reaches the
// original. The copy is removed on Close.
func (backend) Inspect(ctx context.Context, location string) (storage.Inspection, error) {
	abs, err := filepath.Abs(location)
	if err != nil {
		return nil, err
	}
	if info, err := os.Lstat(abs); err != nil {
		return nil, err
	} else if !privateFile(info) {
		return nil, errors.New("sqlite: database must be a private regular file")
	}
	tmp, err := os.MkdirTemp("", "oc-sqlite-inspect-")
	if err != nil {
		return nil, err
	}
	copied := filepath.Join(tmp, "conduit.db")
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := copyPrivate(abs+suffix, copied+suffix); err != nil && !(suffix != "" && errors.Is(err, os.ErrNotExist)) {
			os.RemoveAll(tmp)
			return nil, err
		}
	}
	s, err := Open(ctx, copied)
	if err != nil {
		os.RemoveAll(tmp)
		return nil, err
	}
	return inspection{s, tmp}, nil
}

// inspection is an opened private copy. Only its Viewer side is offered:
// writes to the copy would be lost, so it is read through View alone.
type inspection struct {
	s   *Store
	dir string
}

func (i inspection) View(ctx context.Context, fn func(storage.Tx) error) error {
	return i.s.View(ctx, fn)
}
func (i inspection) IntegrityCheck(ctx context.Context) error { return i.s.IntegrityCheck(ctx) }
func (i inspection) SchemaVersion() uint32                    { return i.s.SchemaVersion() }
func (i inspection) Close() error {
	return errors.Join(i.s.Close(), os.RemoveAll(i.dir))
}

func copyPrivate(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, in)
	return errors.Join(err, out.Close())
}

// Snapshot takes an online backup to path, then reads that copy, so read
// sees exactly the state the snapshot holds.
func (s *Store) Snapshot(ctx context.Context, path string, read func(storage.Tx) error) error {
	if err := s.Backup(ctx, path); err != nil {
		return err
	}
	if read == nil {
		return nil
	}
	snapshot, err := Open(ctx, path)
	if err != nil {
		return err
	}
	err = snapshot.View(ctx, read)
	if err == nil {
		// Fold the WAL Open started back into the file, so the published
		// snapshot is the whole of it.
		_, err = snapshot.db.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)")
	}
	return errors.Join(err, snapshot.Close())
}

// Fence clears every Execution lease's owner and expiry in one
// transaction, keeping epochs.
func (s *Store) Fence(ctx context.Context, now time.Time) error {
	if now.IsZero() {
		return storage.ErrInvalid
	}
	return s.Transact(ctx, storage.Guard{Now: now}, func(tx storage.Tx) error {
		t := tx.(*transaction)
		xs, err := t.ListExecutions("")
		if err != nil {
			return err
		}
		for _, x := range xs {
			if x.Lease.Owner == "" && x.Lease.ExpiresAt.IsZero() {
				continue
			}
			x.Lease.Owner = ""
			x.Lease.ExpiresAt = time.Time{}
			x.Revision++
			x.UpdatedAt = now
			if err = t.writeExecution(x, true); err != nil {
				return err
			}
		}
		return nil
	})
}
