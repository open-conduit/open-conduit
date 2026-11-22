package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/open-conduit/open-conduit/internal/localfs"
	"github.com/open-conduit/open-conduit/storage"
)

// Name is the name this backend registers under, and the backend a state
// directory uses when its configuration names none.
const Name = "sqlite"

// Backend is SQLite's coordination lifecycle. A location is the absolute
// path of the database file. Open and Inspect own it through an exclusive
// lock on <path>.lock, held until Close.
var Backend storage.Backend = backend{}

func init() { storage.RegisterBackend(Name, Backend) }

var (
	_ storage.Database = (*Store)(nil)
	_ storage.Database = owned{}
)

type backend struct{}

// absolute refuses a relative location: it would name a different file
// from each process's working directory.
func absolute(location string) error {
	if location == "" || !filepath.IsAbs(location) {
		return fmt.Errorf("sqlite: location %q must be an absolute path: %w", location, storage.ErrInvalid)
	}
	return nil
}

func (backend) Present(_ context.Context, location string) (bool, error) {
	if err := absolute(location); err != nil {
		return false, err
	}
	switch info, err := os.Lstat(location); {
	case errors.Is(err, os.ErrNotExist):
		return false, nil
	case err != nil:
		return false, err
	case !privateFile(info):
		return false, errors.New("sqlite: database must be a private regular file")
	}
	return true, nil
}

// own takes the exclusive lock beside the database at location.
func own(location string) (*os.File, error) {
	if err := localfs.Require(location, "database"); err != nil {
		return nil, fmt.Errorf("sqlite: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(location), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(location+".lock", os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("sqlite: %s: %w", location, storage.ErrInUse)
		}
		return nil, err
	}
	return f, nil
}

func (backend) Open(ctx context.Context, location string) (storage.Database, error) {
	if err := absolute(location); err != nil {
		return nil, err
	}
	lock, err := own(location)
	if err != nil {
		return nil, err
	}
	s, err := Open(ctx, location)
	if err != nil {
		lock.Close()
		return nil, err
	}
	return owned{s, lock}, nil
}

// owned is a Store opened through the backend, holding its location's lock
// until Close.
type owned struct {
	*Store
	lock *os.File
}

func (o owned) Close() error { return errors.Join(o.Store.Close(), o.lock.Close()) }

func (backend) Restore(ctx context.Context, snapshot, destination string, now time.Time) error {
	if err := absolute(destination); err != nil {
		return err
	}
	return Restore(ctx, snapshot, destination, now)
}

var _ storage.ReplicaRestorer = backend{}

// RestoreReplica implements storage.ReplicaRestorer. Restore reads its source
// immutable, which ignores a WAL, so frames a replicator left beside the file
// are checkpointed into a private copy first. The copy is owner-only whatever
// mode the replicator wrote the file with. Tables a replicator added stay.
func (backend) RestoreReplica(ctx context.Context, replica, destination string, now time.Time) error {
	if err := absolute(destination); err != nil {
		return err
	}
	source, err := filepath.Abs(replica)
	if err != nil {
		return err
	}
	switch info, err := os.Stat(source); {
	case err != nil:
		return err
	case !info.Mode().IsRegular():
		return fmt.Errorf("sqlite: replica %s is not a regular file: %w", source, storage.ErrInvalid)
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(filepath.Dir(destination), ".sqlite-replica-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	copied := filepath.Join(tmp, "conduit.db")
	if err := copyPrivate(source, copied); err != nil {
		return err
	}
	switch err := copyPrivate(source+"-wal", copied+"-wal"); {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return err
	default:
		if err := absorbWAL(ctx, copied); err != nil {
			return fmt.Errorf("sqlite: applying the replica's WAL: %w", err)
		}
	}
	return Restore(ctx, copied, destination, now)
}

// absorbWAL checkpoints a private copy's WAL into its database file.
func absorbWAL(ctx context.Context, path string) error {
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: path}).String())
	if err != nil {
		return err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	var busy, log, done int
	if err := db.QueryRowContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)").Scan(&busy, &log, &done); err != nil {
		return err
	}
	if busy != 0 {
		return errors.New("the checkpoint could not finish")
	}
	return nil
}

// ReadSnapshot restores the snapshot to a private file beside it and reads
// that, so a snapshot of an older format is read as Open would raise it.
func (backend) ReadSnapshot(ctx context.Context, snapshot string, now time.Time, read func(storage.Tx) error) error {
	if read == nil {
		return storage.ErrInvalid
	}
	abs, err := filepath.Abs(snapshot)
	if err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(filepath.Dir(abs), ".sqlite-read-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	copied := filepath.Join(tmp, "conduit.db")
	if err := Restore(ctx, abs, copied, now); err != nil {
		return err
	}
	s, err := Open(ctx, copied)
	if err != nil {
		return err
	}
	defer s.Close()
	return s.View(ctx, read)
}

// Inspect opens a private copy of the database and its WAL, so nothing
// Open does (recovering the WAL, recording the record format) reaches the
// original. It owns the original while the inspection is open, so no
// daemon writes it meanwhile. The copy is removed on Close.
func (b backend) Inspect(ctx context.Context, location string) (storage.Inspection, error) {
	switch present, err := b.Present(ctx, location); {
	case err != nil:
		return nil, err
	case !present:
		return nil, fmt.Errorf("sqlite: no database at %s: %w", location, os.ErrNotExist)
	}
	lock, err := own(location)
	if err != nil {
		return nil, err
	}
	i, err := inspect(ctx, location)
	if err != nil {
		lock.Close()
		return nil, err
	}
	i.lock = lock
	return i, nil
}

// inspectAttempts is how many times a copy that changed while it was read is
// taken again.
const inspectAttempts = 5

// inspectCopied is a test seam, called after each copy and before it is
// checked against the source.
var inspectCopied = func(string) {}

// inspect copies the database and its WAL. The daemon's lock doesn't stop a
// replicator checkpointing the file while it is read, and a copy torn that way
// reads as corruption, so one that changed is taken again. The -shm is left
// behind: it only indexes the WAL, SQLite rebuilds it, and a stale copy could
// point at frames from before a checkpoint.
func inspect(ctx context.Context, abs string) (inspection, error) {
	for range inspectAttempts {
		if err := ctx.Err(); err != nil {
			return inspection{}, err
		}
		before, err := fileState(abs)
		if err != nil {
			return inspection{}, err
		}
		tmp, err := os.MkdirTemp("", "oc-sqlite-inspect-")
		if err != nil {
			return inspection{}, err
		}
		copied := filepath.Join(tmp, "conduit.db")
		for _, suffix := range []string{"", "-wal"} {
			if err := copyPrivate(abs+suffix, copied+suffix); err != nil && !(suffix != "" && errors.Is(err, os.ErrNotExist)) {
				os.RemoveAll(tmp)
				return inspection{}, err
			}
		}
		inspectCopied(abs)
		after, err := fileState(abs)
		if err != nil {
			os.RemoveAll(tmp)
			return inspection{}, err
		}
		if before != after {
			os.RemoveAll(tmp)
			continue
		}
		s, err := Open(ctx, copied)
		if err != nil {
			os.RemoveAll(tmp)
			return inspection{}, err
		}
		return inspection{s: s, dir: tmp}, nil
	}
	return inspection{}, fmt.Errorf("sqlite: %s kept changing while it was read: stop whatever is writing to it (a replicator, say) and check again", abs)
}

// databaseFiles is the size and modification time of a database and its WAL.
type databaseFiles struct {
	size, walSize   int64
	mtime, walMtime int64
}

func fileState(abs string) (databaseFiles, error) {
	info, err := os.Stat(abs)
	if err != nil {
		return databaseFiles{}, err
	}
	state := databaseFiles{size: info.Size(), mtime: info.ModTime().UnixNano()}
	switch wal, err := os.Stat(abs + "-wal"); {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return databaseFiles{}, err
	default:
		state.walSize, state.walMtime = wal.Size(), wal.ModTime().UnixNano()
	}
	return state, nil
}

// inspection is an opened private copy. Only its Viewer side is offered:
// writes to the copy would be lost, so it is read through View alone.
type inspection struct {
	s    *Store
	dir  string
	lock *os.File
}

func (i inspection) View(ctx context.Context, fn func(storage.Tx) error) error {
	return i.s.View(ctx, fn)
}
func (i inspection) IntegrityCheck(ctx context.Context) error { return i.s.IntegrityCheck(ctx) }
func (i inspection) SchemaVersion() uint32                    { return i.s.SchemaVersion() }
func (i inspection) Stored(ctx context.Context) (storage.StoredVersion, error) {
	return i.s.Stored(ctx)
}
func (i inspection) Close() error {
	return errors.Join(i.s.Close(), os.RemoveAll(i.dir), i.lock.Close())
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

// OpenPrior reads the stored versions over a query_only connection, which
// never runs Open's raise of the format, its indexes or its rewrites.
// Writes is the versions this binary writes.
func (backend) Writes() storage.StoredVersion {
	return storage.StoredVersion{Schema: SchemaVersion, Records: recordFormat}
}

func (b backend) OpenPrior(ctx context.Context, location string) (storage.Prior, error) {
	switch present, err := b.Present(ctx, location); {
	case err != nil:
		return nil, err
	case !present:
		return nil, nil
	}
	lock, err := own(location)
	if err != nil {
		return nil, err
	}
	u := url.URL{Scheme: "file", Path: location}
	q := u.Query()
	q.Add("_pragma", fmt.Sprintf("busy_timeout(%d)", busyTimeout.Milliseconds()))
	q.Add("_pragma", "query_only(1)")
	u.RawQuery = q.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		lock.Close()
		return nil, err
	}
	db.SetMaxOpenConns(1)
	p := &prior{s: &Store{db: db, readers: db, path: location}, lock: lock}
	var schema uint32
	if err = db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&schema); err != nil {
		return nil, errors.Join(err, p.Close())
	}
	// Schema 0 is a database Open will create; a newer one it refuses.
	if schema == 0 || schema > SchemaVersion {
		return nil, p.Close()
	}
	records, err := storedRecordFormat(ctx, db)
	if err != nil {
		return nil, errors.Join(err, p.Close())
	}
	if schema == SchemaVersion && records >= recordFormat {
		return nil, p.Close()
	}
	p.stored = storage.StoredVersion{Schema: schema, Records: records}
	return p, nil
}

// prior is a database held as an older binary left it.
type prior struct {
	s      *Store
	lock   *os.File
	stored storage.StoredVersion
}

func (p *prior) Stored() storage.StoredVersion { return p.stored }
func (p *prior) Snapshot(ctx context.Context, path string, read func(storage.Tx) error) error {
	return p.s.snapshot(ctx, path, read, p.stored.Schema)
}
func (p *prior) Close() error { return errors.Join(p.s.db.Close(), p.lock.Close()) }

// Snapshot takes an online backup to path, then reads that copy, so read
// sees exactly the state the snapshot holds.
func (s *Store) Snapshot(ctx context.Context, path string, read func(storage.Tx) error) error {
	return s.snapshot(ctx, path, read, SchemaVersion)
}

func (s *Store) snapshot(ctx context.Context, path string, read func(storage.Tx) error, oldest uint32) error {
	if err := s.backup(ctx, path, oldest); err != nil {
		return err
	}
	if read == nil {
		return nil
	}
	// Read it immutable: no lock, no WAL and no -shm beside it, so a snapshot
	// can be taken onto a mount that couldn't hold a live database.
	db, err := readonlyDB(path)
	if err != nil {
		return err
	}
	err = (&Store{db: db, readers: db, path: path}).View(ctx, read)
	return errors.Join(err, db.Close())
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
