package sqlite

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/internal/localfs"
	"github.com/open-conduit/open-conduit/storage"
)

// archiveSchemaVersion is the schema of the index and of each month's file.
const archiveSchemaVersion = 1

// archiveChunk bounds the IDs or sequences one archive query binds.
const archiveChunk = 400

// archiveReaders bounds the connections reading the index at once.
const archiveReaders = 4

// ArchiveBackend is the built-in event archive: a directory holding one
// SQLite file per calendar month (UTC) of ReceivedAt with the whole records,
// and index.db, which maps each ID, sequence and Work to its month. A read
// asks the index and opens only the months its page falls in, so its cost
// doesn't grow with the archive's age and an idle month holds no file open.
var ArchiveBackend storage.EventArchiveBackend = archiveBackend{}

type archiveBackend struct{}

var (
	archiveFile   = regexp.MustCompile(`^events-([0-9]{4}-[0-9]{2})\.db$`)
	archivePeriod = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}$`)
)

var archiveIndexSchema = []string{
	`CREATE TABLE records (sequence INTEGER PRIMARY KEY, id TEXT NOT NULL UNIQUE, period TEXT NOT NULL, work_id TEXT NOT NULL, routed_work_id TEXT NOT NULL)`,
	`CREATE INDEX records_work ON records(work_id, sequence)`,
	`CREATE INDEX records_routed_work ON records(routed_work_id, sequence)`,
	fmt.Sprintf(`PRAGMA user_version=%d`, archiveSchemaVersion),
}

type eventArchive struct {
	// state guards closed: every call holds it to read, and Close to write,
	// so Close waits for calls in progress. write serialises PutEvents.
	// Reads take neither lock against a write: a record and its index entry
	// commit together and are never removed, so an entry a read finds has
	// its record.
	state  sync.RWMutex
	write  sync.Mutex
	closed bool
	dir    string
	lock   *os.File
	writer *sql.DB
	reader *sql.DB
	// periodReads counts the month files reads have opened.
	periodReads atomic.Int64
}

func archiveDSN(path string, pragmas ...string) string {
	u := url.URL{Scheme: "file", Path: path}
	q := u.Query()
	q.Add("_pragma", fmt.Sprintf("busy_timeout(%d)", busyTimeout.Milliseconds()))
	q.Add("_pragma", "synchronous(FULL)")
	for _, p := range pragmas {
		q.Add("_pragma", p)
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// ensureArchiveFile creates path privately, or checks the one there is.
func ensureArchiveFile(path string) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	info, err := f.Stat()
	if err = errors.Join(err, f.Close()); err != nil {
		return err
	}
	l, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !privateFile(l) || !os.SameFile(info, l) {
		return fmt.Errorf("sqlite: archive file must be a private regular file")
	}
	return nil
}

// Open implements storage.EventArchiveBackend. It holds an exclusive lock on
// the directory until Close, so a second owner gets storage.ErrInUse, and
// refuses a file a newer binary wrote before anything is read from it.
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
	a := &eventArchive{dir: dir, lock: lock}
	if err = a.open(context.WithoutCancel(ctx)); err != nil {
		_ = a.Close()
		return nil, err
	}
	return a, nil
}

func (a *eventArchive) open(ctx context.Context) error {
	index := filepath.Join(a.dir, "index.db")
	if err := ensureArchiveFile(index); err != nil {
		return err
	}
	var err error
	if a.writer, err = sql.Open("sqlite", archiveDSN(index)); err != nil {
		return err
	}
	a.writer.SetMaxOpenConns(1)
	if err = migrateArchiveIndex(ctx, a.writer); err != nil {
		return fmt.Errorf("sqlite: archive index: %w", err)
	}
	if a.reader, err = sql.Open("sqlite", archiveDSN(index, "query_only(1)")); err != nil {
		return err
	}
	a.reader.SetMaxOpenConns(archiveReaders)
	entries, err := os.ReadDir(a.dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if m := archiveFile.FindStringSubmatch(entry.Name()); m != nil {
			if err = checkArchivePeriod(ctx, filepath.Join(a.dir, entry.Name())); err != nil {
				return fmt.Errorf("sqlite: archive %s: %w", m[1], err)
			}
		}
	}
	return nil
}

func migrateArchiveIndex(ctx context.Context, db *sql.DB) error {
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
	for _, statement := range archiveIndexSchema {
		if _, err = tx.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func checkArchivePeriod(ctx context.Context, path string) error {
	l, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !privateFile(l) {
		return fmt.Errorf("archive file must be a private regular file")
	}
	db, err := openArchivePeriod(path)
	if err != nil {
		return err
	}
	defer db.Close()
	var version int
	if err = db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if version > archiveSchemaVersion {
		return fmt.Errorf("%w (archive schema %d; this binary supports %d)", storage.ErrSchemaNewer, version, archiveSchemaVersion)
	}
	return nil
}

// openArchivePeriod opens an existing month to read. It is read-write at the
// file so SQLite can roll back a journal a crash left, and query_only above
// that; mode=rw never creates a month a stale index entry names.
func openArchivePeriod(path string) (*sql.DB, error) {
	u, err := url.Parse(archiveDSN(path, "query_only(1)"))
	if err != nil {
		return nil, err
	}
	q := u.Query()
	q.Set("mode", "rw")
	u.RawQuery = q.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err == nil {
		db.SetMaxOpenConns(1)
	}
	return db, err
}

func archivable(e domain.AcceptedEvent) bool {
	return e.Routed && !e.Retired && e.Event.ID != "" && e.Sequence > 0 && e.Sequence <= 1<<63-1 && !e.Event.ReceivedAt.IsZero()
}

func archivePeriodOf(e domain.AcceptedEvent) string {
	return e.Event.ReceivedAt.UTC().Format("2006-01")
}

// archiveEntry is one index row.
type archiveEntry struct {
	sequence uint64
	id       domain.ID
	period   string
}

func scanArchiveEntries(rows *sql.Rows, out []archiveEntry) ([]archiveEntry, error) {
	defer rows.Close()
	for rows.Next() {
		var e archiveEntry
		var id string
		if err := rows.Scan(&e.sequence, &id, &e.period); err != nil {
			return nil, err
		}
		e.id = domain.ID(id)
		out = append(out, e)
	}
	return out, rows.Err()
}

// located reads the index entries holding any of events' IDs or sequences.
func (a *eventArchive) located(ctx context.Context, events []domain.AcceptedEvent) ([]archiveEntry, error) {
	var out []archiveEntry
	for chunk := range slices.Chunk(events, archiveChunk) {
		marks := strings.TrimSuffix(strings.Repeat("?,", len(chunk)), ",")
		args := make([]any, 0, 2*len(chunk))
		for _, e := range chunk {
			args = append(args, string(e.Event.ID))
		}
		for _, e := range chunk {
			args = append(args, e.Sequence)
		}
		rows, err := a.reader.QueryContext(ctx, "SELECT sequence,id,period FROM records WHERE id IN ("+marks+") OR sequence IN ("+marks+")", args...)
		if err != nil {
			return nil, mapError(err)
		}
		if out, err = scanArchiveEntries(rows, out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// records reads the entries' whole records, opening each month they are in
// once, and fails closed on an entry whose record is missing.
func (a *eventArchive) records(ctx context.Context, entries []archiveEntry) (map[uint64]domain.AcceptedEvent, error) {
	byPeriod := map[string][]uint64{}
	for _, e := range entries {
		byPeriod[e.period] = append(byPeriod[e.period], e.sequence)
	}
	out := make(map[uint64]domain.AcceptedEvent, len(entries))
	for period, sequences := range byPeriod {
		if err := a.readPeriod(ctx, period, sequences, out); err != nil {
			return nil, fmt.Errorf("sqlite: archive %s: %w", period, err)
		}
	}
	for _, e := range entries {
		if _, ok := out[e.sequence]; !ok {
			return nil, fmt.Errorf("sqlite: archive %s has no record for sequence %d", e.period, e.sequence)
		}
	}
	return out, nil
}

func (a *eventArchive) readPeriod(ctx context.Context, period string, sequences []uint64, out map[uint64]domain.AcceptedEvent) error {
	if !archivePeriod.MatchString(period) {
		return fmt.Errorf("invalid period %q", period)
	}
	a.periodReads.Add(1)
	db, err := openArchivePeriod(filepath.Join(a.dir, "events-"+period+".db"))
	if err != nil {
		return err
	}
	defer db.Close()
	for chunk := range slices.Chunk(sequences, archiveChunk) {
		args := make([]any, len(chunk))
		for i, s := range chunk {
			args[i] = s
		}
		rows, err := db.QueryContext(ctx, "SELECT record FROM events WHERE sequence IN ("+strings.TrimSuffix(strings.Repeat("?,", len(chunk)), ",")+")", args...)
		if err != nil {
			return mapError(err)
		}
		for rows.Next() {
			var b []byte
			if err = rows.Scan(&b); err != nil {
				rows.Close()
				return err
			}
			var v domain.AcceptedEvent
			if err = json.Unmarshal(b, &v); err != nil {
				rows.Close()
				return err
			}
			normalize(&v)
			out[v.Sequence] = v
		}
		if err = errors.Join(rows.Err(), rows.Close()); err != nil {
			return err
		}
	}
	return nil
}

// PutEvents implements storage.EventArchive. Every record is checked against
// the batch and the index before the first write, so a refused batch stores
// nothing. Each month commits with its index entries in one transaction;
// a batch that fails between months is completed by running it again.
func (a *eventArchive) PutEvents(ctx context.Context, events []domain.AcceptedEvent) error {
	a.state.RLock()
	defer a.state.RUnlock()
	if a.closed {
		return storage.ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	for _, e := range events {
		if !archivable(e) {
			return storage.ErrInvalid
		}
	}
	a.write.Lock()
	defer a.write.Unlock()
	work := context.WithoutCancel(ctx)
	byID, bySequence := map[domain.ID]domain.AcceptedEvent{}, map[uint64]domain.AcceptedEvent{}
	unique := make([]domain.AcceptedEvent, 0, len(events))
	for _, e := range events {
		old, sameID := byID[e.Event.ID]
		if sameID && !storage.SameAcceptedEvent(old, e) {
			return storage.ErrConflict
		}
		if other, ok := bySequence[e.Sequence]; ok && !storage.SameAcceptedEvent(other, e) {
			return storage.ErrConflict
		}
		if !sameID {
			byID[e.Event.ID], bySequence[e.Sequence] = e, e
			unique = append(unique, e)
		}
	}
	entries, err := a.located(work, unique)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		e, ok := byID[entry.id]
		if !ok || e.Sequence != entry.sequence {
			return storage.ErrConflict
		}
	}
	held, err := a.records(work, entries)
	if err != nil {
		return err
	}
	byPeriod := map[string][]domain.AcceptedEvent{}
	for _, e := range unique {
		if old, ok := held[e.Sequence]; ok {
			if !storage.SameAcceptedEvent(old, e) {
				return storage.ErrConflict
			}
			continue
		}
		period := archivePeriodOf(e)
		byPeriod[period] = append(byPeriod[period], e)
	}
	periods := make([]string, 0, len(byPeriod))
	for period := range byPeriod {
		periods = append(periods, period)
	}
	slices.Sort(periods)
	for _, period := range periods {
		if err = ctx.Err(); err != nil {
			return err
		}
		if err = a.writePeriod(work, period, byPeriod[period]); err != nil {
			return err
		}
	}
	return nil
}

// writePeriod commits events to their month and the index together: the
// month is attached to the index's connection, and SQLite commits a
// transaction across attached rollback-journal files atomically.
func (a *eventArchive) writePeriod(ctx context.Context, period string, events []domain.AcceptedEvent) (err error) {
	path := filepath.Join(a.dir, "events-"+period+".db")
	if err = ensureArchiveFile(path); err != nil {
		return err
	}
	conn, err := a.writer.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err = conn.ExecContext(ctx, "ATTACH DATABASE ? AS period", path); err != nil {
		return mapError(err)
	}
	defer func() {
		if _, e := conn.ExecContext(ctx, "DETACH DATABASE period"); e != nil {
			// A connection left attached can't attach the next month.
			_ = conn.Raw(func(c any) error {
				if closer, ok := c.(io.Closer); ok {
					_ = closer.Close()
				}
				return driver.ErrBadConn
			})
		}
	}()
	if _, err = conn.ExecContext(ctx, "PRAGMA period.synchronous=FULL"); err != nil {
		return err
	}
	if _, err = conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return mapError(err)
	}
	committed := false
	defer func() {
		if !committed {
			rollback(ctx, conn)
		}
	}()
	var version int
	if err = conn.QueryRowContext(ctx, "PRAGMA period.user_version").Scan(&version); err != nil {
		return err
	}
	switch {
	case version > archiveSchemaVersion:
		return fmt.Errorf("sqlite: archive %s: %w (archive schema %d; this binary supports %d)", period, storage.ErrSchemaNewer, version, archiveSchemaVersion)
	case version == 0:
		if _, err = conn.ExecContext(ctx, `CREATE TABLE period.events (sequence INTEGER PRIMARY KEY, id TEXT NOT NULL UNIQUE, record BLOB NOT NULL)`); err != nil {
			return err
		}
		if _, err = conn.ExecContext(ctx, fmt.Sprintf("PRAGMA period.user_version=%d", archiveSchemaVersion)); err != nil {
			return err
		}
	}
	for _, e := range events {
		b, err := encode(e)
		if err != nil {
			return err
		}
		if _, err = conn.ExecContext(ctx, "INSERT INTO main.records(sequence,id,period,work_id,routed_work_id) VALUES(?,?,?,?,?)", e.Sequence, string(e.Event.ID), period, string(e.Event.WorkID), string(e.RoutedWorkID)); err != nil {
			return mapError(err)
		}
		if _, err = conn.ExecContext(ctx, "INSERT INTO period.events(sequence,id,record) VALUES(?,?,?)", e.Sequence, string(e.Event.ID), b); err != nil {
			return mapError(err)
		}
	}
	if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
		return mapError(err)
	}
	committed = true
	return nil
}

// Event implements storage.EventArchive.
func (a *eventArchive) Event(ctx context.Context, id domain.ID) (domain.AcceptedEvent, error) {
	out, err := a.page(ctx, "SELECT sequence,id,period FROM records WHERE id=?", string(id))
	if err != nil {
		return domain.AcceptedEvent{}, err
	}
	if len(out) == 0 {
		return domain.AcceptedEvent{}, storage.ErrNotFound
	}
	return out[0], nil
}

// Events implements storage.EventArchive.
func (a *eventArchive) Events(ctx context.Context, after uint64, limit int) ([]domain.AcceptedEvent, error) {
	if after > 1<<63-1 {
		return a.empty()
	}
	return a.page(ctx, "SELECT sequence,id,period FROM records WHERE sequence>? ORDER BY sequence LIMIT ?", after, archiveLimit(limit))
}

// WorkEvents implements storage.EventArchive.
func (a *eventArchive) WorkEvents(ctx context.Context, work domain.ID, after uint64, limit int) ([]domain.AcceptedEvent, error) {
	if work == "" || after > 1<<63-1 {
		return a.empty()
	}
	return a.page(ctx, "SELECT sequence,id,period FROM records WHERE (work_id=? OR routed_work_id=?) AND sequence>? ORDER BY sequence LIMIT ?", string(work), string(work), after, archiveLimit(limit))
}

func archiveLimit(limit int) int {
	if limit <= 0 {
		return -1
	}
	return limit
}

func (a *eventArchive) empty() ([]domain.AcceptedEvent, error) {
	a.state.RLock()
	defer a.state.RUnlock()
	if a.closed {
		return nil, storage.ErrInvalid
	}
	return []domain.AcceptedEvent{}, nil
}

// page reads the index entries query selects and their whole records, in
// the order the index gave them.
func (a *eventArchive) page(ctx context.Context, query string, args ...any) ([]domain.AcceptedEvent, error) {
	a.state.RLock()
	defer a.state.RUnlock()
	if a.closed {
		return nil, storage.ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	work := context.WithoutCancel(ctx)
	rows, err := a.reader.QueryContext(work, query, args...)
	if err != nil {
		return nil, mapError(err)
	}
	entries, err := scanArchiveEntries(rows, nil)
	if err != nil {
		return nil, err
	}
	records, err := a.records(work, entries)
	if err != nil {
		return nil, err
	}
	out := make([]domain.AcceptedEvent, 0, len(entries))
	for _, e := range entries {
		out = append(out, records[e.sequence])
	}
	return out, nil
}

// Close implements storage.EventArchive.
func (a *eventArchive) Close() error {
	a.state.Lock()
	defer a.state.Unlock()
	if a.closed {
		return nil
	}
	a.closed = true
	var errs []error
	for _, db := range []*sql.DB{a.reader, a.writer} {
		if db != nil {
			errs = append(errs, db.Close())
		}
	}
	errs = append(errs, a.lock.Close())
	return errors.Join(errs...)
}
