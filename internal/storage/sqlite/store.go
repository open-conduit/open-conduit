// Package sqlite implements serializable, durable coordination storage.
package sqlite

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/storage"
	modernsqlite "modernc.org/sqlite"
)

const SchemaVersion uint32 = 1

// busyTimeout bounds how long a connection waits for a competing connection's lock.
const busyTimeout = 5 * time.Second

type Store struct {
	db   *sql.DB
	path string
}

var _ storage.Store = (*Store)(nil)

// Open initializes an empty database, or validates the version of an existing one.
// It never resets, downgrades or reuses Open Supervisor state.
func Open(ctx context.Context, path string) (*Store, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if err = os.MkdirAll(filepath.Dir(abs), 0700); err != nil {
		return nil, err
	}
	if info, e := os.Lstat(abs); e == nil {
		if !privateFile(info) {
			return nil, fmt.Errorf("sqlite: database must be a private regular file")
		}
	} else if !os.IsNotExist(e) {
		return nil, e
	} else {
		f, e := os.OpenFile(abs, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
		if e != nil {
			if !os.IsExist(e) {
				return nil, e
			}
			info, statErr := os.Lstat(abs)
			if statErr != nil {
				return nil, statErr
			}
			if !privateFile(info) {
				return nil, fmt.Errorf("sqlite: database must be a private regular file")
			}
		} else if e = f.Close(); e != nil {
			return nil, e
		}
	}
	u := url.URL{Scheme: "file", Path: abs}
	q := u.Query()
	q.Add("_pragma", fmt.Sprintf("busy_timeout(%d)", busyTimeout.Milliseconds()))
	q.Add("_pragma", "foreign_keys(1)")
	q.Add("_pragma", "synchronous(FULL)")
	u.RawQuery = q.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db, path: abs}
	ok := false
	defer func() {
		if !ok {
			db.Close()
		}
	}()
	var version uint32
	if err = db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return nil, err
	}
	if version > SchemaVersion {
		return nil, fmt.Errorf("sqlite: schema %d is newer than supported %d; use a matching newer binary", version, SchemaVersion)
	}
	if version == 0 {
		if err = s.migrate(ctx); err != nil {
			return nil, err
		}
	}
	var storedVersion string
	if err = db.QueryRowContext(ctx, "SELECT value FROM metadata WHERE key='schema_version'").Scan(&storedVersion); err != nil {
		return nil, fmt.Errorf("sqlite: invalid schema metadata: %w", err)
	}
	if storedVersion != "1" {
		return nil, fmt.Errorf("sqlite: schema metadata mismatch")
	}

	if err = enableWAL(ctx, db); err != nil {
		return nil, err
	}
	for _, statement := range indexes {
		if _, err = db.ExecContext(ctx, statement); err != nil {
			return nil, fmt.Errorf("sqlite: index: %w", err)
		}
	}
	for _, r := range rewrites {
		if err = s.rewrite(ctx, r); err != nil {
			return nil, fmt.Errorf("sqlite: rewrite %s: %w", r.key, err)
		}
	}
	if err = s.IntegrityCheck(ctx); err != nil {
		return nil, err
	}
	ok = true
	return s, nil
}

// enableWAL switches the database to WAL, waiting out competing connections.
// The switch takes a read transaction and then upgrades it to an exclusive
// write. SQLite fails that upgrade with SQLITE_BUSY at once, without calling the
// busy handler, when another connection holds the write lock, which a
// concurrent first open does during its migration or its own switch to WAL.
// So the switch is retried here, within the same budget as the busy timeout.
func enableWAL(ctx context.Context, db *sql.DB) error {
	deadline := time.Now().Add(busyTimeout)
	delay := time.Millisecond
	for {
		var journal string
		err := db.QueryRowContext(ctx, "PRAGMA journal_mode=WAL").Scan(&journal)
		if err == nil {
			if journal != "wal" {
				return fmt.Errorf("sqlite: WAL unavailable")
			}
			return nil
		}
		var e *modernsqlite.Error
		if !errors.As(err, &e) || e.Code()&255 != 5 || time.Now().Add(delay).After(deadline) {
			return err
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
		delay = min(2*delay, 50*time.Millisecond)
	}
}
func (s *Store) Close() error          { return s.db.Close() }
func (s *Store) SchemaVersion() uint32 { return SchemaVersion }
func (s *Store) IntegrityCheck(ctx context.Context) error {
	var result string
	if err := s.db.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&result); err != nil {
		return err
	}
	if result != "ok" {
		return fmt.Errorf("sqlite: integrity check failed")
	}
	rows, err := s.db.QueryContext(ctx, "PRAGMA foreign_key_check")
	if err != nil {
		return err
	}
	defer rows.Close()
	if rows.Next() {
		return fmt.Errorf("sqlite: foreign key check failed")
	}
	return rows.Err()
}
func (s *Store) migrate(ctx context.Context) error {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err = conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return err
	}
	defer conn.ExecContext(context.Background(), "ROLLBACK")
	// Recheck under the write lock: two processes can open a new database together.
	var version int
	if err = conn.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if version == int(SchemaVersion) {
		_, err = conn.ExecContext(ctx, "COMMIT")
		return err
	}
	if version != 0 {
		return fmt.Errorf("sqlite: schema changed during migration")
	}
	var count int
	if err = conn.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'").Scan(&count); err != nil {
		return err
	}
	if count != 0 {
		return fmt.Errorf("sqlite: unversioned nonempty database; explicit migration required")
	}
	for _, statement := range schema {
		if _, err = conn.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("sqlite: migration rolled back: %w", err)
		}
	}
	_, err = conn.ExecContext(ctx, "COMMIT")
	return err
}

var schema = []string{
	`CREATE TABLE metadata (key TEXT PRIMARY KEY, value TEXT NOT NULL)`,
	`INSERT INTO metadata VALUES ('schema_version','1')`,
	`CREATE TABLE scopes (id TEXT PRIMARY KEY, parent_id TEXT REFERENCES scopes(id), record BLOB NOT NULL)`,
	`CREATE TABLE works (id TEXT PRIMARY KEY, scope_id TEXT NOT NULL REFERENCES scopes(id), parent_id TEXT REFERENCES works(id), record BLOB NOT NULL)`,
	`CREATE TABLE correlations (source TEXT NOT NULL, external_id TEXT NOT NULL, work_id TEXT NOT NULL REFERENCES works(id), PRIMARY KEY(source,external_id))`,
	`CREATE INDEX correlation_work ON correlations(work_id)`,
	`CREATE TABLE events (sequence INTEGER PRIMARY KEY AUTOINCREMENT, id TEXT NOT NULL UNIQUE, source TEXT NOT NULL, delivery_id TEXT NOT NULL, record BLOB NOT NULL, UNIQUE(source,delivery_id))`,
	`CREATE TABLE executions (id TEXT PRIMARY KEY, work_id TEXT NOT NULL REFERENCES works(id), attempt INTEGER NOT NULL CHECK(attempt>0), active INTEGER NOT NULL CHECK(active IN(0,1)), record BLOB NOT NULL, UNIQUE(work_id,attempt))`,
	`CREATE UNIQUE INDEX one_active_attempt ON executions(work_id) WHERE active=1`,
	`CREATE TABLE timer_versions (id TEXT PRIMARY KEY, revision INTEGER NOT NULL CHECK(revision>0))`,
	`CREATE TABLE timers (id TEXT PRIMARY KEY REFERENCES timer_versions(id), record BLOB NOT NULL)`,
	`CREATE TABLE waits (id TEXT PRIMARY KEY, work_id TEXT NOT NULL REFERENCES works(id), execution_id TEXT NOT NULL REFERENCES executions(id), record BLOB NOT NULL)`,
	`CREATE TABLE kv (namespace TEXT NOT NULL CHECK(length(namespace)>0), key TEXT NOT NULL CHECK(length(key)>0), value BLOB NOT NULL, PRIMARY KEY(namespace,key))`,
	`PRAGMA user_version=1`,
}

// unrouted selects events not yet marked routed. The routed flag lives in the
// record, so the partial index below covers exactly the unrouted backlog and
// routing never reads retained history (#36). Queries must repeat this exact
// expression for SQLite to use the index.
const unrouted = "json_extract(CAST(record AS TEXT),'$.Routed')=0"

// indexes are derived from records, so they are created on every open: an
// older database gains them without a schema version change, and an older
// binary that opens a database with them keeps them current.
var indexes = []string{
	`CREATE INDEX IF NOT EXISTS events_unrouted ON events(sequence) WHERE ` + unrouted,
}

// rewrites bring records written by earlier binaries up to date. Each runs
// once per store: it and its metadata key commit in one transaction, so a
// crash leaves it to run again and a later open skips it without a scan.
var rewrites = []struct{ key, statement string }{
	// Turns were once declared under a source-named "<source>.status.v1"
	// schema; the core now reads only domain.TurnSchema.
	{"rewrite_turn_schema", `UPDATE executions SET record=CAST(json_set(CAST(record AS TEXT),'$.Outcome.Data.Schema','` + domain.TurnSchema + `') AS BLOB) ` +
		`WHERE json_extract(CAST(record AS TEXT),'$.Outcome.Data.Schema') LIKE '_%.status.v1'`},
}

// rewrite runs one rewrite unless the store's metadata records it as done.
func (s *Store) rewrite(ctx context.Context, r struct{ key, statement string }) error {
	var done int
	if err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM metadata WHERE key=?", r.key).Scan(&done); err != nil || done > 0 {
		return err
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err = conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return err
	}
	defer conn.ExecContext(context.Background(), "ROLLBACK")
	// Recheck under the write lock: two processes can open the store together.
	if err = conn.QueryRowContext(ctx, "SELECT count(*) FROM metadata WHERE key=?", r.key).Scan(&done); err != nil || done > 0 {
		return err
	}
	if _, err = conn.ExecContext(ctx, r.statement); err != nil {
		return err
	}
	if _, err = conn.ExecContext(ctx, "INSERT INTO metadata VALUES (?, '1')", r.key); err != nil {
		return err
	}
	_, err = conn.ExecContext(ctx, "COMMIT")
	return err
}

// Transact runs fn in one write transaction. SQLite never sees the caller's
// context: an interrupted statement can leave the transaction open on a
// connection the pool then discards, and SQLite keeps that connection's write
// lock while it waits to be finalized, so every later write fails busy (#155).
// The caller's cancellation is checked between statements and before COMMIT
// instead, and the transaction is rolled back before the connection goes back.
func (s *Store) Transact(ctx context.Context, g storage.Guard, fn func(storage.Tx) error) error {
	if g.Now.IsZero() || fn == nil {
		return storage.ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	work := context.WithoutCancel(ctx)
	if _, err = conn.ExecContext(work, "BEGIN IMMEDIATE"); err != nil {
		return mapError(err)
	}
	committed := false
	defer func() {
		if !committed {
			rollback(work, conn)
		}
	}()
	t := &transaction{conn: conn, caller: ctx, ctx: work, now: g.Now, live: true}
	defer func() { t.live = false }()
	if g.Lease != nil {
		if err = t.authorize(*g.Lease); err != nil {
			return err
		}
	}
	if err = fn(t); err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	if _, err = conn.ExecContext(work, "COMMIT"); err != nil {
		return mapError(err)
	}
	committed = true
	return nil
}

// rollback ends a transaction that did not commit. If ROLLBACK fails the
// transaction may still hold the write lock, so the connection is closed, which
// releases it, rather than returned to the pool.
func rollback(ctx context.Context, conn *sql.Conn) {
	if _, err := conn.ExecContext(ctx, "ROLLBACK"); err == nil {
		return
	}
	_ = conn.Raw(func(c any) error {
		if closer, ok := c.(io.Closer); ok {
			_ = closer.Close()
		}
		return driver.ErrBadConn
	})
}

type transaction struct {
	conn   *sql.Conn
	caller context.Context
	ctx    context.Context
	now    time.Time
	live   bool
}

// usable reports why no more statements may run in the transaction, if any.
func (t *transaction) usable() error {
	if !t.live {
		return storage.ErrInvalid
	}
	return t.caller.Err()
}

func (t *transaction) exec(query string, args ...any) (sql.Result, error) {
	if err := t.usable(); err != nil {
		return nil, err
	}
	r, err := t.conn.ExecContext(t.ctx, query, args...)
	return r, mapError(err)
}

// queryRow is QueryRowContext on the transaction's connection, once the
// transaction is checked usable.
func (t *transaction) queryRow(query string, args ...any) interface{ Scan(...any) error } {
	if err := t.usable(); err != nil {
		return failedRow{err}
	}
	return t.conn.QueryRowContext(t.ctx, query, args...)
}

type failedRow struct{ err error }

func (r failedRow) Scan(...any) error { return r.err }

func (t *transaction) query(query string, args ...any) (*sql.Rows, error) {
	if err := t.usable(); err != nil {
		return nil, err
	}
	return t.conn.QueryContext(t.ctx, query, args...)
}
func read[T any](t *transaction, query string, args ...any) (T, error) {
	var v T
	var b []byte
	err := t.queryRow(query, args...).Scan(&b)
	if err != nil {
		return v, mapError(err)
	}
	err = json.Unmarshal(b, &v)
	normalize(&v)
	return v, err
}
func list[T any](t *transaction, query string, args ...any) ([]T, error) {
	out := []T{}
	rows, err := t.query(query, args...)
	if err != nil {
		return nil, mapError(err)
	}
	defer rows.Close()
	for rows.Next() {
		var b []byte
		var v T
		if err = rows.Scan(&b); err != nil {
			return nil, err
		}
		if err = json.Unmarshal(b, &v); err != nil {
			return nil, err
		}
		normalize(&v)
		out = append(out, v)
	}
	return out, rows.Err()
}
func encode(v any) ([]byte, error) { return json.Marshal(v) }
func nullable(id domain.ID) any {
	if id == "" {
		return nil
	}
	return string(id)
}
func mapError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return storage.ErrNotFound
	}
	var e *modernsqlite.Error
	if errors.As(err, &e) {
		switch e.Code() & 255 {
		case 19:
			return fmt.Errorf("%w: database constraint", storage.ErrConflict)
		case 5, 6:
			return fmt.Errorf("%w: database busy", storage.ErrConflict)
		}
	}
	return err
}
func namespaced(s string) bool { return namespace(s) }
func payload(p domain.Payload) bool {
	return p.Schema == "" && len(p.Data) == 0 || namespaced(p.Schema) && json.Valid(p.Data)
}
func keyOK(k domain.CorrelationKey) bool { return k.Source != "" && k.ExternalID != "" }
func revision(exists bool, current, expected domain.Revision) error {
	if !exists && expected == 0 || exists && expected != 0 && current == expected {
		if expected >= math.MaxInt64 {
			return storage.ErrInvalid
		}
		return nil
	}
	return storage.ErrConflict
}

// encoding/json decodes a nil RawMessage encoded as JSON null into the literal
// bytes "null". Restore the absent-payload representation at the record boundary;
// present opaque payload JSON remains untouched.
func normalize(value any) {
	clean := func(p *domain.Payload) {
		if p.Schema == "" && string(p.Data) == "null" {
			p.Data = nil
		}
	}
	switch v := value.(type) {
	case *domain.AcceptedEvent:
		clean(&v.Event.Data)
	case *domain.Event:
		clean(&v.Data)
	case *domain.Work:
		clean(&v.Context)
	case *domain.Execution:
		if v.Outcome != nil {
			clean(&v.Outcome.Data)
		}
	case *domain.Timer:
		clean(&v.Event.Data)
	case *domain.Scope:
		clean(&v.Config)
		for i := range v.Bindings {
			clean(&v.Bindings[i].Config)
		}
		for i := range v.Routes {
			clean(&v.Routes[i].Config)
		}
	}
}

func privateFile(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Getuid()) && info.Mode().IsRegular() && info.Mode().Perm()&0077 == 0
}
