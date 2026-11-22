package sqlite

import (
	"context"
	"fmt"

	"github.com/open-conduit/open-conduit/storage"
)

// autoVacuumIncremental is SQLite's auto_vacuum mode that tracks pages so
// free ones can be returned to the file system a few at a time. It can only
// be chosen before a database's first table, or by rewriting it with VACUUM.
const autoVacuumIncremental = 2

var _ storage.Compactor = (*Store)(nil)

// Space implements storage.Compactor.
func (s *Store) Space(ctx context.Context) (storage.Space, error) {
	read := func(pragma string) (int64, error) {
		var v int64
		err := s.db.QueryRowContext(ctx, "PRAGMA "+pragma).Scan(&v)
		return v, err
	}
	var pageSize, pages, free, mode int64
	for _, p := range []struct {
		name string
		to   *int64
	}{{"page_size", &pageSize}, {"page_count", &pages}, {"freelist_count", &free}, {"auto_vacuum", &mode}} {
		var err error
		if *p.to, err = read(p.name); err != nil {
			return storage.Space{}, fmt.Errorf("sqlite: %s: %w", p.name, err)
		}
	}
	return storage.Space{TotalBytes: pages * pageSize, FreeBytes: free * pageSize, Incremental: mode == autoVacuumIncremental}, nil
}

// Reclaim implements storage.Compactor. The file shrinks when the WAL holding
// the move is checkpointed. A reader in the way only delays that to a later
// checkpoint, so a checkpoint that can't finish isn't a failure.
func (s *Store) Reclaim(ctx context.Context, bytes int64) (int64, error) {
	before, err := s.Space(ctx)
	if err != nil {
		return 0, err
	}
	if !before.Incremental || bytes <= 0 || before.FreeBytes == 0 {
		return 0, nil
	}
	var pageSize int64
	if err = s.db.QueryRowContext(ctx, "PRAGMA page_size").Scan(&pageSize); err != nil {
		return 0, err
	}
	pages := min(bytes, before.FreeBytes) / pageSize
	if pages == 0 {
		return 0, nil
	}
	// The pragma does its work a step per row, so it has to be read to its
	// end; a plain Exec could stop after the first page.
	rows, err := s.db.QueryContext(ctx, fmt.Sprintf("PRAGMA incremental_vacuum(%d)", pages))
	if err != nil {
		return 0, fmt.Errorf("sqlite: incremental vacuum: %w", err)
	}
	for rows.Next() {
	}
	err = rows.Err()
	if closeErr := rows.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return 0, fmt.Errorf("sqlite: incremental vacuum: %w", err)
	}
	if err = s.checkpoint(ctx, "PASSIVE"); err != nil {
		return 0, err
	}
	after, err := s.Space(ctx)
	if err != nil {
		return 0, err
	}
	return before.FreeBytes - after.FreeBytes, nil
}

// Rebuild implements storage.Compactor.
func (s *Store) Rebuild(ctx context.Context) error {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err = conn.ExecContext(ctx, fmt.Sprintf("PRAGMA auto_vacuum=%d", autoVacuumIncremental)); err != nil {
		return fmt.Errorf("sqlite: auto_vacuum: %w", err)
	}
	if _, err = conn.ExecContext(ctx, "VACUUM"); err != nil {
		return fmt.Errorf("sqlite: vacuum: %w", err)
	}
	var busy, log, done int
	if err = conn.QueryRowContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)").Scan(&busy, &log, &done); err != nil {
		return fmt.Errorf("sqlite: checkpoint: %w", err)
	}
	return nil
}

func (s *Store) checkpoint(ctx context.Context, mode string) error {
	var busy, log, done int
	if err := s.db.QueryRowContext(ctx, "PRAGMA wal_checkpoint("+mode+")").Scan(&busy, &log, &done); err != nil {
		return fmt.Errorf("sqlite: checkpoint: %w", err)
	}
	return nil
}
