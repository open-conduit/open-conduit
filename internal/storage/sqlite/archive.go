package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sync"
	"syscall"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/internal/localfs"
	"github.com/open-conduit/open-conduit/storage"
)

// archiveSchemaVersion is the schema of one period's archive file.
const archiveSchemaVersion = 1

// ArchiveBackend is the built-in event archive: a directory holding one
// SQLite file per calendar month (UTC) of ReceivedAt.
var ArchiveBackend storage.EventArchiveBackend = archiveBackend{}

type archiveBackend struct{}

var archiveFile = regexp.MustCompile(`^events-([0-9]{4}-[0-9]{2})\.db$`)

var archiveSchema = []string{
	`CREATE TABLE events (id TEXT PRIMARY KEY, sequence INTEGER NOT NULL UNIQUE, work_id TEXT NOT NULL, routed_work_id TEXT NOT NULL, record BLOB NOT NULL)`,
	`CREATE INDEX events_work ON events(work_id, sequence)`,
	`CREATE INDEX events_routed_work ON events(routed_work_id, sequence)`,
	fmt.Sprintf(`PRAGMA user_version=%d`, archiveSchemaVersion),
}

type eventArchive struct {
	mu      sync.RWMutex
	dir     string
	lock    *os.File
	periods map[string]*sql.DB
}

// Open implements storage.EventArchiveBackend. It holds an exclusive lock on
// the directory until Close, so a second owner gets storage.ErrInUse.
func (archiveBackend) Open(ctx context.Context, location string) (storage.EventArchive, error) {
	if !filepath.IsAbs(location) {
		return nil, storage.ErrInvalid
	}
	dir := filepath.Clean(location)
	if err := localfs.Require(dir, "event archive"); err != nil {
		return nil, fmt.Errorf("sqlite: %w", err)
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(filepath.Join(dir, ".lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, storage.ErrInUse
		}
		return nil, err
	}
	a := &eventArchive{dir: dir, lock: lock, periods: map[string]*sql.DB{}}
	entries, err := os.ReadDir(dir)
	if err != nil {
		a.Close()
		return nil, err
	}
	for _, entry := range entries {
		m := archiveFile.FindStringSubmatch(entry.Name())
		if m == nil {
			continue
		}
		if _, err = a.period(ctx, m[1]); err != nil {
			a.Close()
			return nil, err
		}
	}
	return a, nil
}

// period opens one month's file, creating it when it is absent. The caller
// holds mu for writing, or is Open.
func (a *eventArchive) period(ctx context.Context, name string) (*sql.DB, error) {
	if db, ok := a.periods[name]; ok {
		return db, nil
	}
	path := filepath.Join(a.dir, "events-"+name+".db")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	closeErr := f.Close()
	if err != nil {
		return nil, err
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if l, e := os.Lstat(path); e != nil {
		return nil, e
	} else if !privateFile(l) || !os.SameFile(info, l) {
		return nil, fmt.Errorf("sqlite: archive file must be a private regular file")
	}
	u := url.URL{Scheme: "file", Path: path}
	q := u.Query()
	q.Add("_pragma", fmt.Sprintf("busy_timeout(%d)", busyTimeout.Milliseconds()))
	q.Add("_pragma", "synchronous(FULL)")
	u.RawQuery = q.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err = migrateArchive(context.WithoutCancel(ctx), db); err != nil {
		db.Close()
		return nil, fmt.Errorf("sqlite: archive %s: %w", name, err)
	}
	a.periods[name] = db
	return db, nil
}

func migrateArchive(ctx context.Context, db *sql.DB) error {
	var version int
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	switch {
	case version > archiveSchemaVersion:
		return fmt.Errorf("%w (archive schema %d; this binary supports %d)", storage.ErrSchemaNewer, version, archiveSchemaVersion)
	case version == archiveSchemaVersion:
		return nil
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, statement := range archiveSchema {
		if _, err = tx.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func archivable(e domain.AcceptedEvent) bool {
	return e.Routed && !e.Retired && e.Event.ID != "" && e.Sequence > 0 && e.Sequence <= 1<<63-1 && !e.Event.ReceivedAt.IsZero()
}

// held finds the record stored under id or sequence, in any period.
func (a *eventArchive) held(ctx context.Context, id domain.ID, sequence uint64) ([][]byte, error) {
	var found [][]byte
	for _, db := range a.periods {
		rows, err := db.QueryContext(ctx, "SELECT record FROM events WHERE id=? OR sequence=?", string(id), sequence)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var b []byte
			if err = rows.Scan(&b); err != nil {
				rows.Close()
				return nil, err
			}
			found = append(found, b)
		}
		err = errors.Join(rows.Err(), rows.Close())
		if err != nil {
			return nil, err
		}
	}
	return found, nil
}

// PutEvents implements storage.EventArchive. Every record is checked against
// the batch and every period before the first write, so a refused batch
// stores nothing. Each period commits on its own, and a batch that fails
// between them is completed by running it again.
func (a *eventArchive) PutEvents(ctx context.Context, events []domain.AcceptedEvent) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.periods == nil {
		return storage.ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	work := context.WithoutCancel(ctx)
	type pending struct {
		event  domain.AcceptedEvent
		record []byte
	}
	byPeriod := map[string][]pending{}
	byID, bySequence := map[domain.ID][]byte{}, map[uint64][]byte{}
	for _, e := range events {
		if !archivable(e) {
			return storage.ErrInvalid
		}
	}
	for _, e := range events {
		b, err := encode(e)
		if err != nil {
			return err
		}
		if old, ok := byID[e.Event.ID]; ok && !bytes.Equal(old, b) {
			return storage.ErrConflict
		}
		if old, ok := bySequence[e.Sequence]; ok && !bytes.Equal(old, b) {
			return storage.ErrConflict
		}
		if _, ok := byID[e.Event.ID]; ok {
			continue
		}
		byID[e.Event.ID], bySequence[e.Sequence] = b, b
		found, err := a.held(work, e.Event.ID, e.Sequence)
		if err != nil {
			return err
		}
		if len(found) > 1 || len(found) == 1 && !bytes.Equal(found[0], b) {
			return storage.ErrConflict
		}
		if len(found) == 0 {
			name := e.Event.ReceivedAt.UTC().Format("2006-01")
			byPeriod[name] = append(byPeriod[name], pending{e, b})
		}
	}
	names := make([]string, 0, len(byPeriod))
	for name := range byPeriod {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return err
		}
		db, err := a.period(work, name)
		if err != nil {
			return err
		}
		tx, err := db.BeginTx(work, nil)
		if err != nil {
			return mapError(err)
		}
		for _, p := range byPeriod[name] {
			if _, err = tx.ExecContext(work, "INSERT INTO events(id,sequence,work_id,routed_work_id,record) VALUES(?,?,?,?,?)", string(p.event.Event.ID), p.event.Sequence, string(p.event.Event.WorkID), string(p.event.RoutedWorkID), p.record); err != nil {
				_ = tx.Rollback()
				return mapError(err)
			}
		}
		if err = tx.Commit(); err != nil {
			return mapError(err)
		}
	}
	return nil
}

// Event implements storage.EventArchive.
func (a *eventArchive) Event(ctx context.Context, id domain.ID) (domain.AcceptedEvent, error) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.periods == nil {
		return domain.AcceptedEvent{}, storage.ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return domain.AcceptedEvent{}, err
	}
	for _, db := range a.periods {
		out, err := scanArchive(context.WithoutCancel(ctx), db, "SELECT record FROM events WHERE id=?", string(id))
		if err != nil {
			return domain.AcceptedEvent{}, err
		}
		if len(out) > 0 {
			return out[0], nil
		}
	}
	return domain.AcceptedEvent{}, storage.ErrNotFound
}

// Events implements storage.EventArchive.
func (a *eventArchive) Events(ctx context.Context, after uint64, limit int) ([]domain.AcceptedEvent, error) {
	return a.merged(ctx, after, limit, "", nil)
}

// WorkEvents implements storage.EventArchive.
func (a *eventArchive) WorkEvents(ctx context.Context, work domain.ID, after uint64, limit int) ([]domain.AcceptedEvent, error) {
	if work == "" {
		return []domain.AcceptedEvent{}, nil
	}
	return a.merged(ctx, after, limit, "(work_id=? OR routed_work_id=?) AND ", []any{string(work), string(work)})
}

// merged reads each period's first limit matches and keeps the first limit
// of their union: a period's ReceivedAt range says nothing about its
// sequences, which a stepped clock can interleave.
func (a *eventArchive) merged(ctx context.Context, after uint64, limit int, where string, args []any) ([]domain.AcceptedEvent, error) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.periods == nil {
		return nil, storage.ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	out := []domain.AcceptedEvent{}
	if after > 1<<63-1 {
		return out, nil
	}
	bound := limit
	if bound <= 0 {
		bound = -1
	}
	query := "SELECT record FROM events WHERE " + where + "sequence>? ORDER BY sequence LIMIT ?"
	for _, db := range a.periods {
		page, err := scanArchive(context.WithoutCancel(ctx), db, query, append(slices.Clone(args), after, bound)...)
		if err != nil {
			return nil, err
		}
		out = append(out, page...)
	}
	slices.SortFunc(out, func(x, y domain.AcceptedEvent) int {
		switch {
		case x.Sequence < y.Sequence:
			return -1
		case x.Sequence > y.Sequence:
			return 1
		}
		return 0
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func scanArchive(ctx context.Context, db *sql.DB, query string, args ...any) ([]domain.AcceptedEvent, error) {
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, mapError(err)
	}
	defer rows.Close()
	var out []domain.AcceptedEvent
	for rows.Next() {
		var b []byte
		if err = rows.Scan(&b); err != nil {
			return nil, err
		}
		v, err := decodeArchived(b)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func decodeArchived(b []byte) (domain.AcceptedEvent, error) {
	var v domain.AcceptedEvent
	err := json.Unmarshal(b, &v)
	normalize(&v)
	return v, err
}

// Close implements storage.EventArchive.
func (a *eventArchive) Close() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.periods == nil {
		return nil
	}
	var errs []error
	for _, db := range a.periods {
		errs = append(errs, db.Close())
	}
	a.periods = nil
	errs = append(errs, a.lock.Close())
	return errors.Join(errs...)
}
