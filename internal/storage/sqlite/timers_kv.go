package sqlite

import (
	"errors"

	"github.com/open-conduit/open-conduit/domain"
	"github.com/open-conduit/open-conduit/storage"
)

func (t *transaction) Timer(id domain.ID) (domain.Timer, error) {
	return read[domain.Timer](t, "SELECT record FROM timers WHERE id=?", id)
}
func (t *transaction) ListTimers() ([]domain.Timer, error) {
	return list[domain.Timer](t, "SELECT record FROM timers ORDER BY id")
}
func (t *transaction) PutTimer(v domain.Timer, expected domain.Revision) error {
	old, err := t.Timer(v.ID)
	exists := err == nil
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		return err
	}
	if err = revision(exists, old.Revision, expected); err != nil {
		return err
	}
	if v.ID == "" || v.FireAt.IsZero() || v.Event.ID == "" || !namespaced(v.Event.Type) || v.Event.Source == "" || v.Event.DeliveryID == "" || !payload(v.Event.Data) {
		return storage.ErrInvalid
	}
	if v.Event.CorrelationKey != nil && !keyOK(*v.Event.CorrelationKey) {
		return storage.ErrInvalid
	}
	if v.Event.CausationID != "" {
		if _, err = t.Event(v.Event.CausationID); err != nil {
			return err
		}
	}
	var n int
	if err = t.conn.QueryRowContext(t.ctx, "SELECT count(*) FROM events WHERE id=? OR (source=? AND delivery_id=?)", v.Event.ID, v.Event.Source, v.Event.DeliveryID).Scan(&n); err != nil {
		return err
	}
	if n != 0 {
		return storage.ErrConflict
	}
	v.Revision = expected + 1
	if !exists {
		var previous domain.Revision
		err = t.conn.QueryRowContext(t.ctx, "SELECT revision FROM timer_versions WHERE id=?", v.ID).Scan(&previous)
		if err != nil && !errors.Is(mapError(err), storage.ErrNotFound) {
			return err
		}
		if previous >= 1<<63-1 {
			return storage.ErrInvalid
		}
		v.Revision = previous + 1
	}
	v.CreatedAt = old.CreatedAt
	if !exists {
		v.CreatedAt = t.now
	}
	v.UpdatedAt = t.now
	b, err := encode(v)
	if err != nil {
		return err
	}
	if _, err = t.exec("INSERT INTO timer_versions(id,revision) VALUES(?,?) ON CONFLICT(id) DO UPDATE SET revision=excluded.revision", v.ID, v.Revision); err != nil {
		return err
	}
	if exists {
		_, err = t.exec("UPDATE timers SET record=? WHERE id=?", b, v.ID)
	} else {
		_, err = t.exec("INSERT INTO timers(id,record) VALUES(?,?)", v.ID, b)
	}
	return err
}
func (t *transaction) DeleteTimer(id domain.ID, expected domain.Revision) error {
	v, err := t.Timer(id)
	if err != nil {
		return err
	}
	if v.Revision != expected {
		return storage.ErrConflict
	}
	_, err = t.exec("DELETE FROM timers WHERE id=?", id)
	return err
}
func (t *transaction) FireTimer(id domain.ID, expected domain.Revision, next *domain.Timer) (err error) {
	v, err := t.Timer(id)
	if err != nil {
		return err
	}
	if v.Revision != expected {
		return storage.ErrConflict
	}
	if t.now.Before(v.FireAt) {
		return storage.ErrInvalid
	}
	if next != nil && (next.ID != id || !next.FireAt.After(v.FireAt) || next.Event.ID == v.Event.ID || next.Event.Source == v.Event.Source && next.Event.DeliveryID == v.Event.DeliveryID) {
		return storage.ErrInvalid
	}
	if _, err = t.exec("SAVEPOINT fire_timer"); err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_, _ = t.exec("ROLLBACK TO fire_timer")
		}
		_, releaseErr := t.exec("RELEASE fire_timer")
		if err == nil {
			err = releaseErr
		}
	}()
	if next != nil {
		if err = t.PutTimer(*next, expected); err != nil {
			return err
		}
	} else {
		if err = t.DeleteTimer(id, expected); err != nil {
			return err
		}
	}
	e := v.Event
	e.OccurredAt = v.FireAt
	e.ReceivedAt = t.now
	_, _, err = t.AcceptEvent(e)
	return err
}
func kvOK(ns, key string) bool { return ns != "" && key != "" }
func (t *transaction) Get(ns, key string) ([]byte, error) {
	if !kvOK(ns, key) {
		return nil, storage.ErrInvalid
	}
	if !t.live {
		return nil, storage.ErrInvalid
	}
	var b []byte
	err := t.conn.QueryRowContext(t.ctx, "SELECT value FROM kv WHERE namespace=? AND key=?", ns, key).Scan(&b)
	return b, mapError(err)
}
func (t *transaction) Put(ns, key string, value []byte) error {
	if !kvOK(ns, key) {
		return storage.ErrInvalid
	}
	if value == nil {
		value = []byte{}
	}
	_, err := t.exec("INSERT INTO kv(namespace,key,value) VALUES(?,?,?) ON CONFLICT(namespace,key) DO UPDATE SET value=excluded.value", ns, key, value)
	return err
}
func (t *transaction) Delete(ns, key string) error {
	if !kvOK(ns, key) {
		return storage.ErrInvalid
	}
	_, err := t.exec("DELETE FROM kv WHERE namespace=? AND key=?", ns, key)
	return err
}
func (t *transaction) List(ns, prefix string) ([]storage.Entry, error) {
	if ns == "" || !t.live {
		return nil, storage.ErrInvalid
	}
	out := []storage.Entry{}
	rows, err := t.conn.QueryContext(t.ctx, "SELECT key,value FROM kv WHERE namespace=? AND substr(CAST(key AS BLOB),1,length(CAST(? AS BLOB)))=CAST(? AS BLOB) ORDER BY key", ns, prefix, prefix)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var e storage.Entry
		if err = rows.Scan(&e.Key, &e.Value); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
