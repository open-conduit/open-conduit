package sqlite

import (
	"context"
	"crypto/sha256"
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
	content, err := copyDatabase(source, copied)
	if err != nil {
		return err
	}
	if content.hasWAL {
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

// RecoveryPoint states that a WAL replicator can keep a continuous copy of the
// database. A replicator is a separate process that leaves no signal SQLite
// can rely on, so this never claims that one is running.
func (backend) RecoveryPoint(context.Context, string) (storage.RecoveryPoint, error) {
	return storage.RecoveryPoint{
		Replication: storage.ReplicationUnobserved,
		Detail:      "a WAL replicator can keep a continuous copy of this SQLite database, but SQLite can't see whether one is running",
	}, nil
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

// inspectAttempts is how many copies are taken looking for two in a row with
// the same content.
const inspectAttempts = 5

// inspectCopied is a test seam, called after each copy.
var inspectCopied = func(string) {}

// inspect copies the database and its WAL. The daemon's lock doesn't stop a
// replicator checkpointing the file while it is read, and a copy torn that way
// reads as corruption, so a copy is used only once the one before it had the
// same content. Size and modification time can't show this: a checkpoint
// writes pages in place, and a write within one clock tick leaves the time
// alone. The -shm is left behind: it only indexes the WAL, SQLite rebuilds it,
// and a stale copy could point at frames from before a checkpoint.
func inspect(ctx context.Context, abs string) (inspection, error) {
	var previous databaseContent
	for attempt := range inspectAttempts {
		if err := ctx.Err(); err != nil {
			return inspection{}, err
		}
		tmp, err := os.MkdirTemp("", "oc-sqlite-inspect-")
		if err != nil {
			return inspection{}, err
		}
		copied := filepath.Join(tmp, "conduit.db")
		content, err := copyDatabase(abs, copied)
		if err != nil {
			os.RemoveAll(tmp)
			return inspection{}, err
		}
		inspectCopied(abs)
		if attempt == 0 || content != previous {
			previous = content
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

// databaseContent identifies the bytes of a database and its WAL, if any.
type databaseContent struct {
	db, wal [sha256.Size]byte
	hasWAL  bool
}

// copyDatabase copies a database and its WAL, if any, and returns what it read.
func copyDatabase(src, dst string) (databaseContent, error) {
	var content databaseContent
	var err error
	if content.db, err = copyPrivate(src, dst); err != nil {
		return databaseContent{}, err
	}
	switch content.wal, err = copyPrivate(src+"-wal", dst+"-wal"); {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return databaseContent{}, err
	default:
		content.hasWAL = true
	}
	return content, nil
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
	err := errors.Join(i.s.Close(), os.RemoveAll(i.dir))
	if i.lock != nil {
		err = errors.Join(err, i.lock.Close())
	}
	return err
}

// copyPrivate copies src to a new private file and returns the hash of the
// bytes it read.
func copyPrivate(src, dst string) ([sha256.Size]byte, error) {
	var sum [sha256.Size]byte
	in, err := os.Open(src)
	if err != nil {
		return sum, err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		return sum, err
	}
	hash := sha256.New()
	_, err = io.Copy(io.MultiWriter(out, hash), in)
	if err = errors.Join(err, out.Close()); err != nil {
		return sum, err
	}
	hash.Sum(sum[:0])
	return sum, nil
}

// OpenPrior reads the stored versions over a query_only connection, which
// never runs Open's raise of the format, its indexes or its rewrites.
// Writes is the versions this binary writes.
func (backend) Writes() storage.StoredVersion {
	return storage.StoredVersion{Schema: SchemaVersion, Records: recordFormat}
}

func (b backend) OpenPrior(ctx context.Context, location string) (storage.Prior, error) {
	return b.openPrior(ctx, location, true)
}

// OpenPriorUnowned is OpenPrior without the location's lock, as Open is to the
// backend's Open, for a backend that shares the location under the lease.
func OpenPriorUnowned(ctx context.Context, location string) (storage.Prior, error) {
	return backend{}.openPrior(ctx, location, false)
}

// InspectUnowned is Inspect without taking ownership of the location, as a
// shared backend's Inspect must not: a standby reads state a holder is running.
func InspectUnowned(ctx context.Context, location string) (storage.Inspection, error) {
	switch present, err := (backend{}).Present(ctx, location); {
	case err != nil:
		return nil, err
	case !present:
		return nil, fmt.Errorf("sqlite: no database at %s: %w", location, os.ErrNotExist)
	}
	i, err := inspect(ctx, location)
	if err != nil {
		return nil, err
	}
	return i, nil
}

// ViewUnowned is storage.SharedBackend's ViewInPlace for a backend that shares
// the location under the lease. It reads over a query_only connection, so it
// costs one read transaction however large the database is, and a holder's
// commits don't disturb it as they would a copy.
func ViewUnowned(ctx context.Context, location string, read func(storage.Tx) error) error {
	switch present, err := (backend{}).Present(ctx, location); {
	case err != nil:
		return err
	case !present:
		return fmt.Errorf("sqlite: no database at %s: %w", location, os.ErrNotExist)
	}
	db, err := queryOnlyDB(location)
	if err != nil {
		return err
	}
	defer db.Close()
	var schema uint32
	if err = db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&schema); err != nil {
		return mapError(err)
	}
	switch {
	case schema == 0:
		// Created but not yet migrated: nothing is stored in it.
		return fmt.Errorf("sqlite: no schema at %s: %w", location, os.ErrNotExist)
	case schema > SchemaVersion:
		return fmt.Errorf("sqlite: %w (schema %d; this binary supports %d)", storage.ErrSchemaNewer, schema, SchemaVersion)
	}
	records, err := storedRecordFormat(ctx, db)
	if err != nil {
		return err
	}
	if records > recordFormat {
		return fmt.Errorf("sqlite: %w (format %d; this binary writes %d)", storage.ErrRecordsNewer, records, recordFormat)
	}
	return (&Store{db: db, readers: db, path: location}).View(ctx, read)
}

// queryOnlyDB opens location for reading in place.
func queryOnlyDB(location string) (*sql.DB, error) {
	u := url.URL{Scheme: "file", Path: location}
	q := u.Query()
	q.Add("_pragma", fmt.Sprintf("busy_timeout(%d)", busyTimeout.Milliseconds()))
	q.Add("_pragma", "query_only(1)")
	u.RawQuery = q.Encode()
	return sql.Open("sqlite", u.String())
}

func (b backend) openPrior(ctx context.Context, location string, exclusive bool) (storage.Prior, error) {
	switch present, err := b.Present(ctx, location); {
	case err != nil:
		return nil, err
	case !present:
		return nil, nil
	}
	var lock *os.File
	if exclusive {
		var err error
		if lock, err = own(location); err != nil {
			return nil, err
		}
	}
	db, err := queryOnlyDB(location)
	if err != nil {
		if lock != nil {
			lock.Close()
		}
		return nil, err
	}
	// One connection stays pinned for the write position, and the other
	// reads and takes the snapshot.
	db.SetMaxOpenConns(2)
	p := &prior{s: &Store{db: db, readers: db, path: location}, lock: lock}
	if p.pin, err = db.Conn(ctx); err != nil {
		return nil, errors.Join(err, p.Close())
	}
	// Read first, so a commit after any read below is seen by Raise.
	if err = p.pin.QueryRowContext(ctx, "PRAGMA data_version").Scan(&p.position); err != nil {
		return nil, errors.Join(err, p.Close())
	}
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
	// A live instance is still changing this state, so a copy of it now
	// would be stale by the time Open could raise it.
	expiry, err := recordedExpiry(ctx, db)
	if err != nil {
		return nil, errors.Join(err, p.Close())
	}
	if expiry.Valid && expiry.Int64 > time.Now().UnixNano() {
		return nil, errors.Join(storage.ErrInstanceHeld, p.Close())
	}
	p.stored = storage.StoredVersion{Schema: schema, Records: records}
	return p, nil
}

// prior is a database held as an older binary left it.
type prior struct {
	s      *Store
	lock   *os.File
	stored storage.StoredVersion
	// pin is the connection position was read on: PRAGMA data_version
	// changes when another connection commits, and means nothing across
	// connections.
	pin      *sql.Conn
	position int64
}

var _ storage.PriorRaiser = (*prior)(nil)

// Raise implements storage.PriorRaiser. The comparison and the raise share one
// write transaction on the pinned connection, so nothing can commit between
// them.
func (p *prior) Raise(ctx context.Context) error {
	for _, pragma := range []string{"PRAGMA query_only=0", "PRAGMA synchronous=FULL", "BEGIN IMMEDIATE"} {
		if _, err := p.pin.ExecContext(ctx, pragma); err != nil {
			return mapError(err)
		}
	}
	defer p.pin.ExecContext(context.Background(), "ROLLBACK")
	var now int64
	if err := p.pin.QueryRowContext(ctx, "PRAGMA data_version").Scan(&now); err != nil {
		return mapError(err)
	}
	if now != p.position {
		return fmt.Errorf("sqlite: %w", storage.ErrStateMoved)
	}
	return raiseAndCommit(ctx, p.pin)
}

func (p *prior) Stored() storage.StoredVersion { return p.stored }
func (p *prior) Snapshot(ctx context.Context, path string, read func(storage.Tx) error) error {
	return p.s.snapshot(ctx, path, read, p.stored.Schema)
}
func (p *prior) Close() error {
	var err error
	if p.pin != nil {
		err = p.pin.Close()
		if errors.Is(err, sql.ErrConnDone) {
			err = nil
		}
	}
	err = errors.Join(err, p.s.db.Close())
	if p.lock != nil {
		err = errors.Join(err, p.lock.Close())
	}
	return err
}

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

// Fence clears every Execution lease's owner and expiry in one transaction,
// keeping epochs. It is an ordinary write naming no holder, so it is refused
// while an instance holds the state.
func (s *Store) Fence(ctx context.Context, now time.Time) error {
	if now.IsZero() {
		return storage.ErrInvalid
	}
	return s.Transact(ctx, storage.Guard{Now: now}, func(tx storage.Tx) error {
		return tx.(*transaction).fenceExecutions(now)
	})
}

// fenceRestored is Fence for a private copy being restored, which nothing
// else can reach: it also ends the tenure the copy recorded, since that
// holder has no authority over the restored state.
func (s *Store) fenceRestored(ctx context.Context, now time.Time) error {
	if now.IsZero() {
		return storage.ErrInvalid
	}
	return s.lease(ctx, now, func(t *transaction) error {
		if err := t.fenceExecutions(now); err != nil {
			return err
		}
		_, err := t.exec(`UPDATE instance_lease SET expires_at=NULL WHERE id=1`)
		return err
	})
}
