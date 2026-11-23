package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/open-conduit/open-conduit/storage"
)

// instanceTable holds the instance lease in one row. A NULL expiry means no
// instance holds the state; owner and epoch stay as the last holder's, so
// the next acquisition advances the epoch and knows whose attempts it
// inherits. Record format 10 keeps a binary that doesn't check it from
// opening the store and writing past the lease.
const instanceTable = `CREATE TABLE IF NOT EXISTS instance_lease (id INTEGER PRIMARY KEY CHECK(id=1), owner TEXT NOT NULL CHECK(length(owner)>0), epoch INTEGER NOT NULL CHECK(epoch>0), expires_at INTEGER)`

var _ storage.InstanceLeaser = (*Store)(nil)

type instanceRow struct {
	owner   string
	epoch   uint64
	expires sql.NullInt64
}

func (t *transaction) instance() (instanceRow, bool, error) {
	if err := t.usable(); err != nil {
		return instanceRow{}, false, err
	}
	var r instanceRow
	err := t.conn.QueryRowContext(t.ctx, `SELECT owner, epoch, expires_at FROM instance_lease WHERE id=1`).Scan(&r.owner, &r.epoch, &r.expires)
	if errors.Is(err, sql.ErrNoRows) {
		return instanceRow{}, false, nil
	}
	if err != nil {
		return instanceRow{}, false, mapError(err)
	}
	return r, true, nil
}

// authorizeInstance is the check every write transaction makes under the
// write lock: the epoch decides, and no clock is read.
func (t *transaction) authorizeInstance(ref *storage.InstanceRef) error {
	r, ok, err := t.instance()
	if err != nil {
		return err
	}
	held := ok && r.expires.Valid
	if ref == nil {
		if held {
			return storage.ErrInstanceFenced
		}
		return nil
	}
	if !held || r.owner != ref.Owner || r.epoch != ref.Epoch {
		return storage.ErrInstanceFenced
	}
	return nil
}

// lease runs fn in a transaction that manages the lease itself, so it is
// not subject to the check above.
func (s *Store) lease(ctx context.Context, now time.Time, fn func(*transaction) error) error {
	inner := func(tx storage.Tx) error { return fn(tx.(*transaction)) }
	g := storage.Guard{Now: now}
	if rerun {
		return rerunTransact(ctx, g, s.transactUnfenced, inner)
	}
	return s.transactUnfenced(ctx, g, inner)
}

// AcquireInstance implements storage.InstanceLeaser, against this host's
// clock: a SQLite database has one host.
func (s *Store) AcquireInstance(ctx context.Context, owner string, ttl time.Duration) (storage.InstanceRef, error) {
	if owner == "" || ttl <= 0 {
		return storage.InstanceRef{}, storage.ErrInvalid
	}
	var ref storage.InstanceRef
	now := time.Now()
	err := s.lease(ctx, now, func(t *transaction) error {
		r, ok, err := t.instance()
		if err != nil {
			return err
		}
		if ok && r.owner != owner && r.expires.Valid && r.expires.Int64 > now.UnixNano() {
			return storage.ErrInstanceHeld
		}
		if ok && r.owner != owner {
			if err = t.fenceExecutions(now); err != nil {
				return err
			}
		}
		ref = storage.InstanceRef{Owner: owner, Epoch: r.epoch + 1}
		_, err = t.exec(`INSERT INTO instance_lease(id, owner, epoch, expires_at) VALUES (1,?,?,?)
			ON CONFLICT(id) DO UPDATE SET owner=excluded.owner, epoch=excluded.epoch, expires_at=excluded.expires_at`,
			ref.Owner, ref.Epoch, now.Add(ttl).UnixNano())
		return err
	})
	if err != nil {
		return storage.InstanceRef{}, err
	}
	return ref, nil
}

// RenewInstance implements storage.InstanceLeaser.
func (s *Store) RenewInstance(ctx context.Context, ref storage.InstanceRef, ttl time.Duration) error {
	if ref.Owner == "" || ttl <= 0 {
		return storage.ErrInvalid
	}
	now := time.Now()
	return s.lease(ctx, now, func(t *transaction) error {
		if err := t.authorizeInstance(&ref); err != nil {
			return err
		}
		_, err := t.exec(`UPDATE instance_lease SET expires_at=max(expires_at,?) WHERE id=1`, now.Add(ttl).UnixNano())
		return err
	})
}

// ReleaseInstance implements storage.InstanceLeaser.
func (s *Store) ReleaseInstance(ctx context.Context, ref storage.InstanceRef) error {
	if ref.Owner == "" {
		return storage.ErrInvalid
	}
	return s.lease(ctx, time.Now(), func(t *transaction) error {
		r, ok, err := t.instance()
		if err != nil {
			return err
		}
		if !ok || r.owner != ref.Owner || r.epoch != ref.Epoch {
			return storage.ErrInstanceFenced
		}
		if !r.expires.Valid {
			return nil
		}
		_, err = t.exec(`UPDATE instance_lease SET expires_at=NULL WHERE id=1`)
		return err
	})
}

// fenceExecutions clears every Execution lease's owner and expiry, keeping
// epochs.
func (t *transaction) fenceExecutions(now time.Time) error {
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
}
